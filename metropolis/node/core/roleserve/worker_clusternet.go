// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package roleserve

import (
	"context"
	"net"

	"source.monogon.dev/metropolis/node/core/localstorage"
	"source.monogon.dev/metropolis/node/core/network/ipam"
	"source.monogon.dev/metropolis/node/core/network/overlay"
	"source.monogon.dev/osbase/event/memory"
	"source.monogon.dev/osbase/supervisor"

	ipb "source.monogon.dev/metropolis/node/core/curator/proto/api"
)

type workerClusternet struct {
	storageRoot *localstorage.Root

	// curatorConnection will be read
	curatorConnection *memory.Value[*CuratorConnection]
	// podNetwork will be read.
	podNetwork *memory.Value[*ipam.Prefixes]
}

func (s *workerClusternet) run(ctx context.Context) error {
	w := s.curatorConnection.Watch()
	defer w.Close()
	supervisor.Logger(ctx).Infof("Waiting for curator connection...")
	cc, err := w.Get(ctx)
	if err != nil {
		return err
	}
	supervisor.Logger(ctx).Infof("Got curator connection, starting...")
	cur := ipb.NewCuratorClient(cc.conn)

	svc := overlay.Service{
		Curator: cur,
		ClusterNet: net.IPNet{
			IP:   []byte{10, 192, 0, 0},
			Mask: net.IPMask{255, 224, 0, 0},
		},
		DataDirectory:   &s.storageRoot.Data.Kubernetes.ClusterNetworking,
		OverlayPrefixes: s.podNetwork,
	}
	return svc.Run(ctx)
}
