// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package curator

import (
	"context"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"source.monogon.dev/metropolis/node/allocs"
	"source.monogon.dev/metropolis/node/core/consensus"
	"source.monogon.dev/metropolis/node/core/consensus/client"
	cpb "source.monogon.dev/metropolis/node/core/curator/proto/api"
	"source.monogon.dev/metropolis/node/core/identity"
	"source.monogon.dev/metropolis/node/core/rpc"
	apb "source.monogon.dev/metropolis/proto/api"
	"source.monogon.dev/osbase/event/memory"
	"source.monogon.dev/osbase/supervisor"
)

// listener is the curator runnable responsible for listening for gRPC
// connections and forwarding them over to one of two possible implementations:
// - a local leader implementation which is backed by etcd
// - a follower implementation that forwards the RPCs over to a remote leader.
//
// Its goal is to make any switches over between leader and follower painless to
// the gRPC callers. Each incoming RPC first goes into a shim defined directly
// on the listener, then goes on to be passed into either implementation with a
// context that is valid as long as that implementation is current.
//
// Any calls which are pending during a switchover will have their context
// canceled with UNAVAILABLE and an error message describing the fact that the
// implementation has been switched over. The gRPC sockets will always be
// listening for connections, and block until able to serve a request (either
// locally or by forwarding). No retries will be attempted on switchover, as
// some calls might not be idempotent and the caller is better equipped to know
// when to retry.
type listener struct {
	node *identity.NodeCredentials
	// etcd is a client to the locally running consensus (etcd) server which is used
	// both for storing lock/leader election status and actual Curator data.
	etcd            client.Namespaced
	consensusStatus *consensus.Status

	consensus consensus.ServiceHandle
	status    *memory.Value[*electionStatus]
}

// run is the listener runnable. It listens on the Curator's gRPC socket, either
// by starting a leader or follower instance.
func (l *listener) run(ctx context.Context) error {

	// First, figure out what we're ought to be running by watching the election and
	// waiting for a result.
	w := l.status.Watch()
	defer w.Close()

	supervisor.Logger(ctx).Infof("Waiting for election status...")
	st, err := w.Get(ctx)
	if err != nil {
		return fmt.Errorf("could not get election status: %w", err)
	}

	// Short circuit a possible situation in which we're a follower of an unknown
	// leader, or neither a follower nor a leader.
	if (st.leader == nil && st.follower == nil) || (st.follower != nil && st.follower.lock == nil) {
		return fmt.Errorf("curator is neither leader nor follower - this is likely transient, restarting listener now")
	}

	if st.leader != nil && st.follower != nil {
		// This indicates a serious programming error. Let's catch it explicitly.
		panic("curator listener is supposed to run both as leader and follower")
	}

	sec := rpc.ServerSecurity{
		NodeCredentials: l.node,
	}

	// Prepare a gRPC server and listener.
	logger := supervisor.MustSubLogger(ctx, "rpc")
	opts := sec.GRPCOptions(logger)
	opts = append(opts, grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
		MinTime:             time.Second,
		PermitWithoutStream: true,
	}))
	srv := grpc.NewServer(opts...)
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", allocs.PortCuratorService))
	if err != nil {
		return fmt.Errorf("failed to listen on curator socket: %w", err)
	}
	defer lis.Close()

	// Depending on the election status, register either a leader or a follower to
	// the gRPC server.
	switch {
	case st.leader != nil:
		supervisor.Logger(ctx).Infof("This curator is a leader.")

		// Create a leader instance and serve it over gRPC.
		leader := newCuratorLeader(&leadership{
			lockKey:         st.leader.lockKey,
			lockRev:         st.leader.lockRev,
			leaderID:        l.node.ID(),
			etcd:            l.etcd,
			consensusStatus: l.consensusStatus,
			consensus:       l.consensus,
		}, &l.node.Node)

		cpb.RegisterCuratorServer(srv, leader)
		cpb.RegisterCuratorLocalServer(srv, leader)
		apb.RegisterAAAServer(srv, leader)
		apb.RegisterManagementServer(srv, leader)

		if err := supervisor.Run(ctx, "background", leader.background); err != nil {
			return fmt.Errorf("could not run leader background processor: %w", err)
		}
	case st.follower != nil:
		supervisor.Logger(ctx).Infof("This curator is a follower (leader is %q), starting minimal implementation.", st.follower.lock.NodeId)

		// Create a follower instance and serve it over gRPC.
		follower := &curatorFollower{
			etcd:       l.etcd,
			followerID: l.node.ID(),
			status:     l.status,
			node:       l.node.Node,
		}
		cpb.RegisterCuratorLocalServer(srv, follower)
	}

	// Start running the server as a runnable, stopping whenever this runnable exits
	// (on leadership change) or crashes. It's set to not be terminated gracefully
	// because:
	//  1. Followers should notify (by closing) clients about a leadership change as
	//     early as possible,
	//  2. Any long-running leadership calls will start failing anyway as the
	//     leadership has been lost.
	runnable := supervisor.GRPCServer(srv, lis, false)
	if err := supervisor.Run(ctx, "server", runnable); err != nil {
		return fmt.Errorf("could not run server: %w", err)
	}
	supervisor.Signal(ctx, supervisor.SignalHealthy)

	// Act upon any leadership changes. This depends on whether we were running a
	// leader or a follower.
	switch {
	case st.leader != nil:
		supervisor.Logger(ctx).Infof("Leader running until leadership lost.")
		for {
			nst, err := w.Get(ctx)
			if err != nil {
				return fmt.Errorf("getting election status after starting listener failed, bailing just in case: %w", err)
			}
			if nst.leader == nil {
				return fmt.Errorf("this curator stopped being a leader, quitting")
			}
			if !st.leader.equal(nst.leader) {
				return fmt.Errorf("this curator got re-elected, quitting")
			}
		}
	case st.follower != nil:
		supervisor.Logger(ctx).Infof("Follower running until leadership change.")
		for {
			nst, err := w.Get(ctx)
			if err != nil {
				return fmt.Errorf("getting election status after starting listener failed, bailing just in case: %w", err)
			}
			if nst.follower == nil {
				return fmt.Errorf("this curator stopped being a follower, quitting")
			}
			if !st.follower.equal(nst.follower) {
				return fmt.Errorf("leader changed, quitting")
			}
		}
	default:
		panic("unreachable")
	}
}
