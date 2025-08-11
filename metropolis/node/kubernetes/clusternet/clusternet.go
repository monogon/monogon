// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Package clusternet implements a WireGuard-based overlay network for
// Kubernetes. It relies on controller-manager's IPAM to assign IP ranges to
// nodes and on Kubernetes' Node objects to distribute the Node IPs and public
// keys.
//
// It sets up a single WireGuard network interface and routes the entire
// ClusterCIDR into that network interface, relying on WireGuard's AllowedIPs
// mechanism to look up the correct peer node to send the traffic to. This
// means that the routing table doesn't change and doesn't have to be
// separately managed. When clusternet is started it annotates its WireGuard
// public key onto its node object.
// For each node object that's created or updated on the K8s apiserver it
// checks if a public key annotation is set and if yes a peer with that public
// key, its InternalIP as endpoint and the CIDR for that node as AllowedIPs is
// created.
package clusternet

import (
	"context"
	"net/netip"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"source.monogon.dev/go/logging"
	"source.monogon.dev/metropolis/node/core/network/ipam"
	"source.monogon.dev/osbase/event"
	"source.monogon.dev/osbase/supervisor"
)

type Service struct {
	NodeName   string
	Kubernetes kubernetes.Interface
	Prefixes   event.Value[*ipam.Prefixes]

	logger logging.Leveled
}

// ensureNode is called any time the node that this Service is running on gets
// updated. It uses this data to update this node's prefixes in the Curator.
func (s *Service) ensureNode(newNode *corev1.Node) error {
	if newNode.Name != s.NodeName {
		// We only care about our own node
		return nil
	}

	var prefixes ipam.Prefixes
	for _, podNetStr := range newNode.Spec.PodCIDRs {
		prefix, err := netip.ParsePrefix(podNetStr)
		if err != nil {
			s.logger.Warningf("Node %s PodCIDR failed to parse, ignored: %v", newNode.Name, err)
			continue
		}
		prefixes = append(prefixes, prefix)
	}

	s.logger.V(1).Infof("Updating locally originated prefixes: %+v", prefixes)
	s.Prefixes.Set(&prefixes)
	return nil
}

// Run runs the ClusterNet service. See package description for what it does.
func (s *Service) Run(ctx context.Context) error {
	logger := supervisor.Logger(ctx)
	s.logger = logger

	// Make a 'shared' informer. It's shared by name, but we don't actually share it
	// - instead we have to use it as the standard Informer API does not support
	// error handling. And we want to use a dedicated informer because we want to
	// only watch our own node.
	lw := cache.NewListWatchFromClient(
		s.Kubernetes.CoreV1().RESTClient(),
		"nodes", "",
		fields.OneTermEqualSelector("metadata.name", s.NodeName),
	)
	ni := cache.NewSharedInformer(lw, &corev1.Node{}, time.Second*5)
	ni.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(new interface{}) {
			newNode, ok := new.(*corev1.Node)
			if !ok {
				logger.Errorf("Received non-node item %+v in node event handler", new)
				return
			}
			if err := s.ensureNode(newNode); err != nil {
				logger.Warningf("Failed to sync node: %v", err)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			newNode, ok := new.(*corev1.Node)
			if !ok {
				logger.Errorf("Received non-node item %+v in node event handler", new)
				return
			}
			if err := s.ensureNode(newNode); err != nil {
				logger.Warningf("Failed to sync node: %v", err)
			}
		},
	})
	ni.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		supervisor.Logger(ctx).Errorf("node informer watch error: %v", err)
	})

	supervisor.Signal(ctx, supervisor.SignalHealthy)
	ni.Run(ctx.Done())
	return ctx.Err()
}
