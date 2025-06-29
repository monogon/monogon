// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"

	"golang.org/x/net/proxy"
	"google.golang.org/grpc"

	"source.monogon.dev/metropolis/cli/metroctl/core"
	"source.monogon.dev/metropolis/node/core/rpc"
	"source.monogon.dev/metropolis/node/core/rpc/resolver"
)

func newAuthenticatedClient(ctx context.Context) (*grpc.ClientConn, error) {
	// Collect credentials, validate command parameters, and create the grpc
	// client.
	ocert, opkey, err := core.GetOwnerCredentials(flags.configPath)
	if errors.Is(err, core.ErrNoCredentials) {
		return nil, fmt.Errorf("you have to take ownership of the cluster first: %w", err)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get owner credentials: %w", err)
	}
	if len(flags.clusterEndpoints) == 0 {
		return nil, fmt.Errorf("please provide at least one cluster endpoint using the --endpoint parameter")
	}

	ca, err := core.GetClusterCAWithTOFU(ctx, connectOptions())
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster CA: %w", err)
	}

	tlsc := tls.Certificate{
		Certificate: [][]byte{ocert.Raw},
		PrivateKey:  opkey,
	}
	creds := rpc.NewAuthenticatedCredentials(tlsc, rpc.WantRemoteCluster(ca))
	opts, err := core.DialOpts(ctx, connectOptions())
	if err != nil {
		return nil, fmt.Errorf("while configuring dial options: %w", err)
	}
	opts = append(opts, grpc.WithTransportCredentials(creds))

	cc, err := grpc.NewClient(resolver.MetropolisControlAddress, opts...)
	if err != nil {
		return nil, fmt.Errorf("while creating client: %w", err)
	}
	return cc, nil
}

func newAuthenticatedNodeClient(ctx context.Context, id, address string, cacert *x509.Certificate) (*grpc.ClientConn, error) {
	// Collect credentials, validate command parameters, and create the grpc
	// client.
	ocert, opkey, err := core.GetOwnerCredentials(flags.configPath)
	if errors.Is(err, core.ErrNoCredentials) {
		return nil, fmt.Errorf("you have to take ownership of the cluster first: %w", err)
	}
	cc, err := core.NewNodeClient(ctx, opkey, ocert, cacert, flags.proxyAddr, id, address)
	if err != nil {
		return nil, fmt.Errorf("while creating client: %w", err)
	}
	return cc, nil
}

func newAuthenticatedNodeHTTPTransport(ctx context.Context, id string) (*http.Transport, error) {
	cacert, err := core.GetClusterCAWithTOFU(ctx, connectOptions())
	if err != nil {
		return nil, fmt.Errorf("could not get CA certificate: %w", err)
	}
	ocert, opkey, err := core.GetOwnerCredentials(flags.configPath)
	if errors.Is(err, core.ErrNoCredentials) {
		return nil, fmt.Errorf("you have to take ownership of the cluster first: %w", err)
	}
	tlsc := tls.Certificate{
		Certificate: [][]byte{ocert.Raw},
		PrivateKey:  opkey,
	}
	tlsconf := rpc.NewAuthenticatedTLSConfig(tlsc, rpc.WantRemoteCluster(cacert), rpc.WantRemoteNode(id))
	transport := &http.Transport{
		TLSClientConfig: tlsconf,
	}
	if flags.proxyAddr != "" {
		dialer, err := proxy.SOCKS5("tcp", flags.proxyAddr, nil, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("failed to create proxy dialer: %w", err)
		}
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			// proxy.SOCKS5 returns a Dialer that implements the DialContext
			// function, just doesn't exposes it.
			return dialer.(proxy.ContextDialer).DialContext(ctx, network, addr)
		}
	}
	return transport, nil
}
