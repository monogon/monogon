// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package curator

import (
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	tpb "google.golang.org/protobuf/types/known/timestamppb"

	common "source.monogon.dev/metropolis/node"
	"source.monogon.dev/metropolis/node/allocs"
	"source.monogon.dev/metropolis/node/core/consensus"
	ipb "source.monogon.dev/metropolis/node/core/curator/proto/api"
	"source.monogon.dev/metropolis/node/core/identity"
	"source.monogon.dev/metropolis/node/core/rpc"
	cpb "source.monogon.dev/metropolis/proto/common"
	"source.monogon.dev/osbase/event"
	"source.monogon.dev/osbase/event/etcd"
	"source.monogon.dev/osbase/pki"
)

// leaderCurator implements the Curator gRPC API (ipb.Curator) as a curator
// leader.
type leaderCurator struct {
	*leadership
}

// Watch returns a stream of updates concerning some part of the cluster
// managed by the curator.
//
// See metropolis.node.core.curator.proto.api.Curator for more information about
// the RPC semantics.
//
// TODO(q3k): Currently the watch RPCs are individually backed by etcd cluster
// watches (via individual etcd event values), which might be problematic in
// case of a significant amount of parallel Watches being issued to the Curator.
// It might make sense to combine all pending Watch requests into a single watch
// issued to the cluster, with an intermediary caching stage within the curator
// instance. However, that is effectively implementing etcd learner/relay logic,
// which has has to be carefully considered, especially with regards to serving
// stale data.
func (l *leaderCurator) Watch(req *ipb.WatchRequest, srv ipb.Curator_WatchServer) error {
	switch x := req.Kind.(type) {
	case *ipb.WatchRequest_NodeInCluster_:
		return l.watchNodeInCluster(x.NodeInCluster, srv)
	case *ipb.WatchRequest_NodesInCluster_:
		return l.watchNodesInCluster(x.NodesInCluster, srv)
	default:
		return status.Error(codes.Unimplemented, "unsupported watch kind")
	}
}

// watchNodeInCluster implements the Watch API when dealing with a single
// node-in-cluster request. Effectively, it pipes an etcd value watcher into the
// Watch API.
func (l *leaderCurator) watchNodeInCluster(nic *ipb.WatchRequest_NodeInCluster, srv ipb.Curator_WatchServer) error {
	ctx := srv.Context()

	// Constructing arbitrary etcd path: this is okay, as we only have node objects
	// underneath the NodeEtcdPrefix. Worst case an attacker can do is request a node
	// that doesn't exist, and that will just hang . All access is privileged, so
	// there's also no need to filter anything.
	nodePath, err := NodeEtcdPrefix.Key(nic.NodeId)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid node name: %v", err)
	}
	value := etcd.NewValue(l.etcd, nodePath, nodeValueConverter)

	w := value.Watch()
	defer w.Close()

	for {
		nodeKV, err := w.Get(ctx)
		if err != nil {
			if rpcErr, ok := rpcError(err); ok {
				return rpcErr
			}
			rpc.Trace(ctx).Printf("etcd watch failed: %v", err)
			return status.Error(codes.Unavailable, "internal error")
		}

		ev := &ipb.WatchEvent{}
		nodeKV.appendToEvent(ev)
		if err := srv.Send(ev); err != nil {
			return err
		}
	}
}

// watchNodesInCluster implements the Watch API when dealing with a
// all-nodes-in-cluster request. Effectively, it pipes a ranged etcd value
// watcher into the Watch API.
func (l *leaderCurator) watchNodesInCluster(_ *ipb.WatchRequest_NodesInCluster, srv ipb.Curator_WatchServer) error {
	ctx := srv.Context()

	start, end := NodeEtcdPrefix.KeyRange()
	value := etcd.NewValue[*nodeAtID](l.etcd, start, nodeValueConverter, etcd.Range(end))

	w := value.Watch()
	defer w.Close()

	// Perform initial fetch from etcd.
	nodes := make(map[string]*Node)
	for {
		nodeKV, err := w.Get(ctx, event.BacklogOnly[*nodeAtID]())
		if errors.Is(err, event.ErrBacklogDone) {
			break
		}
		if err != nil {
			rpc.Trace(ctx).Printf("etcd watch failed (initial fetch): %v", err)
			return status.Error(codes.Unavailable, "internal error during initial fetch")
		}
		if nodeKV.value != nil {
			nodes[nodeKV.id] = nodeKV.value
		}
	}

	// Initial send, chunked to not go over 2MiB (half of the default gRPC message
	// size limit).
	//
	// TODO(q3k): formalize message limits, set const somewhere.
	we := &ipb.WatchEvent{}
	for _, n := range nodes {
		n.appendToEvent(we)
		if proto.Size(we) > (2 << 20) {
			if err := srv.Send(we); err != nil {
				return err
			}
			we = &ipb.WatchEvent{}
		}
	}
	// Send last update message. This might be empty, but we need to send the
	// LAST_BACKLOGGED marker.
	we.Progress = ipb.WatchEvent_PROGRESS_LAST_BACKLOGGED
	if err := srv.Send(we); err != nil {
		return err
	}

	// Send updates as they arrive from etcd watcher.
	for {
		nodeKV, err := w.Get(ctx)
		if err != nil {
			rpc.Trace(ctx).Printf("etcd watch failed (update): %v", err)
			return status.Errorf(codes.Unavailable, "internal error during update")
		}
		we := &ipb.WatchEvent{}
		nodeKV.appendToEvent(we)
		if err := srv.Send(we); err != nil {
			return err
		}
	}
}

// nodeAtID is a key/pair container for a node update received from an etcd
// watcher. The value will be nil if this update represents a node being
// deleted.
type nodeAtID struct {
	id    string
	value *Node
}

// nodeValueConverter is called by etcd node value watchers to convert updates
// from the cluster into nodeAtID, ensuring data integrity and checking
// invariants.
func nodeValueConverter(key, value []byte) (*nodeAtID, error) {
	res := nodeAtID{
		id: NodeEtcdPrefix.ExtractID(string(key)),
	}
	if len(value) > 0 {
		node, err := nodeUnmarshal(&mvccpb.KeyValue{Key: key, Value: value})
		if err != nil {
			return nil, err
		}
		res.value = node
		if res.id != res.value.ID() {
			return nil, fmt.Errorf("node ID mismatch (etcd key: %q, value: %q)", res.id, res.value.ID())
		}
	}
	if res.id == "" {
		// This shouldn't happen, to the point where this might be better handled by a
		// panic.
		return nil, fmt.Errorf("invalid node key %q", key)
	}
	return &res, nil
}

// appendToId records a node state represented by Node into a Curator
// WatchEvent.
func (n *Node) appendToEvent(ev *ipb.WatchEvent) {
	np := n.proto()
	ev.Nodes = append(ev.Nodes, &ipb.Node{
		Id:         n.ID(),
		Roles:      np.Roles,
		Status:     np.Status,
		Clusternet: np.Clusternet,
		State:      np.FsmState,
		Labels:     np.Labels,
	})
}

// appendToId records a node update represented by nodeAtID into a Curator
// WatchEvent, either a Node or NodeTombstone.
func (kv nodeAtID) appendToEvent(ev *ipb.WatchEvent) {
	if node := kv.value; node != nil {
		node.appendToEvent(ev)
	} else {
		ev.NodeTombstones = append(ev.NodeTombstones, &ipb.WatchEvent_NodeTombstone{
			NodeId: kv.id,
		})
	}
}

// UpdateNodeStatus is called by nodes in the cluster to report their own
// status. This status is recorded by the curator and can be retrieed via
// Watch.
func (l *leaderCurator) UpdateNodeStatus(ctx context.Context, req *ipb.UpdateNodeStatusRequest) (*ipb.UpdateNodeStatusResponse, error) {
	// Ensure that the given node_id matches the calling node. We currently
	// only allow for direct self-reporting of status by nodes.
	pi := rpc.GetPeerInfo(ctx)
	if pi == nil || pi.Node == nil {
		return nil, status.Error(codes.PermissionDenied, "only nodes can update node status")
	}
	id := pi.Node.ID
	if id != req.NodeId {
		return nil, status.Errorf(codes.PermissionDenied, "node %q cannot update the status of node %q", id, req.NodeId)
	}

	// Verify sent status. Currently we assume the entire status must be set at
	// once, and cannot be unset.
	if req.Status == nil || req.Status.ExternalAddress == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Status and Status.ExternalAddress must be set")
	}

	if net.ParseIP(req.Status.ExternalAddress) == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Status.ExternalAddress must be a valid IP address")
	}

	// As we're performing a node update with two etcd transactions below (one
	// to retrieve, one to save and upate node), take a local lock to ensure
	// that we don't have a race between either two UpdateNodeStatus calls or
	// an UpdateNodeStatus call and some other mutation to the node store.
	l.muNodes.Lock()
	defer l.muNodes.Unlock()

	// Retrieve node ...
	node, err := nodeLoad(ctx, l.leadership, id)
	if err != nil {
		return nil, err
	}
	// ... update its' status ...
	node.status = req.Status
	node.status.Timestamp = tpb.Now()
	// ... and save it to etcd.
	if err := nodeSave(ctx, l.leadership, node); err != nil {
		return nil, err
	}

	return &ipb.UpdateNodeStatusResponse{}, nil
}

func (l *leaderCurator) Heartbeat(stream ipb.Curator_HeartbeatServer) error {
	// Ensure that the given node_id matches the calling node. We currently
	// only allow for direct self-reporting of status by nodes.
	ctx := stream.Context()
	pi := rpc.GetPeerInfo(ctx)
	if pi == nil || pi.Node == nil {
		return status.Error(codes.PermissionDenied, "only nodes can send heartbeats")
	}
	id := pi.Node.ID

	for {
		_, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		// Update the node's timestamp within the local Curator state.
		l.ls.heartbeatTimestamps.Store(id, time.Now())

		rsp := &ipb.HeartbeatResponse{}
		if err := stream.Send(rsp); err != nil {
			return err
		}
	}
}

func (l *leaderCurator) RegisterNode(ctx context.Context, req *ipb.RegisterNodeRequest) (*ipb.RegisterNodeResponse, error) {
	// Call is unauthenticated - verify the other side has connected with an
	// ephemeral certificate. That certificate's pubkey will become the node's
	// pubkey.
	pi := rpc.GetPeerInfo(ctx)
	if pi == nil || pi.Unauthenticated == nil || pi.Unauthenticated.SelfSignedPublicKey == nil {
		return nil, status.Error(codes.Unauthenticated, "connection must be established with a self-signed ephemeral certificate")
	}
	pubkey := pi.Unauthenticated.SelfSignedPublicKey

	// Check the Join Key size.
	if want, got := ed25519.PublicKeySize, len(req.JoinKey); want != got {
		return nil, status.Errorf(codes.InvalidArgument, "join_key must be set and be %d bytes long", want)
	}

	// Verify that call contains a RegisterTicket and that this RegisterTicket is
	// valid.
	wantTicket, err := l.ensureRegisterTicket(ctx)
	if err != nil {
		rpc.Trace(ctx).Printf("could not ensure register ticket: %v", err)
		return nil, status.Error(codes.Unavailable, "could not retrieve register ticket")
	}
	gotTicket := req.RegisterTicket
	if subtle.ConstantTimeCompare(wantTicket, gotTicket) != 1 {
		return nil, status.Error(codes.PermissionDenied, "registerticket invalid")
	}

	// Doing a read-then-write operation below, take lock.
	//
	// MVP: This can lock up the cluster if too many RegisterNode calls get issued,
	// we should either ratelimit these or remove the need to lock.
	l.muNodes.Lock()
	defer l.muNodes.Unlock()

	cl, err := clusterLoad(ctx, l.leadership)
	if err != nil {
		return nil, err
	}
	nodeStorageSecurity, err := cl.NodeStorageSecurity()
	if err != nil {
		rpc.Trace(ctx).Printf("NodeStorageSecurity: %v", err)
		return nil, status.Error(codes.InvalidArgument, "cannot generate recommended node storage security")
	}

	// Figure out if node should be using TPM.
	tpmUsage, err := cl.NodeTPMUsage(req.HaveLocalTpm)
	if err != nil {
		return nil, status.Errorf(codes.PermissionDenied, "%s", err)
	}

	// Check if there already is a node with this pubkey in the cluster.
	id := identity.NodeID(pubkey)
	node, err := nodeLoad(ctx, l.leadership, id)
	if err == nil {
		// If the existing node is in the NEW state already, there's nothing to do,
		// return no error. This can happen in case of spurious retries from the calling
		// node.
		if node.state == cpb.NodeState_NODE_STATE_NEW {
			return &ipb.RegisterNodeResponse{}, nil
		}
		// We can return a bit more information to the calling node here, as if it's in
		// possession of the private key corresponding to an existing node in the
		// cluster, it should have access to the status of the node without danger of
		// leaking data about other nodes.
		//
		rpc.Trace(ctx).Printf("node %s already exists in cluster, failing", id)
		return nil, status.Errorf(codes.FailedPrecondition, "node already exists in cluster, state %s", node.state.String())
	}
	if !errors.Is(err, errNodeNotFound) {
		return nil, err
	}

	// Populate node labels if applicable.
	labels := make(map[string]string)
	if l := req.Labels; l != nil {
		if nlabels := len(l.Pairs); nlabels > common.MaxLabelsPerNode {
			rpc.Trace(ctx).Printf("Too many labels (%d, limit %d), truncating...", nlabels, common.MaxLabelsPerNode)
			l.Pairs = l.Pairs[:common.MaxLabelsPerNode]
		}
		for _, pair := range l.Pairs {
			k := pair.Key
			v := pair.Value

			if err := common.ValidateLabelKey(k); err != nil {
				rpc.Trace(ctx).Printf("Label key %q is invalid: %v, skipping", k, err)
				continue
			}
			if err := common.ValidateLabelValue(v); err != nil {
				rpc.Trace(ctx).Printf("Label value %q (key %q) is invalid: %v, skipping", v, k, err)
				continue
			}
			if _, ok := labels[k]; ok {
				rpc.Trace(ctx).Printf("Label key %q is duplicate, skipping", k)
				continue
			}
			labels[k] = v
		}
	}

	// No node exists, create one.
	node = &Node{
		id:       id,
		pubkey:   pubkey,
		jkey:     req.JoinKey,
		state:    cpb.NodeState_NODE_STATE_NEW,
		tpmUsage: tpmUsage,
		labels:   labels,
	}
	if err := nodeSave(ctx, l.leadership, node); err != nil {
		return nil, err
	}

	// Eat error, as we just deserialized this from a proto.
	clusterConfig, _ := cl.proto()
	return &ipb.RegisterNodeResponse{
		ClusterConfiguration:           clusterConfig,
		TpmUsage:                       tpmUsage,
		RecommendedNodeStorageSecurity: nodeStorageSecurity,
	}, nil
}

func (l *leaderCurator) CommitNode(ctx context.Context, req *ipb.CommitNodeRequest) (*ipb.CommitNodeResponse, error) {
	// Call is unauthenticated - verify the other side has connected with an
	// ephemeral certificate. That certificate's pubkey will become the node's
	// pubkey.
	pi := rpc.GetPeerInfo(ctx)
	if pi == nil || pi.Unauthenticated == nil || pi.Unauthenticated.SelfSignedPublicKey == nil {
		return nil, status.Error(codes.Unauthenticated, "connection must be established with a self-signed ephemeral certificate")
	}
	pubkey := pi.Unauthenticated.SelfSignedPublicKey

	// First pass check of node storage security, before loading the cluster data and
	// taking a lock on it.
	switch req.StorageSecurity {
	case cpb.NodeStorageSecurity_NODE_STORAGE_SECURITY_INSECURE:
	case cpb.NodeStorageSecurity_NODE_STORAGE_SECURITY_ENCRYPTED:
	case cpb.NodeStorageSecurity_NODE_STORAGE_SECURITY_AUTHENTICATED_ENCRYPTED:
	default:
		return nil, status.Error(codes.InvalidArgument, "invalid storage_security (is it set?)")
	}

	// Doing a read-then-write operation below, take lock.
	//
	// MVP: This can lock up the cluster if too many RegisterNode calls get issued,
	// we should either ratelimit these or remove the need to lock.
	l.muNodes.Lock()
	defer l.muNodes.Unlock()

	cl, err := clusterLoad(ctx, l.leadership)
	if err != nil {
		return nil, err
	}
	if err := cl.ValidateNodeStorage(req.StorageSecurity); err != nil {
		return nil, err
	}

	// Retrieve the node and act on its current state, either returning early or
	// mutating it and continuing with the rest of the Commit logic.
	id := identity.NodeID(pubkey)
	node, err := nodeLoad(ctx, l.leadership, id)
	if err != nil {
		return nil, err
	}

	switch node.state {
	case cpb.NodeState_NODE_STATE_NEW:
		return nil, status.Error(codes.PermissionDenied, "node is NEW, wait for attestation/approval")
	case cpb.NodeState_NODE_STATE_DECOMMISSIONED:
		// This node has been since decommissioned by the cluster for some reason, the
		// register flow should be aborted.
		return nil, status.Error(codes.FailedPrecondition, "node is DECOMMISSIONED, abort register flow")
	case cpb.NodeState_NODE_STATE_UP:
		// This can happen due to a network failure when we already handled a
		// CommitNode, but we weren't able to respond to the user. CommitNode is
		// non-idempotent, so just abort, the node should retry from scratch and this
		// node should be manually disowned/deleted by system owners.
		return nil, status.Error(codes.FailedPrecondition, "node is already UP, abort register flow")
	case cpb.NodeState_NODE_STATE_STANDBY:
		// This is what we want.
	default:
		return nil, status.Errorf(codes.Internal, "node is in unknown state: %v", node.state)
	}

	// Check the given CUK is valid.
	// TODO(q3k): unify length with localstorage/crypt keySize.
	if req.StorageSecurity != cpb.NodeStorageSecurity_NODE_STORAGE_SECURITY_INSECURE {
		if want, got := 32, len(req.ClusterUnlockKey); want != got {
			return nil, status.Errorf(codes.InvalidArgument, "invalid ClusterUnlockKey length, wanted %d bytes, got %d", want, got)
		}
	}

	// Generate certificate for node, save new node state, return.

	// If this fails we are safe to let the client retry, as the PKI code is
	// idempotent.
	caCertBytes, err := pkiCA.Ensure(ctx, l.etcd)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "could not get CA certificate: %v", err)
	}
	nodeCert := &pki.Certificate{
		Namespace: &pkiNamespace,
		Issuer:    pkiCA,
		Template:  identity.NodeCertificate(node.ID()),
		Mode:      pki.CertificateExternal,
		PublicKey: node.pubkey,
		Name:      fmt.Sprintf("node-%s", node.ID()),
	}
	nodeCertBytes, err := nodeCert.Ensure(ctx, l.etcd)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "could not emit node credentials: %v", err)
	}

	node.state = cpb.NodeState_NODE_STATE_UP
	node.clusterUnlockKey = req.ClusterUnlockKey
	if err := nodeSave(ctx, l.leadership, node); err != nil {
		return nil, err
	}

	// From this point on, any failure (in the server, or in the network, ...) dooms
	// the node from making progress in registering, as Commit is non-idempotent.

	return &ipb.CommitNodeResponse{
		CaCertificate:   caCertBytes,
		NodeCertificate: nodeCertBytes,
	}, nil
}

func (l *leaderCurator) JoinNode(ctx context.Context, req *ipb.JoinNodeRequest) (*ipb.JoinNodeResponse, error) {
	// Gather peer information.
	pi := rpc.GetPeerInfo(ctx)
	if pi == nil || pi.Unauthenticated == nil || pi.Unauthenticated.SelfSignedPublicKey == nil {
		return nil, status.Error(codes.PermissionDenied, "connection must be established with a self-signed ephemeral certificate")
	}
	// The node will attempt to connect using its Join Key. jkey will contain
	// its public part.
	jkey := pi.Unauthenticated.SelfSignedPublicKey

	// Take the lock to prevent data races during the next step.
	l.muNodes.Lock()
	defer l.muNodes.Unlock()

	// Resolve the Node ID using Join Key, then use the ID to load node
	// information from etcd.
	id, err := nodeIdByJoinKey(ctx, l.leadership, jkey)
	if err != nil {
		return nil, err
	}
	node, err := nodeLoad(ctx, l.leadership, id)
	if err != nil {
		return nil, err
	}

	cl, err := clusterLoad(ctx, l.leadership)
	if err != nil {
		return nil, err
	}

	switch cl.TPMMode {
	case cpb.ClusterConfiguration_TPM_MODE_REQUIRED:
		if !req.UsingSealedConfiguration {
			return nil, status.Errorf(codes.PermissionDenied, "cannot join this cluster with an unsealed configuration")
		}
	case cpb.ClusterConfiguration_TPM_MODE_DISABLED:
		if req.UsingSealedConfiguration {
			return nil, status.Errorf(codes.PermissionDenied, "cannot join this cluster with a sealed configuration")
		}
	}

	if node.tpmUsage == cpb.NodeTPMUsage_NODE_TPM_USAGE_PRESENT_AND_USED && !req.UsingSealedConfiguration {
		return nil, status.Errorf(codes.PermissionDenied, "node registered with TPM, cannot join without one")
	}
	if node.tpmUsage != cpb.NodeTPMUsage_NODE_TPM_USAGE_PRESENT_AND_USED && req.UsingSealedConfiguration {
		return nil, status.Errorf(codes.PermissionDenied, "node registered without TPM, cannot join with one")
	}

	// Don't progress further unless the node is already UP.
	if node.state != cpb.NodeState_NODE_STATE_UP {
		return nil, status.Errorf(codes.FailedPrecondition, "node isn't UP, cannot join")
	}

	// Return the Node's CUK, completing the Join Flow.
	return &ipb.JoinNodeResponse{
		ClusterUnlockKey: node.clusterUnlockKey,
	}, nil
}

func (l *leaderCurator) GetCurrentLeader(_ *ipb.GetCurrentLeaderRequest, srv ipb.CuratorLocal_GetCurrentLeaderServer) error {
	ctx := srv.Context()

	// We're the leader.
	node, err := nodeLoad(ctx, l.leadership, l.leaderID)
	if err != nil {
		rpc.Trace(ctx).Printf("nodeLoad(%q) failed: %v", l.leaderID, err)
		return status.Errorf(codes.Unavailable, "failed to load leader node")
	}
	host := ""
	if node.status != nil && node.status.ExternalAddress != "" {
		host = node.status.ExternalAddress
	}

	err = srv.Send(&ipb.GetCurrentLeaderResponse{
		LeaderNodeId: l.leaderID,
		LeaderHost:   host,
		LeaderPort:   int32(allocs.PortCuratorService),
		ThisNodeId:   l.leaderID,
	})
	if err != nil {
		return err
	}

	<-ctx.Done()
	rpc.Trace(ctx).Printf("Interrupting due to context cancellation")
	return nil
}

func (l *leaderCurator) GetConsensusStatus(ctx context.Context, _ *ipb.GetConsensusStatusRequest) (*ipb.GetConsensusStatusResponse, error) {
	var res ipb.GetConsensusStatusResponse
	members, err := l.consensusStatus.ClusterClient().MemberList(ctx)
	if err != nil {
		rpc.Trace(ctx).Printf("Could not get etcd members: %v", err)
		return nil, status.Errorf(codes.Internal, "could not get etcd members")
	}
	for _, member := range members.Members {
		st := ipb.GetConsensusStatusResponse_EtcdMember{
			Id:     consensus.GetEtcdMemberNodeId(member),
			Status: ipb.GetConsensusStatusResponse_EtcdMember_STATUS_FULL,
		}
		if member.IsLearner {
			st.Status = ipb.GetConsensusStatusResponse_EtcdMember_STATUS_LEARNER
		}
		res.EtcdMember = append(res.EtcdMember, &st)
	}

	return &res, nil
}
