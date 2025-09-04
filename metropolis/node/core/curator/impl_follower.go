// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package curator

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"source.monogon.dev/metropolis/node/allocs"
	"source.monogon.dev/metropolis/node/core/consensus/client"
	cpb "source.monogon.dev/metropolis/node/core/curator/proto/api"
	"source.monogon.dev/metropolis/node/core/identity"
	"source.monogon.dev/metropolis/node/core/rpc"
	"source.monogon.dev/osbase/event/memory"
)

type curatorFollower struct {
	etcd       client.Namespaced
	followerID string
	node       identity.Node

	status *memory.Value[*electionStatus]
}

func (f *curatorFollower) GetCurrentLeader(_ *cpb.GetCurrentLeaderRequest, srv cpb.CuratorLocal_GetCurrentLeaderServer) error {
	ctx := srv.Context()

	w := f.status.Watch()
	defer w.Close()

	for {
		st, err := w.Get(srv.Context())
		if err != nil {
			return err
		}

		if st.follower == nil {
			return status.Errorf(codes.Unavailable, "election status changed, try again")
		}

		lock := st.follower.lock
		// Manually load node status data from etcd, even though we are not a leader.
		// This is fine, as if we ever end up serving stale data, the client will
		// realize and call us again.
		key, err := NodeEtcdPrefix.Key(lock.NodeId)
		if err != nil {
			rpc.Trace(ctx).Printf("invalid leader node id %q: %v", lock.NodeId, err)
			return status.Errorf(codes.Internal, "current leader has invalid node id")
		}
		res, err := f.etcd.Get(ctx, key)
		if err != nil {
			rpc.Trace(ctx).Printf("could not get current leader's data: %v", err)
			return status.Errorf(codes.Internal, "could not get current leader's data")
		}
		if len(res.Kvs) < 1 {
			rpc.Trace(ctx).Printf("could not get current leader's data: 0 kvs")
			return status.Errorf(codes.Internal, "could not get current leader's data")
		}
		node, err := nodeUnmarshal(res.Kvs[0])
		if err != nil {
			rpc.Trace(ctx).Printf("could not unmarshal leader node %s: %v", lock.NodeId, err)
			return status.Errorf(codes.Unavailable, "could not unmarshal leader node")
		}
		if node.status == nil || node.status.ExternalAddress == "" {
			rpc.Trace(ctx).Printf("leader node %s has no address in status", lock.NodeId)
			return status.Errorf(codes.Unavailable, "current leader has no reported address")
		}

		rpc.Trace(ctx).Printf("Sending leader: %s at %s", lock.NodeId, node.status.ExternalAddress)
		err = srv.Send(&cpb.GetCurrentLeaderResponse{
			LeaderNodeId: lock.NodeId,
			LeaderHost:   node.status.ExternalAddress,
			LeaderPort:   int32(allocs.PortCuratorService),
			ThisNodeId:   f.followerID,
		})
		if err != nil {
			return err
		}
	}
}

func (f *curatorFollower) GetCACertificate(ctx context.Context, _ *cpb.GetCACertificateRequest) (*cpb.GetCACertificateResponse, error) {
	return &cpb.GetCACertificateResponse{
		IdentityCaCertificate: f.node.ClusterCA().Raw,
	}, nil
}
