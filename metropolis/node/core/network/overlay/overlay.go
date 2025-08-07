// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Package overlay implements a Cluster Networking mesh service running on all
// Metropolis nodes.
//
// The mesh is based on wireguard and a centralized configuration store in the
// cluster Curator (in etcd).
//
// While the implementation is nearly generic, it currently makes an assumption
// that it is used only for Kubernetes pod networking. That has a few
// implications:
//
// First, we only have a single real route on the host into the wireguard
// networking mesh / interface, and that is configured ahead of time in the
// Service as ClusterNet. All destination addresses that should be carried by the
// mesh must thus be part of this single route. Otherwise, traffic will be able
// to flow into the node from other nodes, but will exit through another
// interface.
package overlay

import (
	"context"
	"fmt"
	"net"
	"slices"

	"github.com/cenkalti/backoff/v4"

	"source.monogon.dev/metropolis/node/core/curator/watcher"
	"source.monogon.dev/metropolis/node/core/localstorage"
	"source.monogon.dev/metropolis/node/core/network/ipam"
	"source.monogon.dev/osbase/event"
	"source.monogon.dev/osbase/supervisor"

	apb "source.monogon.dev/metropolis/node/core/curator/proto/api"
	cpb "source.monogon.dev/metropolis/proto/common"
)

// Service implements the Cluster Networking Mesh. See package-level docs for
// more details.
type Service struct {
	// Curator is the gRPC client that the service will use to reach the cluster's
	// Curator, for pushing locally announced prefixes and pulling information about
	// other nodes.
	Curator apb.CuratorClient
	// ClusterNet is the prefix that will be programmed to exit through the wireguard
	// mesh.
	ClusterNet net.IPNet
	// DataDirectory is where the WireGuard key of this node will be stored.
	DataDirectory *localstorage.DataKubernetesClusterNetworkingDirectory
	// OverlayPrefixes is an event.Value watched for prefixes that should
	// be announced into the mesh.
	OverlayPrefixes event.Value[*ipam.Prefixes]

	// wg is the interface to all the low-level interactions with WireGuard (and
	// kernel routing). If not set, this defaults to a production implementation.
	// This can be overridden by test to a test implementation instead.
	wg wireguard
}

// Run the Service. This must be used in a supervisor Runnable.
func (s *Service) Run(ctx context.Context) error {
	if s.wg == nil {
		s.wg = &localWireguard{}
	}
	if err := s.wg.ensureOnDiskKey(s.DataDirectory); err != nil {
		return fmt.Errorf("could not ensure wireguard key: %w", err)
	}
	if err := s.wg.setup(&s.ClusterNet); err != nil {
		return fmt.Errorf("could not setup wireguard: %w", err)
	}

	supervisor.Logger(ctx).Infof("Wireguard setup complete, starting updaters...")

	if err := supervisor.Run(ctx, "push", s.push); err != nil {
		return err
	}

	if err := supervisor.Run(ctx, "pull", s.pull); err != nil {
		return err
	}
	supervisor.Signal(ctx, supervisor.SignalHealthy)
	<-ctx.Done()
	return ctx.Err()
}

// push is the sub-runnable responsible for letting the Curator know about what
// prefixes that are originated by this node.
func (s *Service) push(ctx context.Context) error {
	w := s.OverlayPrefixes.Watch()
	supervisor.Signal(ctx, supervisor.SignalHealthy)

	var prevKubePrefixes *ipam.Prefixes
	for {
		prefixes, err := w.Get(ctx)
		if err != nil {
			return err
		}
		if prefixes.Equal(prevKubePrefixes) {
			continue
		}
		supervisor.Logger(ctx).Infof("Submitting prefixes: %s", prefixes)

		err = backoff.Retry(func() error {
			_, err := s.Curator.UpdateNodeClusterNetworking(ctx, &apb.UpdateNodeClusterNetworkingRequest{
				Clusternet: &cpb.NodeClusterNetworking{
					WireguardPubkey: s.wg.key().PublicKey().String(),
					Prefixes:        prefixes.Proto(),
				},
			})
			if err != nil {
				supervisor.Logger(ctx).Warningf("Could not submit cluster networking update: %v", err)
			}
			return err
		}, backoff.WithContext(backoff.NewExponentialBackOff(), ctx))
		if err != nil {
			return fmt.Errorf("couldn't update curator: %w", err)
		}

		prevKubePrefixes = prefixes
	}
}

// pull is the sub-runnable responsible for fetching information about the
// cluster networking setup/status of other nodes, and programming it as
// WireGuard peers.
func (s *Service) pull(ctx context.Context) error {
	supervisor.Signal(ctx, supervisor.SignalHealthy)

	var batch []*apb.Node
	return watcher.WatchNodes(ctx, s.Curator, watcher.SimpleFollower{
		FilterFn: func(a *apb.Node) bool {
			if a.Clusternet == nil {
				return false
			}
			if a.Clusternet.WireguardPubkey == "" {
				return false
			}
			return true
		},
		EqualsFn: func(a *apb.Node, b *apb.Node) bool {
			if a.Status.ExternalAddress != b.Status.ExternalAddress {
				return false
			}
			if a.Clusternet.WireguardPubkey != b.Clusternet.WireguardPubkey {
				return false
			}
			if !slices.Equal(a.Clusternet.Prefixes, b.Clusternet.Prefixes) {
				return false
			}
			return true
		},
		OnNewUpdated: func(new *apb.Node) error {
			batch = append(batch, new)
			return nil
		},
		OnBatchDone: func() error {
			if err := s.wg.configurePeers(batch); err != nil {
				supervisor.Logger(ctx).Errorf("nodes couldn't be configured: %v", err)
			}
			batch = nil
			return nil
		},
		OnDeleted: func(prev *apb.Node) error {
			if err := s.wg.unconfigurePeer(prev); err != nil {
				supervisor.Logger(ctx).Errorf("Node %s couldn't be unconfigured: %v", prev.Id, err)
			}
			return nil
		},
	})
}
