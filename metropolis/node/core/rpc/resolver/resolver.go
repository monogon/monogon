// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package resolver

import (
	"context"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"source.monogon.dev/go/logging"
	"source.monogon.dev/metropolis/node/allocs"
	"source.monogon.dev/metropolis/node/core/curator/watcher"

	apb "source.monogon.dev/metropolis/node/core/curator/proto/api"
	cpb "source.monogon.dev/metropolis/proto/common"
)

const (
	// MetropolisControlAddress is the address of the current Metropolis leader as
	// accepted by the Resolver. Dialing a gRPC channel to this address while the
	// Resolver is used will open the channel to the current leader of the
	// Metropolis control plane.
	MetropolisControlAddress = "metropolis:///control"
)

// Resolver is a gRPC resolver Builder that can be passed to
// grpc.WithResolvers() when dialing a gRPC endpoint.
//
// It's responsible for resolving the magic MetropolisControlAddress
// (metropolis:///control) into an address of the node that is currently the
// leader of the cluster's control plane.
//
// To function, the ClusterResolver needs to be provided with at least one
// control plane node address. It will use these addresses to retrieve the
// address of the node which is the current leader of the control plane.
//
// Then, having established communication with the leader, it will continuously
// update an internal set of control plane node endpoints (the curator map) that
// will be contacted in the future about the state of the leadership when the
// current leader fails over.
//
// The resolver will wait for a first gRPC connection established through it to
// extract the transport credentials used, then use these credentials to call
// the Curator and CuratorLocal services on control plane nodes to perform its
// logic.
//
// This resolver is designed to be used as a long-running object which multiple
// gRPC client connections can use. Usually one ClusterResolver instance should
// be used per application.
//
//	.------------------------.        .--------------------------------------.
//	| Metropolis Cluster     |        | Resolver                             |
//	:------------------------:        :--------------------------------------:
//	:                        :        :                                      :
//	: .--------------------. :        :   .----------------.                 :
//	: | curator (follower) |<---.---------| Leader Updater |------------.    :
//	: '--------------------' :  |     :   '----------------'            |    :
//	: .--------------------. :  |     :   .------------------------.    |    :
//	: | curator (follower) |<---:     :   | Processor (CuratorMap) |<-.-'-.  :
//	: '--------------------' :  |     :   '------------------------'  |   |  :
//	: .--------------------.<---'     :   .-----------------.         |   |  :
//	: | curator (leader)   |<-------------| Curator Updater |---------'   |  :
//	: '--------------------' :        :   '-----------------'             |  :
//	:                        :        :                                   |  :
//	'------------------------'        :   .----------.                    |  :
//	                                  :   | Watchers |-.                  |  :
//	                                  :   '----------' |------------------'  :
//	                                  :     '-^--------'                     :
//	                                  :       |  ^                           :
//	                                  :       |  |                           :
//	                                       .---------------.
//	                                       | gRPC channels |
//	                                       '---------------'
type Resolver struct {
	reqC chan *request
	ctx  context.Context

	// logger, if set, will be called with fmt.Sprintf-like arguments containing
	// debug logs from the running ClusterResolver, subordinate watchers and
	// updaters.
	logger logging.Leveled

	// noCuratorUpdater makes the resolver not run a curator updater. This is used
	// in one-shot resolvers which are given an ahead-of-time list of curators to
	// attempt to contact, eg. joining and registering nodes.
	noCuratorUpdater bool
}

// New starts a new Resolver, ready to be used as a gRPC via WithResolvers.
// However, it needs to be populated with at least one endpoint first (via
// AddEndpoint).
func New(ctx context.Context, opts ...ResolverOption) *Resolver {
	r := &Resolver{
		reqC:   make(chan *request),
		ctx:    ctx,
		logger: logging.NewFunctionBackend(func(severity logging.Severity, msg string) {}),
	}
	for _, opt := range opts {
		opt(r)
	}
	go r.run(ctx)
	return r
}

// ResolverOption are passed to a Resolver being created.
type ResolverOption func(r *Resolver)

// WithLogger sets the logger that the resolver will use. If not configured, the
// resolver will silently block on errors!
func WithLogger(logger logging.Leveled) ResolverOption {
	return func(r *Resolver) {
		r.logger = logger
	}
}

// WithoutCuratorUpdater configures the Resolver to not attmept to update
// curators from the cluster. This is useful in one-shot resolvers, eg.
// unauthenticated ones.
func WithoutCuratorUpdater() ResolverOption {
	return func(r *Resolver) {
		r.noCuratorUpdater = true
	}
}

// NodeEndpoint is the gRPC endpoint (host+port) of a Metropolis control plane
// node.
type NodeEndpoint struct {
	endpoint string
}

// NodeWithDefaultPort returns a NodeEndpoint referencing the default control
// plane port (the Curator port) of a node resolved by its ID over DNS. This is
// the easiest way to construct a NodeEndpoint provided DNS is fully set up.
func NodeWithDefaultPort(id string) (*NodeEndpoint, error) {
	if m, _ := regexp.MatchString(`metropolis-[a-f0-9]+`, id); !m {
		return nil, fmt.Errorf("invalid node ID")
	}
	return NodeByHostPort(id, uint16(allocs.PortCuratorService)), nil
}

// NodeAtAddressWithDefaultPort returns a NodeEndpoint referencing the default
// control plane port (the Curator port) of a node at a given address.
func NodeAtAddressWithDefaultPort(host string) *NodeEndpoint {
	return NodeByHostPort(host, uint16(allocs.PortCuratorService))
}

// NodeByHostPort returns a NodeEndpoint for a fully specified host + port pair.
// The host can either be a hostname or an IP address.
func NodeByHostPort(host string, port uint16) *NodeEndpoint {
	return &NodeEndpoint{
		endpoint: net.JoinHostPort(host, fmt.Sprintf("%d", port)),
	}
}

// nodeAtListener is used in tests to connect to the address of a given listener.
func nodeAtListener(lis net.Listener) *NodeEndpoint {
	return &NodeEndpoint{
		endpoint: lis.Addr().String(),
	}
}

// AddEndpoint tells the resolver that it should attempt to reach the cluster
// through a node available at the given NodeEndpoint.
//
// The resolver will make use of this during the next leadership find routine,
// but this node might then get overridden when the resolver retrieves the
// newest set of Curators from the acquired leader.
func (r *Resolver) AddEndpoint(endpoint *NodeEndpoint) {
	select {
	case <-r.ctx.Done():
		return
	case r.reqC <- &request{
		sa: &requestSeedAdd{
			endpoint: endpoint,
		},
	}:
	}
}

// AddOverride adds a long-lived override which forces the resolver to assume
// that a given node (by ID) is available at the given endpoint, instead of at
// whatever endpoint is reported by the cluster. This should be used sparingly
// outside the cluster, and is mostly designed so that nodes which connect to
// themselves can do so over the loopback address instead of their (possibly
// changing) external address.
func (r *Resolver) AddOverride(id string, ep *NodeEndpoint) {
	select {
	case <-r.ctx.Done():
		return
	case r.reqC <- &request{
		oa: &requestOverrideAdd{
			nodeID:   id,
			endpoint: ep,
		},
	}:
	}
}

// runCuratorUpdater runs the curator updater, noted in logs as CURUPDATE. It
// uses the resolver itself to contact the current leader, retrieve all nodes
// which are running a curator, and populate the processor's curator list in the
// curatorMap. That curatorMap will then be used by the leader updater to find
// the current leader.
func (r *Resolver) runCuratorUpdater(ctx context.Context, opts []grpc.DialOption) error {
	bo := backoff.NewExponentialBackOff()
	bo.MaxElapsedTime = 0
	bo.MaxInterval = 10 * time.Second

	// Use a keepalive to make sure we time out fast if the node we're connecting to
	// partitions.
	opts = append([]grpc.DialOption(nil), opts...)
	opts = append(opts, grpc.WithResolvers(r), grpc.WithKeepaliveParams(keepalive.ClientParameters{
		Time:    10 * time.Second,
		Timeout: 5 * time.Second,
	}))
	cl, err := grpc.NewClient(MetropolisControlAddress, opts...)
	if err != nil {
		// This generally shouldn't happen.
		return fmt.Errorf("could not create gRPC client: %w", err)
	}
	defer cl.Close()

	cur := apb.NewCuratorClient(cl)

	return backoff.RetryNotify(func() error {
		// Map from node ID to status.
		nodes := make(map[string]*cpb.NodeStatus)

		return watcher.WatchNodes(ctx, cur, &watcher.SimpleFollower{
			FilterFn: func(a *apb.Node) bool {
				if a.Status == nil {
					return false
				}
				if a.Status.ExternalAddress == "" {
					return false
				}
				if a.Status.RunningCurator == nil {
					return false
				}
				return true
			},
			EqualsFn: func(a *apb.Node, b *apb.Node) bool {
				if a.Status.ExternalAddress != b.Status.ExternalAddress {
					return false
				}
				if (a.Status.RunningCurator == nil) != (b.Status.RunningCurator == nil) {
					return false
				}
				return true
			},
			OnNewUpdated: func(new *apb.Node) error {
				nodes[new.Id] = new.Status
				return nil
			},
			OnDeleted: func(prev *apb.Node) error {
				delete(nodes, prev.Id)
				return nil
			},
			OnBatchDone: func() error {
				nodesClone := make(map[string]*cpb.NodeStatus)
				for k, v := range nodes {
					nodesClone[k] = v
				}
				select {
				case r.reqC <- &request{nu: &requestNodesUpdate{nodes: nodesClone}}:
				case <-ctx.Done():
					return ctx.Err()
				}
				return nil
			},
		})
	}, backoff.WithContext(bo, ctx), func(err error, t time.Duration) {
		c := make(chan *responseDebug)
		r.reqC <- &request{dbg: &requestDebug{resC: c}}
		dbg := <-c
		var msg []string
		for k, v := range dbg.curmap.curators {
			msg = append(msg, fmt.Sprintf("curator: %s/%s", k, v.endpoint))
		}
		for k := range dbg.curmap.seeds {
			msg = append(msg, fmt.Sprintf("seed: %s", k))
		}
		if dbg.leader != nil {
			msg = append(msg, fmt.Sprintf("leader: %s/%s", dbg.leader.nodeID, dbg.leader.endpoint.endpoint))
		}

		r.logger.Errorf("CURUPDATE: error in loop: %v, retrying in %s...", err, t.String())
		r.logger.Infof("CURUPDATE: processor state: %s", strings.Join(msg, ", "))
	})
}

// runLeaderUpdater runs the leader updater, noted in logs as FINDLEADER and
// WATCHLEADER. It uses the curator map from the resolver processor to find the
// current leader.
func (r *Resolver) runLeaderUpdater(ctx context.Context, opts []grpc.DialOption) error {
	bo := backoff.NewExponentialBackOff()
	bo.MaxElapsedTime = 0
	bo.MaxInterval = 10 * time.Second

	err := backoff.RetryNotify(func() error {
		curMap := r.curatorMap()
		for _, endpoint := range curMap.candidates() {
			r.logger.Infof("FINDLEADER: trying via %s...", endpoint)
			ok := r.watchLeaderVia(ctx, endpoint, opts)
			if ok {
				bo.Reset()
			}
		}
		return fmt.Errorf("out of endpoints")
	}, backoff.WithContext(bo, ctx), func(err error, t time.Duration) {
		r.logger.Errorf("FINDLEADER: error in loop: %v, retrying in %s...", err, t.String())
	})
	r.logger.Infof("FINDLEADER: exiting: %v", err)
	return err
}

// watchLeaderVia connects to the endpoint defined by 'via' and attempts to
// continuously update the current leader (b.leader) based on data returned from
// it. Whenever new information about a leader is available, b.condLeader is
// updated.
//
// A boolean value is returned indicating whether the update was at all
// successful. This is used by retry logic to figure out whether to wait before
// retrying or not.
func (r *Resolver) watchLeaderVia(ctx context.Context, via string, opts []grpc.DialOption) bool {
	// Use a keepalive to make sure we time out fast if the node we're connecting to
	// partitions. This is particularly critical for the leader updater, as we want
	// to know as early as possible that this happened, so that we can move over to
	// another node.
	opts = append([]grpc.DialOption(nil), opts...)
	opts = append(opts, grpc.WithKeepaliveParams(keepalive.ClientParameters{
		Time:                10 * time.Second,
		Timeout:             5 * time.Second,
		PermitWithoutStream: true,
	}))
	cl, err := grpc.NewClient(via, opts...)
	if err != nil {
		r.logger.Infof("WATCHLEADER: creating client %s failed: %v", via, err)
		return false
	}
	defer cl.Close()
	cpl := apb.NewCuratorLocalClient(cl)

	cur, err := cpl.GetCurrentLeader(ctx, &apb.GetCurrentLeaderRequest{})
	if err != nil {
		r.logger.Warningf("WATCHLEADER: failed to retrieve current leader from %s: %v", via, err)
		return false
	}
	ok := false
	for {
		r.logger.Infof("WATCHLEADER: receiving...")
		leaderInfo, err := cur.Recv()
		if err == io.EOF {
			r.logger.Infof("WATCHLEADER: connection with %s closed", via)
			return ok
		}
		if err != nil {
			r.logger.Infof("WATCHLEADER: connection with %s failed: %v", via, err)
			return ok
		}
		r.logger.Infof("WATCHLEADER: received: %+v", leaderInfo)

		curMap := r.curatorMap()

		viaID := leaderInfo.ThisNodeId
		if viaID == "" {
			// This shouldn't happen, but let's handle this just in case
			viaID = fmt.Sprintf("UNKNOWN NODE ID (%s)", via)
		}

		if leaderInfo.LeaderNodeId == "" {
			r.logger.Warningf("WATCHLEADER: %s does not know the leader, trying next", viaID)
			return false
		}
		endpoint := ""
		if leaderInfo.LeaderHost == "" {
			// This node knows the leader, but doesn't know its host. Perhaps we have an
			// override for this?
			if ep, ok := curMap.overrides[leaderInfo.LeaderNodeId]; ok {
				endpoint = ep.endpoint
			}
		} else {
			if leaderInfo.LeaderPort == 0 {
				r.logger.Warningf("WATCHLEADER: %s knows the leader's host (%s), but not its' port", viaID, leaderInfo.LeaderHost)
				return false
			}
			endpoint = net.JoinHostPort(leaderInfo.LeaderHost, fmt.Sprintf("%d", leaderInfo.LeaderPort))
		}

		r.logger.Infof("WATCHLEADER: got new leader: %s (%s) via %s", leaderInfo.LeaderNodeId, endpoint, viaID)

		select {
		case <-ctx.Done():
			return ok
		case r.reqC <- &request{lu: &requestLeaderUpdate{
			nodeID:   leaderInfo.LeaderNodeId,
			endpoint: &NodeEndpoint{endpoint: endpoint},
		}}:
		}

		ok = true
	}
}
