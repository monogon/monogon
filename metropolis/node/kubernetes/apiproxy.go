// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"net"

	"source.monogon.dev/go/net/tinylb"
	"source.monogon.dev/metropolis/node/allocs"
	ipb "source.monogon.dev/metropolis/node/core/curator/proto/api"
	"source.monogon.dev/metropolis/node/core/curator/watcher"
	"source.monogon.dev/osbase/event/memory"
)

// updateLoadBalancerAPIServers provides a tinylb BackendSet memory value with
// the currently known nodes running a Kubernetes apiserver as retrieved from the
// given curator client.
func updateLoadbalancerAPIServers(ctx context.Context, val *memory.Value[tinylb.BackendSet], cur ipb.CuratorClient) error {
	set := &tinylb.BackendSet{}
	val.Set(set.Clone())

	return watcher.WatchNodes(ctx, cur, watcher.SimpleFollower{
		FilterFn: func(a *ipb.Node) bool {
			if a.Status == nil {
				return false
			}
			if a.Status.ExternalAddress == "" {
				return false
			}
			if a.Roles.KubernetesController == nil {
				return false
			}
			return true
		},
		EqualsFn: func(a *ipb.Node, b *ipb.Node) bool {
			return a.Status.ExternalAddress == b.Status.ExternalAddress
		},
		OnNewUpdated: func(new *ipb.Node) error {
			set.Insert(new.Id, &tinylb.SimpleTCPBackend{
				Remote: net.JoinHostPort(new.Status.ExternalAddress, allocs.PortKubernetesAPI.PortString()),
			})
			val.Set(set.Clone())
			return nil
		},
		OnDeleted: func(prev *ipb.Node) error {
			set.Delete(prev.Id)
			val.Set(set.Clone())
			return nil
		},
	})
}
