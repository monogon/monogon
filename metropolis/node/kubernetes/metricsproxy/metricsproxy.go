// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Package metricsproxy implements an authenticating proxy in front of the K8s
// controller-manager and scheduler providing unauthenticated access to the
// metrics via local ports
package metricsproxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"

	"k8s.io/kubernetes/cmd/kubeadm/app/constants"

	"source.monogon.dev/metropolis/node/allocs"
	"source.monogon.dev/metropolis/node/kubernetes/pki"
	"source.monogon.dev/osbase/supervisor"
)

type Service struct {
	// KPKI is a reference to the Kubernetes PKI
	KPKI *pki.PKI
}

type kubernetesExporter struct {
	Name string
	// TargetPort on which this exporter is running.
	TargetPort allocs.Port
	// TargetPort on which the unauthenticated exporter should run.
	ListenPort allocs.Port
	// ServerName used to verify the tls connection.
	ServerName string
}

// services are the kubernetes services which are exposed via this proxy.
var services = []*kubernetesExporter{
	{
		Name:       "kubernetes-scheduler",
		TargetPort: constants.KubeSchedulerPort,
		ListenPort: allocs.PortMetricsKubeSchedulerListener,
		ServerName: "kube-scheduler.local",
	},
	{
		Name:       "kubernetes-controller-manager",
		TargetPort: constants.KubeControllerManagerPort,
		ListenPort: allocs.PortMetricsKubeControllerManagerListener,
		ServerName: "kube-controller-manager.local",
	},
	{
		Name:       "kubernetes-apiserver",
		TargetPort: allocs.PortKubernetesAPI,
		ListenPort: allocs.PortMetricsKubeAPIServerListener,
		ServerName: "kubernetes",
	},
}

type metricsService struct {
	*kubernetesExporter
	transport *http.Transport
}

func (s *metricsService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, fmt.Sprintf("method %q not allowed", r.Method), http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	// We are supplying the http.Server with a BaseContext that contains the
	// context from our runnable which contains the logger
	logger := supervisor.Logger(ctx)

	url := "https://127.0.0.1:" + s.TargetPort.PortString() + "/metrics"
	outReq, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		logger.Errorf("%s: forwarding to %q failed: %v", r.RemoteAddr, s.Name, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	res, err := s.transport.RoundTrip(outReq)
	if err != nil {
		logger.Errorf("%s: forwarding to %q failed: %v", r.RemoteAddr, s.Name, err)
		http.Error(w, "could not reach endpoint", http.StatusBadGateway)
		return
	}
	defer res.Body.Close()

	copyHeader(w.Header(), res.Header)
	w.WriteHeader(res.StatusCode)

	if _, err := io.Copy(w, res.Body); err != nil {
		logger.Errorf("%s: copying response from %q failed: %v", r.RemoteAddr, s.Name, err)
		return
	}
}

func (s *metricsService) Run(ctx context.Context) error {
	supervisor.Signal(ctx, supervisor.SignalHealthy)

	srv := http.Server{
		BaseContext: func(_ net.Listener) context.Context {
			return ctx
		},
		Addr:    net.JoinHostPort("127.0.0.1", s.ListenPort.PortString()),
		Handler: s,
	}

	go func() {
		<-ctx.Done()
		srv.Close()
	}()

	err := srv.ListenAndServe()
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("ListenAndServe: %w", err)
}

func (s *Service) Run(ctx context.Context) error {
	// TODO(q3k): move this to IssueCertificates and replace with dedicated certificate
	cert, key, err := s.KPKI.Certificate(ctx, pki.Master)
	if err != nil {
		return fmt.Errorf("could not load certificate %q from PKI: %w", pki.Master, err)
	}
	parsedKey, err := x509.ParsePKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("failed to parse key for cert %q: %w", pki.Master, err)
	}

	caCert, _, err := s.KPKI.Certificate(ctx, pki.IdCA)
	if err != nil {
		return fmt.Errorf("could not load certificate %q from PKI: %w", pki.IdCA, err)
	}
	parsedCACert, err := x509.ParseCertificate(caCert)
	if err != nil {
		return fmt.Errorf("failed to parse cert %q: %w", pki.IdCA, err)
	}

	rootCAs := x509.NewCertPool()
	rootCAs.AddCert(parsedCACert)

	tlsConfig := &tls.Config{
		RootCAs: rootCAs,
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{cert},
			PrivateKey:  parsedKey,
		}},
	}

	for _, svc := range services {
		tlsConfig := tlsConfig.Clone()
		tlsConfig.ServerName = svc.ServerName

		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = tlsConfig
		transport.TLSClientConfig.ServerName = svc.ServerName

		err := supervisor.Run(ctx, svc.Name, (&metricsService{
			kubernetesExporter: svc,
			transport:          transport,
		}).Run)
		if err != nil {
			return fmt.Errorf("could not run sub-service %q: %w", svc.Name, err)
		}
	}

	supervisor.Signal(ctx, supervisor.SignalHealthy)

	<-ctx.Done()
	return ctx.Err()
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}
