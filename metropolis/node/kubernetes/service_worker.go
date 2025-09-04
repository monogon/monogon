// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net"
	"net/netip"
	"time"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"source.monogon.dev/go/net/tinylb"
	"source.monogon.dev/metropolis/node/allocs"
	"source.monogon.dev/metropolis/node/core/localstorage"
	"source.monogon.dev/metropolis/node/core/metrics"
	"source.monogon.dev/metropolis/node/core/network"
	"source.monogon.dev/metropolis/node/core/network/ipam"
	"source.monogon.dev/metropolis/node/kubernetes/clusternet"
	"source.monogon.dev/metropolis/node/kubernetes/metricsprovider"
	"source.monogon.dev/metropolis/node/kubernetes/networkpolicy"
	"source.monogon.dev/metropolis/node/kubernetes/nfproxy"
	kpki "source.monogon.dev/metropolis/node/kubernetes/pki"
	"source.monogon.dev/metropolis/node/kubernetes/plugins/kvmdevice"
	"source.monogon.dev/osbase/event"
	"source.monogon.dev/osbase/event/memory"
	kubernetesDNS "source.monogon.dev/osbase/net/dns/kubernetes"
	"source.monogon.dev/osbase/supervisor"

	ipb "source.monogon.dev/metropolis/node/core/curator/proto/api"
)

type ConfigWorker struct {
	ServiceIPRange net.IPNet
	ClusterNet     net.IPNet
	ClusterDomain  string

	Root          *localstorage.Root
	Network       *network.Service
	NodeID        string
	CuratorClient ipb.CuratorClient
	PodNetwork    event.Value[*ipam.Prefixes]
}

type Worker struct {
	c ConfigWorker
}

func NewWorker(c ConfigWorker) *Worker {
	s := &Worker{
		c: c,
	}
	return s
}

func (s *Worker) Run(ctx context.Context) error {
	metrics.CoreRegistry.MustRegister(metricsprovider.Registry)
	defer metrics.CoreRegistry.Unregister(metricsprovider.Registry)
	// Run apiproxy, which load-balances connections from worker components to this
	// cluster's api servers. This is necessary as we want to round-robin across all
	// available apiservers, and Kubernetes components do not implement client-side
	// load-balancing.
	err := supervisor.Run(ctx, "apiproxy", func(ctx context.Context) error {
		lis, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", allocs.PortKubernetesWorkerLocalAPI))
		if err != nil {
			return fmt.Errorf("failed to listen: %w", err)
		}
		defer lis.Close()

		v := memory.Value[tinylb.BackendSet]{}
		srv := tinylb.Server{
			Provider: &v,
			Listener: lis,
		}
		err = supervisor.Run(ctx, "updater", func(ctx context.Context) error {
			return updateLoadbalancerAPIServers(ctx, &v, s.c.CuratorClient)
		})
		if err != nil {
			return err
		}

		supervisor.Logger(ctx).Infof("Starting proxy...")
		return srv.Run(ctx)
	})
	if err != nil {
		return err
	}

	kubelet := kubeletService{
		ClusterDomain:      s.c.ClusterDomain,
		KubeletDirectory:   &s.c.Root.Data.Kubernetes.Kubelet,
		EphemeralDirectory: &s.c.Root.Ephemeral,
		ClusterDNS:         []net.IP{allocs.IPContainerDNS},
	}

	// Gather all required material to send over for certficiate issuance to the
	// curator...
	kwr := &ipb.IssueCertificateRequest_KubernetesWorker{}

	kubeletPK, err := kubelet.getPubkey(ctx)
	if err != nil {
		return fmt.Errorf("when getting kubelet pubkey: %w", err)
	}
	kwr.KubeletPubkey = kubeletPK

	clients := map[string]*struct {
		dir *localstorage.PKIDirectory

		sk ed25519.PrivateKey
		pk ed25519.PublicKey

		client     *kubernetes.Clientset
		informers  informers.SharedInformerFactory
		kubeconfig []byte

		certFrom func(kw *ipb.IssueCertificateResponse_KubernetesWorker) []byte
	}{
		"csi": {
			dir: &s.c.Root.Data.Kubernetes.CSIProvisioner.PKI,
			certFrom: func(kw *ipb.IssueCertificateResponse_KubernetesWorker) []byte {
				return kw.CsiProvisionerCertificate
			},
		},
		"netserv": {
			dir: &s.c.Root.Data.Kubernetes.Netservices.PKI,
			certFrom: func(kw *ipb.IssueCertificateResponse_KubernetesWorker) []byte {
				return kw.NetservicesCertificate
			},
		},
	}

	for name, c := range clients {
		if err := c.dir.GeneratePrivateKey(); err != nil {
			return fmt.Errorf("generating %s key: %w", name, err)
		}
		k, err := c.dir.ReadPrivateKey()
		if err != nil {
			return fmt.Errorf("reading %s key: %w", name, err)
		}
		c.sk = k
		c.pk = c.sk.Public().(ed25519.PublicKey)
	}
	kwr.CsiProvisionerPubkey = clients["csi"].pk
	kwr.NetservicesPubkey = clients["netserv"].pk

	// ...issue certificates...
	res, err := s.c.CuratorClient.IssueCertificate(ctx, &ipb.IssueCertificateRequest{
		Kind: &ipb.IssueCertificateRequest_KubernetesWorker_{
			KubernetesWorker: kwr,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to get certificates from curator: %w", err)
	}
	kw := res.Kind.(*ipb.IssueCertificateResponse_KubernetesWorker_).KubernetesWorker

	// ...write them...
	if err := kubelet.setCertificates(kw); err != nil {
		return fmt.Errorf("failed to write kubelet certs: %w", err)
	}
	for name, c := range clients {
		if c.dir == nil {
			continue
		}
		if err := c.dir.WriteCertificates(kw.IdentityCaCertificate, c.certFrom(kw)); err != nil {
			return fmt.Errorf("failed to write %s certs: %w", name, err)
		}
	}

	// ... and set up connections.
	for name, c := range clients {
		kubeconf, err := kpki.KubeconfigRaw(kw.IdentityCaCertificate, c.certFrom(kw), c.sk, kpki.KubernetesAPIEndpointForWorker)
		if err != nil {
			return fmt.Errorf("failed to make %s kubeconfig: %w", name, err)
		}
		c.kubeconfig = kubeconf
		cs, informers, err := connectByKubeconfig(kubeconf)
		if err != nil {
			return fmt.Errorf("failed to connect with %s: %w", name, err)
		}
		c.client = cs
		c.informers = informers
	}

	csiPlugin := csiPluginServer{
		KubeletDirectory: &s.c.Root.Data.Kubernetes.Kubelet,
		VolumesDirectory: &s.c.Root.Data.Volumes,
	}

	csiProvisioner := csiProvisionerServer{
		NodeName:         s.c.NodeID,
		Kubernetes:       clients["csi"].client,
		InformerFactory:  clients["csi"].informers,
		VolumesDirectory: &s.c.Root.Data.Volumes,
	}

	clusternet := clusternet.Service{
		NodeName:   s.c.NodeID,
		Kubernetes: clients["netserv"].client,
		Prefixes:   s.c.PodNetwork,
	}

	nfproxy := nfproxy.Service{
		ClusterCIDR: s.c.ClusterNet,
		ClientSet:   clients["netserv"].client,
	}

	npc := networkpolicy.Service{
		Kubernetes: clients["netserv"].client,
	}

	var dnsIPRanges []netip.Prefix
	for _, ipNet := range []net.IPNet{s.c.ServiceIPRange, s.c.ClusterNet} {
		ipPrefix, err := netip.ParsePrefix(ipNet.String())
		if err != nil {
			return fmt.Errorf("invalid IP range %s", ipNet)
		}
		dnsIPRanges = append(dnsIPRanges, ipPrefix)
	}
	dnsService := kubernetesDNS.New(s.c.ClusterDomain, dnsIPRanges)
	dnsService.ClientSet = clients["netserv"].client
	// Set the DNS handler. When the node has no Kubernetes Worker role,
	// or the role is removed, this is set to an empty handler in
	// //metropolis/node/core/roleserve/worker_kubernetes.go.
	s.c.Network.DNS.SetHandler("kubernetes", dnsService)

	if err := s.c.Network.AddLoopbackIP(allocs.IPContainerDNS); err != nil {
		return fmt.Errorf("failed to add local IP for container DNS: %w", err)
	}
	defer func() {
		if err := s.c.Network.ReleaseLoopbackIP(allocs.IPContainerDNS); err != nil {
			supervisor.Logger(ctx).Errorf("Failed to release local IP for container DNS: %v", err)
		}
	}()
	runDNSListener := func(ctx context.Context) error {
		return s.c.Network.DNS.RunListenerAddr(ctx, net.JoinHostPort(allocs.IPContainerDNS.String(), "53"))
	}

	kvmDevicePlugin := kvmdevice.Plugin{
		KubeletDirectory: &s.c.Root.Data.Kubernetes.Kubelet,
	}

	for _, sub := range []struct {
		name     string
		runnable supervisor.Runnable
	}{
		{"csi-plugin", csiPlugin.Run},
		{"csi-provisioner", csiProvisioner.Run},
		{"clusternet", clusternet.Run},
		{"nfproxy", nfproxy.Run},
		{"dns-service", dnsService.Run},
		{"dns-listener", runDNSListener},
		{"npc", npc.Run},
		{"kvmdeviceplugin", kvmDevicePlugin.Run},
		{"kubelet", kubelet.Run},
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

func connectByKubeconfig(kubeconfig []byte) (*kubernetes.Clientset, informers.SharedInformerFactory, error) {
	rawClientConfig, err := clientcmd.NewClientConfigFromBytes(kubeconfig)
	if err != nil {
		return nil, nil, fmt.Errorf("could not generate kubernetes client config: %w", err)
	}
	clientConfig, err := rawClientConfig.ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("could not fetch generate client config: %w", err)
	}
	clientSet, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("could not generate kubernetes client: %w", err)
	}
	informerFactory := informers.NewSharedInformerFactory(clientSet, 5*time.Minute)
	return clientSet, informerFactory, nil
}
