// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Package hostsfile implements a service which owns and writes all node-local
// files/interfaces used by the system to resolve the local node's name and the
// names of other nodes in the cluster:
//
//  1. All cluster node names are written into /etc/hosts for DNS resolution.
//  2. The local node's name is written into /etc/machine-id.
//  3. The local node's name is set as the UNIX hostname of the machine (via the
//     sethostname call).
//  4. The local node's ClusterDirectory is updated with the same set of
//     addresses as the one used in /etc/hosts.
//
// The hostsfile Service can start up in two modes: with cluster connectivity
// and without cluster connectivity. Without cluster connectivity, only
// information about the current node (as retrieved from the network service)
// will be used to populate local data. In cluster mode, information about other
// nodes is also used.
package hostsfile

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"

	"github.com/miekg/dns"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"source.monogon.dev/metropolis/node"
	"source.monogon.dev/metropolis/node/core/curator/watcher"
	"source.monogon.dev/metropolis/node/core/localstorage"
	"source.monogon.dev/metropolis/node/core/network"
	"source.monogon.dev/osbase/event"
	netDNS "source.monogon.dev/osbase/net/dns"
	"source.monogon.dev/osbase/supervisor"

	ipb "source.monogon.dev/metropolis/node/core/curator/proto/api"
	cpb "source.monogon.dev/metropolis/proto/common"
)

type Config struct {
	// Network will be read to retrieve the current status of the Network service.
	Network *network.Service
	// Ephemeral is the root of the ephemeral storage of the node, into which the
	// service will write its managed files.
	Ephemeral *localstorage.EphemeralDirectory
	// ESP is the root of the node's EFI System Partition.
	ESP *localstorage.ESPDirectory
	// NodeID of the node the service is running on.
	NodeID string
	// Curator gRPC client authenticated as local node.
	Curator ipb.CuratorClient
	// ClusterDirectorySaved will be written with a boolean indicating whether the
	// ClusterDirectory has been successfully persisted to the ESP.
	ClusterDirectorySaved event.Value[bool]
}

// Service is the hostsfile service instance. See package-level documentation
// for more information.
type Service struct {
	Config

	// clusterC is a channel populated by the cluster sub-runnable with the newest
	// available information about the cluster nodes. It is automatically created and
	// closed by Run.
	clusterC chan nodeMap

	// nodes contains the current information about nodes maintained by Run.
	nodes nodeMap

	// mu guards the nodes field.
	mu sync.RWMutex
}

type ClusterDialer func(ctx context.Context) (*grpc.ClientConn, error)

// nodeInfo contains all of a single node's data needed to build its entry in
// either hostsfile or ClusterDirectory.
type nodeInfo struct {
	// address is the node's IP address.
	address net.IP
	// local is true if address belongs to the local node.
	local bool
	// controlPlane is true if this node can be expected to run the control plane
	// (for example, it was running it at time of retrieval from the cluster). This
	// is used to populate the cluster directory ony with control plane nodes.
	controlPlane bool
}

func (n *nodeInfo) equals(o *nodeInfo) bool {
	if !n.address.Equal(o.address) {
		return false
	}
	if n.controlPlane != o.controlPlane {
		return false
	}
	return true
}

// nodeMap is a map from node ID (effectively DNS name) to node IP address.
type nodeMap map[string]nodeInfo

// clusterDirectory builds a ClusterDirectory based on nodeMap contents. If m
// is empty, an empty ClusterDirectory is returned.
func (m nodeMap) clusterDirectory(ctx context.Context) *cpb.ClusterDirectory {
	var directory cpb.ClusterDirectory
	for nid, ni := range m {
		if !ni.controlPlane {
			continue
		}
		supervisor.Logger(ctx).Infof("ClusterDirectory entry: %s", ni.address)
		addresses := []*cpb.ClusterDirectory_Node_Address{
			{Host: ni.address.String()},
		}
		node := &cpb.ClusterDirectory_Node{
			Id:        nid,
			Addresses: addresses,
		}
		directory.Nodes = append(directory.Nodes, node)
	}
	return &directory
}

func (s *Service) Run(ctx context.Context) error {
	// Let other components know whether a cluster directory has already been
	// persisted.
	exists, _ := s.ESP.Metropolis.ClusterDirectory.Exists()
	s.ClusterDirectorySaved.Set(exists)

	nodes := make(nodeMap)
	if exists {
		supervisor.Logger(ctx).Infof("Saved cluster directory present, restoring host data...")
		cd, err := s.ESP.Metropolis.ClusterDirectory.Unmarshal()
		if err != nil {
			supervisor.Logger(ctx).Errorf("Could not unmarshal saved cluster directory: %v", err)
		} else {
			for i, node := range cd.Nodes {
				if len(node.Id) == 0 {
					supervisor.Logger(ctx).Warningf("Node %d in cluster directory has no ID, skipping...", i)
					continue
				}
				if len(node.Addresses) == 0 {
					supervisor.Logger(ctx).Warningf("Node %d (%s) in cluster directory has no addresses, skipping...", i, node.Id)
					continue
				}
				nodes[node.Id] = nodeInfo{
					address:      net.ParseIP(node.Addresses[0].Host),
					local:        false,
					controlPlane: true,
				}
			}
		}
	} else {
		supervisor.Logger(ctx).Infof("Saved cluster directory absent, not restoring any host data.")
	}

	localC := make(chan *node.NetStatus)
	s.clusterC = make(chan nodeMap)

	if err := supervisor.Run(ctx, "local", event.Pipe(&s.Network.Status, localC)); err != nil {
		return err
	}
	if err := supervisor.Run(ctx, "cluster", s.runCluster); err != nil {
		return err
	}

	// Immediately update machine-id and hostname, we don't need network addresses
	// for that.
	if err := s.Ephemeral.MachineID.Write([]byte(s.NodeID), 0644); err != nil {
		return fmt.Errorf("failed to write /ephemeral/machine-id: %w", err)
	}
	if err := unix.Sethostname([]byte(s.NodeID)); err != nil {
		return fmt.Errorf("failed to set runtime hostname: %w", err)
	}

	// Set the nodes field to start serving DNS queries.
	s.mu.Lock()
	s.nodes = nodes
	s.mu.Unlock()

	// Inject the hostsfile service into the DNS service as handler for hosts queries.
	s.Network.DNS.SetHandler("hosts", s)

	supervisor.Signal(ctx, supervisor.SignalHealthy)

	// Keep note of whether we have received data from the cluster, and only persist
	// cluster directory then. Otherwise we risk overriding an already existing
	// cluster directory with an empty one or one just containing this node.
	haveRemoteData := false

	// Update nodeMap in a loop, issuing writes/updates when any change occurred.
	for {
		changed := false
		select {
		case <-ctx.Done():
			return ctx.Err()
		case st := <-localC:
			// Ignore spurious updates.
			if st.ExternalAddress == nil {
				continue
			}
			u := st.ExternalAddress
			if nodes[s.NodeID].address.Equal(u) {
				continue
			}
			supervisor.Logger(ctx).Infof("Got new local address: %s", u)
			s.mu.Lock()
			nodes[s.NodeID] = nodeInfo{
				address: u,
				local:   true,
			}
			s.mu.Unlock()
			changed = true
		case u := <-s.clusterC:
			// Loop through the nodeMap from the cluster subrunnable, making note of what
			// changed. By design we don't care about any nodes disappearing from the
			// nodeMap: we'd rather keep stale data about nodes that don't exist any more,
			// as these might either be spurious or have a long tail of effectively still
			// being used by the local node for communications while the node gets fully
			// drained/disowned.
			//
			// MVP: we should at least log removed nodes.
			for id, info := range u {
				// We're not interested in what the cluster thinks about our local node, as that
				// might be outdated (eg. when we haven't yet reported a new local address to
				// the cluster).

				existing := nodes[id]

				if id == s.NodeID {
					// ... but this is still a good source of information to know whether we're
					// running the control plane or not.
					if existing.controlPlane != info.controlPlane {
						changed = true
						existing.controlPlane = info.controlPlane
						s.mu.Lock()
						nodes[id] = existing
						s.mu.Unlock()
					}
					continue
				}
				if existing.equals(&info) {
					continue
				}
				supervisor.Logger(ctx).Infof("Update for node %s: address %s, control plane %v", id, info.address, info.controlPlane)
				s.mu.Lock()
				nodes[id] = info
				s.mu.Unlock()
				changed = true
			}
			haveRemoteData = true
		}

		if !changed {
			continue
		}

		// Check that we are self-resolvable.
		if _, err := net.ResolveIPAddr("ip", s.NodeID); err != nil {
			supervisor.Logger(ctx).Errorf("Failed to self-resolve %q: %v", s.NodeID, err)
		}

		// Update this node's ClusterDirectory.
		if haveRemoteData {
			supervisor.Logger(ctx).Info("Updating ClusterDirectory.")
			cd := nodes.clusterDirectory(ctx)
			cdirRaw, err := proto.Marshal(cd)
			if err != nil {
				return fmt.Errorf("couldn't marshal ClusterDirectory: %w", err)
			}
			if err = s.ESP.Metropolis.ClusterDirectory.Write(cdirRaw, 0644); err != nil {
				return err
			}
			unix.Sync()
			s.ClusterDirectorySaved.Set(true)
		}
	}
}

// runCluster updates s.clusterC with the IP addresses of cluster nodes, as
// retrieved from a Curator client from the ClusterDialer. The returned map
// reflects the up-to-date view of the cluster returned from the Curator Watch
// call, including any node deletions.
func (s *Service) runCluster(ctx context.Context) error {
	nodes := make(nodeMap)
	return watcher.WatchNodes(ctx, s.Curator, watcher.SimpleFollower{
		FilterFn: func(a *ipb.Node) bool {
			if a.Status == nil || net.ParseIP(a.Status.ExternalAddress) == nil {
				return false
			}
			return true
		},
		EqualsFn: func(a *ipb.Node, b *ipb.Node) bool {
			if a.Status.ExternalAddress != b.Status.ExternalAddress {
				return false
			}
			if (a.Roles.ConsensusMember != nil) != (b.Roles.ConsensusMember != nil) {
				return false
			}
			return true
		},
		OnNewUpdated: func(new *ipb.Node) error {
			nodes[new.Id] = nodeInfo{
				address:      net.ParseIP(new.Status.ExternalAddress),
				local:        false,
				controlPlane: new.Roles.ConsensusMember != nil,
			}
			return nil
		},
		OnDeleted: func(prev *ipb.Node) error {
			delete(nodes, prev.Id)
			return nil
		},
		OnBatchDone: func() error {
			// Send copy over channel, as we need to decouple the gRPC receiver from the main
			// processing loop of the worker.
			nodesCopy := make(nodeMap)
			for k, v := range nodes {
				nodesCopy[k] = v
			}
			s.clusterC <- nodesCopy
			return nil
		},
	})
}

const hostsTtl = 10

var reHostname = regexp.MustCompile(`^metropolis-[0-9a-f]+\.$`)

// HandleDNS serves DNS queries for node IDs.
func (s *Service) HandleDNS(r *netDNS.Request) {
	// Regexp matching is somewhat expensive, so we first do a cheaper
	// string comparison to filter out most names.
	if !strings.HasPrefix(r.QnameCanonical, "metropolis-") ||
		!reHostname.MatchString(r.QnameCanonical) {
		return
	}

	r.SetAuthoritative()

	// Extract node ID by removing the trailing dot.
	nodeID := r.QnameCanonical[:len(r.QnameCanonical)-1]

	s.mu.RLock()
	node, ok := s.nodes[nodeID]
	s.mu.RUnlock()

	if ok {
		if v4 := node.address.To4(); v4 != nil {
			if r.Qtype == dns.TypeA || r.Qtype == dns.TypeANY {
				rr := new(dns.A)
				rr.Hdr = dns.RR_Header{Name: r.Qname, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: hostsTtl}
				rr.A = v4
				r.Reply.Answer = append(r.Reply.Answer, rr)
			}
		} else if len(node.address) == net.IPv6len {
			if r.Qtype == dns.TypeAAAA || r.Qtype == dns.TypeANY {
				rr := new(dns.AAAA)
				rr.Hdr = dns.RR_Header{Name: r.Qname, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: hostsTtl}
				rr.AAAA = node.address
				r.Reply.Answer = append(r.Reply.Answer, rr)
			}
		} else {
			r.AddExtendedError(dns.ExtendedErrorCodeInvalidData, "host IP address is missing or invalid")
			r.Reply.Rcode = dns.RcodeServerFailure
		}
	} else {
		r.Reply.Rcode = dns.RcodeNameError
	}
	r.SendReply()
}
