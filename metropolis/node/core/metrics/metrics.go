// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os/exec"

	"source.monogon.dev/metropolis/node/allocs"
	"source.monogon.dev/metropolis/node/core/identity"
	"source.monogon.dev/osbase/supervisor"
)

// Service is the Metropolis Metrics Service.
//
// Currently, metrics means Prometheus metrics.
//
// It runs a forwarding proxy from a public HTTPS listener to a number of
// locally-running exporters, themselves listening over HTTP. The listener uses
// the main cluster CA and the node's main certificate, authenticating incoming
// connections with the same CA.
//
// Each exporter is exposed on a separate path, /metrics/<name>, where <name> is
// the name of the exporter.
//
// The HTTPS listener is bound to allocs.PortMetrics.
type Service struct {
	// Credentials used to run the TLS/HTTPS listener and verify incoming
	// connections.
	Credentials *identity.NodeCredentials
	Discovery   Discovery

	// List of Exporters to run and to forward HTTP requests to. If not set, defaults
	// to DefaultExporters.
	Exporters []*Exporter
	// enableDynamicAddr enables listening on a dynamically chosen TCP port. This is
	// used by tests to make sure we don't fail due to the default port being already
	// in use.
	enableDynamicAddr bool

	// dynamicAddr will contain the picked dynamic listen address after the service
	// starts, if enableDynamicAddr is set.
	dynamicAddr chan string
}

// listen starts the public TLS listener for the service.
func (s *Service) listen() (net.Listener, error) {
	cert := s.Credentials.TLSCredentials()

	pool := x509.NewCertPool()
	pool.AddCert(s.Credentials.ClusterCA())

	tlsc := tls.Config{
		Certificates: []tls.Certificate{
			cert,
		},
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  pool,
		// TODO(q3k): use VerifyPeerCertificate/VerifyConnection to check that the
		// incoming client is allowed to access metrics. Currently we allow
		// anyone/anything with a valid cluster certificate to access them.
	}

	addr := net.JoinHostPort("", allocs.PortMetrics.PortString())
	if s.enableDynamicAddr {
		addr = ""
	}
	return tls.Listen("tcp", addr, &tlsc)
}

func (s *Service) Run(ctx context.Context) error {
	lis, err := s.listen()
	if err != nil {
		return fmt.Errorf("listen failed: %w", err)
	}
	if s.enableDynamicAddr {
		s.dynamicAddr <- lis.Addr().String()
	}

	if s.Exporters == nil {
		s.Exporters = DefaultExporters
	}

	// First, make sure we don't have duplicate exporters.
	seenNames := make(map[string]bool)
	for _, exporter := range s.Exporters {
		if seenNames[exporter.Name] {
			return fmt.Errorf("duplicate exporter name: %q", exporter.Name)
		}
		seenNames[exporter.Name] = true
	}

	// Start all exporters as sub-runnables.
	for _, exporter := range s.Exporters {
		exporter := exporter
		if exporter.Executable == "" {
			continue
		}

		err := supervisor.Run(ctx, exporter.Name, func(ctx context.Context) error {
			cmd := exec.CommandContext(ctx, exporter.Executable, exporter.Arguments...)
			return supervisor.RunCommand(ctx, cmd)
		})
		if err != nil {
			return fmt.Errorf("running %s failed: %w", exporter.Name, err)
		}
	}

	// And register all exporter forwarding functions on a mux.
	mux := http.NewServeMux()
	logger := supervisor.Logger(ctx)
	for _, exporter := range s.Exporters {
		mux.HandleFunc(exporter.externalPath(), exporter.ServeHTTP)

		logger.Infof("Registered exporter %q", exporter.Name)
	}

	// And register a http_sd discovery endpoint.
	mux.Handle("/discovery", &s.Discovery)

	supervisor.Signal(ctx, supervisor.SignalHealthy)

	// Start forwarding server.
	srv := http.Server{
		Handler: mux,
		BaseContext: func(_ net.Listener) context.Context {
			return ctx
		},
	}

	go func() {
		<-ctx.Done()
		srv.Close()
	}()

	err = srv.Serve(lis)
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("Serve(): %w", err)
}
