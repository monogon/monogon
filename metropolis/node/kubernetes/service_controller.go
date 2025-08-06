// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"fmt"
	"net"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"source.monogon.dev/metropolis/node"
	"source.monogon.dev/metropolis/node/core/consensus"
	"source.monogon.dev/metropolis/node/core/identity"
	"source.monogon.dev/metropolis/node/core/localstorage"
	"source.monogon.dev/metropolis/node/core/network"
	"source.monogon.dev/metropolis/node/kubernetes/authproxy"
	"source.monogon.dev/metropolis/node/kubernetes/metricsproxy"
	"source.monogon.dev/metropolis/node/kubernetes/pki"
	"source.monogon.dev/metropolis/node/kubernetes/reconciler"
	"source.monogon.dev/osbase/supervisor"

	ipb "source.monogon.dev/metropolis/node/core/curator/proto/api"
	apb "source.monogon.dev/metropolis/proto/api"
)

type ConfigController struct {
	ServiceIPRange net.IPNet
	ClusterNet     net.IPNet

	KPKI       *pki.PKI
	Root       *localstorage.Root
	Consensus  consensus.ServiceHandle
	Network    *network.Service
	Node       *identity.NodeCredentials
	Curator    ipb.CuratorClient
	Management apb.ManagementClient
}

type Controller struct {
	c ConfigController
}

func NewController(c ConfigController) *Controller {
	s := &Controller{
		c: c,
	}
	return s
}

func (s *Controller) Run(ctx context.Context) error {
	controllerManagerConfig, err := getPKIControllerManagerConfig(ctx, s.c.KPKI)
	if err != nil {
		return fmt.Errorf("could not generate controller manager pki config: %w", err)
	}
	controllerManagerConfig.clusterNet = s.c.ClusterNet
	controllerManagerConfig.serviceNet = s.c.ServiceIPRange
	schedulerConfig, err := getPKISchedulerConfig(ctx, s.c.KPKI)
	if err != nil {
		return fmt.Errorf("could not generate scheduler pki config: %w", err)
	}

	masterKubeconfig, err := s.c.KPKI.Kubeconfig(ctx, pki.Master, pki.KubernetesAPIEndpointForController)
	if err != nil {
		return fmt.Errorf("could not generate master kubeconfig: %w", err)
	}

	rawClientConfig, err := clientcmd.NewClientConfigFromBytes(masterKubeconfig)
	if err != nil {
		return fmt.Errorf("could not generate kubernetes client config: %w", err)
	}

	clientConfig, err := rawClientConfig.ClientConfig()
	if err != nil {
		return fmt.Errorf("could not fetch generate client config: %w", err)
	}
	clientSet, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return fmt.Errorf("could not generate kubernetes client: %w", err)
	}

	supervisor.Logger(ctx).Infof("Waiting for consensus...")
	w := s.c.Consensus.Watch()
	defer w.Close()
	st, err := w.Get(ctx, consensus.FilterRunning)
	if err != nil {
		return fmt.Errorf("while waiting for consensus: %w", err)
	}
	etcd, err := st.CuratorClient()
	if err != nil {
		return fmt.Errorf("while retrieving consensus client: %w", err)
	}

	// Sub-runnable which starts all parts of Kubernetes that depend on the
	// machine's external IP address. If it changes, the runnable will exit.
	// TODO(q3k): test this
	supervisor.Run(ctx, "networked", func(ctx context.Context) error {
		networkWatch := s.c.Network.Status.Watch()
		defer networkWatch.Close()

		var status *node.NetStatus

		supervisor.Logger(ctx).Info("Waiting for node networking...")
		for status == nil || status.ExternalAddress == nil {
			status, err = networkWatch.Get(ctx)
			if err != nil {
				return fmt.Errorf("failed to get network status: %w", err)
			}
		}
		address := status.ExternalAddress
		supervisor.Logger(ctx).Info("Node has active networking, starting apiserver/kubelet")

		apiserver := &apiserverService{
			KPKI:                        s.c.KPKI,
			AdvertiseAddress:            address,
			ServiceIPRange:              s.c.ServiceIPRange,
			EphemeralConsensusDirectory: &s.c.Root.Ephemeral.Consensus,
		}

		err := supervisor.RunGroup(ctx, map[string]supervisor.Runnable{
			"apiserver": apiserver.Run,
		})
		if err != nil {
			return fmt.Errorf("when starting apiserver/kubelet: %w", err)
		}

		supervisor.Signal(ctx, supervisor.SignalHealthy)

		for status.ExternalAddress.Equal(address) {
			status, err = networkWatch.Get(ctx)
			if err != nil {
				return fmt.Errorf("when watching for network changes: %w", err)
			}
		}
		return fmt.Errorf("network configuration changed (%s -> %s)", address.String(), status.ExternalAddress.String())
	})

	reconcilerService := &reconciler.Service{
		Etcd:      etcd,
		ClientSet: clientSet,
		NodeID:    s.c.Node.ID(),
	}
	err = supervisor.Run(ctx, "reconciler", reconcilerService.Run)
	if err != nil {
		return fmt.Errorf("could not run sub-service reconciler: %w", err)
	}

	lm := labelmaker{
		clientSet: clientSet,
		curator:   s.c.Curator,
		mgmt:      s.c.Management,
	}
	if err := supervisor.Run(ctx, "labelmaker", lm.run); err != nil {
		return err
	}

	// Before we start anything else, make sure reconciliation passes at least once.
	// This makes the initial startup of a cluster much cleaner as we don't end up
	// starting the scheduler/controller-manager/etc just to get them to immediately
	// fail and back off with 'unauthorized'.
	supervisor.Logger(ctx).Info("Waiting for reconciler...")
	err = reconcilerService.WaitReady(ctx)
	if err != nil {
		return fmt.Errorf("while waiting for reconciler: %w", err)
	}
	supervisor.Logger(ctx).Info("Reconciler is done.")

	authProxy := authproxy.Service{
		KPKI: s.c.KPKI,
		Node: s.c.Node,
	}

	metricsProxy := metricsproxy.Service{
		KPKI: s.c.KPKI,
	}

	for _, sub := range []struct {
		name     string
		runnable supervisor.Runnable
	}{
		{"controller-manager", runControllerManager(*controllerManagerConfig)},
		{"scheduler", runScheduler(*schedulerConfig)},
		{"authproxy", authProxy.Run},
		{"metricsproxy", metricsProxy.Run},
	} {
		err := supervisor.Run(ctx, sub.name, sub.runnable)
		if err != nil {
			return fmt.Errorf("could not run sub-service %q: %w", sub.name, err)
		}
	}

	supervisor.Signal(ctx, supervisor.SignalHealthy)
	<-ctx.Done()
	return nil
}

// GetDebugKubeconfig issues a kubeconfig for an arbitrary given identity.
// Useful for debugging and testing.
func (s *Controller) GetDebugKubeconfig(ctx context.Context, request *apb.GetDebugKubeconfigRequest) (*apb.GetDebugKubeconfigResponse, error) {
	client, err := s.c.KPKI.VolatileClient(ctx, request.Id, request.Groups)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "Failed to get volatile client certificate: %v", err)
	}
	kubeconfig, err := pki.Kubeconfig(ctx, s.c.KPKI.KV, client, pki.KubernetesAPIEndpointForController)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "Failed to generate kubeconfig: %v", err)
	}
	return &apb.GetDebugKubeconfigResponse{DebugKubeconfig: string(kubeconfig)}, nil
}
