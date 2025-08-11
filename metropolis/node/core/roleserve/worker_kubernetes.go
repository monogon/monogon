// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package roleserve

import (
	"context"
	"fmt"
	"net"

	"source.monogon.dev/metropolis/node/core/identity"
	"source.monogon.dev/metropolis/node/core/localstorage"
	"source.monogon.dev/metropolis/node/core/network"
	"source.monogon.dev/metropolis/node/core/network/ipam"
	"source.monogon.dev/metropolis/node/kubernetes"
	"source.monogon.dev/metropolis/node/kubernetes/containerd"
	kpki "source.monogon.dev/metropolis/node/kubernetes/pki"
	"source.monogon.dev/osbase/event"
	"source.monogon.dev/osbase/event/memory"
	"source.monogon.dev/osbase/net/dns"
	"source.monogon.dev/osbase/supervisor"

	ipb "source.monogon.dev/metropolis/node/core/curator/proto/api"
	apb "source.monogon.dev/metropolis/proto/api"
	cpb "source.monogon.dev/metropolis/proto/common"
)

// workerKubernetes is the Kubernetes Worker, responsible for launching
// (currently a control plane / data plane converged) Kubernetes payload on
// Metropolis.
//
// This currently requires locally available Consensus, until it is split up
// into Control/Data plane parts (where then the API server must still be
// colocated with Consensus, but all the other services don't have to).
type workerKubernetes struct {
	network     *network.Service
	storageRoot *localstorage.Root

	localRoles        *memory.Value[*cpb.NodeRoles]
	localControlPlane *memory.Value[*localControlPlane]
	curatorConnection *memory.Value[*CuratorConnection]
	kubernetesStatus  *memory.Value[*KubernetesStatus]
	podNetwork        *memory.Value[*ipam.Prefixes]
}

// kubernetesStartup is used internally to provide a reduced (as in MapReduce
// reduced) datum for the main Kubernetes launcher responsible for starting the
// service, if at all.
type kubernetesStartup struct {
	roles      *cpb.NodeRoles
	lcp        *localControlPlane
	curator    ipb.CuratorClient
	management apb.ManagementClient
	node       *identity.NodeCredentials
}

// changed informs the Kubernetes launcher whether two different
// kubernetesStartups differ to the point where a restart of Kubernetes should
// happen.
func (k *kubernetesStartup) workerChanged(o *kubernetesStartup) bool {
	hasKubernetesA := k.roles.KubernetesWorker != nil
	hasKubernetesB := o.roles.KubernetesWorker != nil

	return hasKubernetesA != hasKubernetesB
}

func (k *kubernetesStartup) controllerChanged(o *kubernetesStartup) bool {
	hasKubernetesA := k.roles.KubernetesController != nil
	hasKubernetesB := o.roles.KubernetesController != nil

	return hasKubernetesA != hasKubernetesB
}

func (s *workerKubernetes) run(ctx context.Context) error {
	var startupV memory.Value[*kubernetesStartup]

	localControlPlaneC := make(chan *localControlPlane)
	curatorConnectionC := make(chan *CuratorConnection)
	rolesC := make(chan *cpb.NodeRoles)

	supervisor.RunGroup(ctx, map[string]supervisor.Runnable{
		// Plain conversion from Event Value to channel.
		"map-local-control-plane": event.Pipe[*localControlPlane](s.localControlPlane, localControlPlaneC),
		// Plain conversion from Event Value to channel.
		"map-curator-connection": event.Pipe[*CuratorConnection](s.curatorConnection, curatorConnectionC),
		// Plain conversion from Event Value to channel.
		"map-roles": event.Pipe[*cpb.NodeRoles](s.localRoles, rolesC),
		// Provide config from the above.
		"reduce-config": func(ctx context.Context) error {
			supervisor.Signal(ctx, supervisor.SignalHealthy)
			var lr *cpb.NodeRoles
			var lcp *localControlPlane
			var cc *CuratorConnection
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case lr = <-rolesC:
				case lcp = <-localControlPlaneC:
				case cc = <-curatorConnectionC:
				}
				if lr != nil && cc != nil {
					startupV.Set(&kubernetesStartup{
						roles:      lr,
						lcp:        lcp,
						node:       cc.Credentials,
						curator:    ipb.NewCuratorClient(cc.conn),
						management: apb.NewManagementClient(cc.conn),
					})
				}
			}
		},
	})

	// TODO(q3k): make these configurable.
	clusterIPRange := net.IPNet{
		IP: net.IP{10, 192, 0, 0},
		// That's a /11.
		Mask: net.IPMask{0xff, 0xe0, 0x00, 0x00},
	}
	serviceIPRange := net.IPNet{
		IP: net.IP{10, 224, 0, 1},
		// That's a /16.
		Mask: net.IPMask{0xff, 0xff, 0x00, 0x00},
	}

	// TODO(q3k): remove this once the controller also uses curator-emitted PKI.
	clusterDomain := "cluster.local"

	// TODO(q3k): move worker services to worker.
	supervisor.Run(ctx, "controller", func(ctx context.Context) error {
		w := startupV.Watch()
		defer w.Close()
		supervisor.Logger(ctx).Infof("Waiting for startup data...")

		// Acquire kubernetesStartup, waiting for it to contain local consensus and a
		// KubernetesController local role.
		var d *kubernetesStartup
		for {
			var err error
			d, err = w.Get(ctx)
			if err != nil {
				return err
			}
			supervisor.Logger(ctx).Infof("Got new startup data.")
			if d.roles.KubernetesController == nil {
				supervisor.Logger(ctx).Infof("No Kubernetes controller role, not starting.")
				continue
			}
			if !d.lcp.exists() {
				supervisor.Logger(ctx).Warningf("No local consensus, cannot start.")
				continue
			}

			break
		}
		pki, err := kpki.FromLocalConsensus(ctx, d.lcp.consensus)
		if err != nil {
			return fmt.Errorf("getting kubernetes PKI client: %w", err)
		}

		supervisor.Logger(ctx).Infof("Starting Kubernetes controller...")

		controller := kubernetes.NewController(kubernetes.ConfigController{
			Node:           d.node,
			ServiceIPRange: serviceIPRange,
			ClusterNet:     clusterIPRange,
			KPKI:           pki,
			Root:           s.storageRoot,
			Consensus:      d.lcp.consensus,
			Network:        s.network,
			Curator:        d.curator,
			Management:     d.management,
		})
		// Start Kubernetes.
		if err := supervisor.Run(ctx, "run", controller.Run); err != nil {
			return fmt.Errorf("failed to start kubernetes controller service: %w", err)
		}

		// Let downstream know that Kubernetes is running.
		s.kubernetesStatus.Set(&KubernetesStatus{
			Controller: controller,
		})

		supervisor.Signal(ctx, supervisor.SignalHealthy)

		// Restart everything if we get a significantly different config (ie., a config
		// whose change would/should either turn up or tear down Kubernetes).
		for {
			nc, err := w.Get(ctx)
			if err != nil {
				return err
			}
			if nc.controllerChanged(d) {
				supervisor.Logger(ctx).Errorf("watcher got new config, restarting")
				return fmt.Errorf("restarting")
			}
		}
	})

	supervisor.Run(ctx, "worker", func(ctx context.Context) error {
		w := startupV.Watch()
		defer w.Close()
		supervisor.Logger(ctx).Infof("Waiting for startup data...")

		// Acquire kubernetesStartup, waiting for it to contain local consensus and a
		// KubernetesWorker local role.
		var d *kubernetesStartup
		for {
			var err error
			d, err = w.Get(ctx)
			if err != nil {
				return err
			}
			supervisor.Logger(ctx).Infof("Got new startup data.")
			if d.roles.KubernetesWorker == nil {
				supervisor.Logger(ctx).Infof("No Kubernetes worker role, not starting.")
				s.network.DNS.SetHandler("kubernetes", dns.EmptyDNSHandler{})
				continue
			}
			break
		}

		// Start containerd.
		containerdSvc := &containerd.Service{
			EphemeralVolume: &s.storageRoot.Ephemeral.Containerd,
		}
		if err := supervisor.Run(ctx, "containerd", containerdSvc.Run); err != nil {
			return fmt.Errorf("failed to start containerd service: %w", err)
		}

		worker := kubernetes.NewWorker(kubernetes.ConfigWorker{
			ServiceIPRange: serviceIPRange,
			ClusterNet:     clusterIPRange,
			ClusterDomain:  clusterDomain,

			Root:          s.storageRoot,
			Network:       s.network,
			NodeID:        d.node.ID(),
			CuratorClient: d.curator,
			PodNetwork:    s.podNetwork,
		})
		// Start Kubernetes.
		if err := supervisor.Run(ctx, "run", worker.Run); err != nil {
			return fmt.Errorf("failed to start kubernetes worker service: %w", err)
		}

		supervisor.Signal(ctx, supervisor.SignalHealthy)

		// Restart everything if we get a significantly different config (ie., a config
		// whose change would/should either turn up or tear down Kubernetes).
		for {
			nc, err := w.Get(ctx)
			if err != nil {
				return err
			}
			if nc.workerChanged(d) {
				supervisor.Logger(ctx).Errorf("watcher got new config, restarting")
				return fmt.Errorf("restarting")
			}
		}
	})

	supervisor.Signal(ctx, supervisor.SignalHealthy)
	<-ctx.Done()
	return ctx.Err()
}
