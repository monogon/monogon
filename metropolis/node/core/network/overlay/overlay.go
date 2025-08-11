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
// interface. This is used in practice to allow other host nodes (whose external
// addresses are outside the cluster network) to access the cluster network.
//
// Second, we have two hardcoded/purpose-specific sources of prefixes:
//  1. Pod networking node prefixes from the kubelet
//  2. The host's external IP address (as a /32) from the network service.
package overlay

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"

	"github.com/cenkalti/backoff/v4"
	"github.com/vishvananda/netlink"

	"source.monogon.dev/metropolis/node"
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
	// LocalKubernetesPodNetwork is an event.Value watched for prefixes that should
	// be announced into the mesh. This is to be Set by the Kubernetes service once
	// it knows about the local node's IPAM address assignment.
	LocalKubernetesPodNetwork event.Value[*ipam.Prefixes]
	// Network service used to get the local node's IP address to submit it as a /32.
	Network event.Value[*node.NetStatus]

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

	kubeC := make(chan *ipam.Prefixes)
	netC := make(chan *node.NetStatus)
	if err := supervisor.RunGroup(ctx, map[string]supervisor.Runnable{
		"source-kubernetes": event.Pipe(s.LocalKubernetesPodNetwork, kubeC),
		"source-network":    event.Pipe(s.Network, netC),
		"push": func(ctx context.Context) error {
			return s.push(ctx, kubeC, netC)
		},
	}); err != nil {
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
func (s *Service) push(ctx context.Context, kubeC chan *ipam.Prefixes, netC chan *node.NetStatus) error {
	supervisor.Signal(ctx, supervisor.SignalHealthy)

	var kubePrefixes *ipam.Prefixes
	var prevKubePrefixes *ipam.Prefixes

	var localAddr net.IP
	var prevLocalAddr net.IP

	for {
		kubeChanged := false
		localChanged := false

		select {
		case <-ctx.Done():
			return ctx.Err()
		case kubePrefixes = <-kubeC:
			if !kubePrefixes.Equal(prevKubePrefixes) {
				kubeChanged = true
			}
		case n := <-netC:
			localAddr = n.ExternalAddress
			if !localAddr.Equal(prevLocalAddr) {
				localChanged = true
			}
		}

		if kubeChanged {
			if err := configureKubeNetwork(prevKubePrefixes, kubePrefixes); err != nil {
				supervisor.Logger(ctx).Warningf("Could not configure cluster networking update: %v", err)
			}
		}

		// Ignore spurious updates.
		if !localChanged && !kubeChanged {
			continue
		}

		// Prepare prefixes to submit to cluster.
		var prefixes ipam.Prefixes

		// Do we have a local node address? Add it to the prefixes.
		if len(localAddr) > 0 {
			addr, ok := netip.AddrFromSlice(localAddr)
			if ok {
				prefixes = append(prefixes, netip.PrefixFrom(addr, 32))
			}
		}
		// Do we have any kubelet prefixes? Add them, too.
		if kubePrefixes != nil {
			prefixes.Update(kubePrefixes)
		}

		supervisor.Logger(ctx).Infof("Submitting prefixes: %s (kube update: %v, local update: %v)", prefixes, kubeChanged, localChanged)

		err := backoff.Retry(func() error {
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

		prevKubePrefixes = kubePrefixes
		prevLocalAddr = localAddr

	}
}

// configureKubeNetwork configures the point-to-point peer IP address of the
// node host network namespace (i.e. the one container P2P interfaces point to)
// on its loopback interface to make it eligible to be used as a source IP
// address for communication into the clusternet overlay.
func configureKubeNetwork(oldPrefixes *ipam.Prefixes, newPrefixes *ipam.Prefixes) error {
	// diff maps prefixes to be removed to false
	// and prefixes to be added to true.
	diff := make(map[netip.Prefix]bool)

	if newPrefixes != nil {
		for _, newAddr := range *newPrefixes {
			diff[newAddr] = true
		}
	}

	if oldPrefixes != nil {
		for _, oldAddr := range *oldPrefixes {
			// Remove all prefixes in both the old
			// and new prefix sets from `diff`.
			if diff[oldAddr] {
				delete(diff, oldAddr)
				continue
			}

			// Mark all remaining (i.e. ones not in the new prefix set)
			// prefixes for removal.
			diff[oldAddr] = false
		}
	}

	loInterface, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("while getting lo interface: %w", err)
	}

	for prefix, shouldAdd := range diff {
		// By CNI convention the first IP after the subnet base address is the
		// point-to-point partner for all pod veths. To make this IP eligible
		// to be used as a general host network namespace source IP we also add
		// it to the loopback interface. This ensures that the kernel picks it
		// as the source IP for traffic flowing into clusternet
		// (due to its preference for source IPs in the same subnet).
		addr := &netlink.Addr{
			IPNet: &net.IPNet{
				IP:   prefix.Addr().Next().AsSlice(),
				Mask: net.CIDRMask(prefix.Addr().BitLen(), prefix.Addr().BitLen()),
			},
		}

		if shouldAdd {
			if err := netlink.AddrAdd(loInterface, addr); err != nil {
				return fmt.Errorf("assigning extra loopback IP: %w", err)
			}
		} else {
			if err := netlink.AddrDel(loInterface, addr); err != nil {
				return fmt.Errorf("removing extra loopback IP: %w", err)
			}
		}
	}

	return nil
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
