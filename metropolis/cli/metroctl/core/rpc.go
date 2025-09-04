// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"

	"golang.org/x/net/proxy"
	"google.golang.org/grpc"

	"source.monogon.dev/metropolis/node/allocs"
	"source.monogon.dev/metropolis/node/core/rpc"
	"source.monogon.dev/metropolis/node/core/rpc/resolver"
	"source.monogon.dev/metropolis/proto/api"
)

func DialOpts(ctx context.Context, c *ConnectOptions) ([]grpc.DialOption, error) {
	var opts []grpc.DialOption
	if c.ProxyServer != "" {
		socksDialer, err := proxy.SOCKS5("tcp", c.ProxyServer, nil, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("failed to build a SOCKS dialer: %w", err)
		}
		grpcd := func(_ context.Context, addr string) (net.Conn, error) {
			return socksDialer.Dial("tcp", addr)
		}
		opts = append(opts, grpc.WithContextDialer(grpcd))
	}

	var resolverOpts []resolver.ResolverOption
	if c.ResolverLogger != nil {
		resolverOpts = append(resolverOpts, resolver.WithLogger(c.ResolverLogger))
	}

	r := resolver.New(ctx, resolverOpts...)

	if len(c.Endpoints) == 0 {
		return nil, fmt.Errorf("no cluster endpoints specified")
	}
	for _, eps := range c.Endpoints {
		ep := resolver.NodeByHostPort(eps, uint16(allocs.PortCuratorService))
		r.AddEndpoint(ep)
	}
	opts = append(opts, grpc.WithResolvers(r))

	return opts, nil
}

func NewNodeClient(ctx context.Context, opkey ed25519.PrivateKey, ocert, ca *x509.Certificate, proxyAddr, nodeId, nodeAddr string) (*grpc.ClientConn, error) {
	var dialOpts []grpc.DialOption

	if opkey == nil {
		return nil, fmt.Errorf("an owner's private key must be provided")
	}
	if proxyAddr != "" {
		socksDialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("failed to build a SOCKS dialer: %w", err)
		}
		grpcd := func(_ context.Context, addr string) (net.Conn, error) {
			return socksDialer.Dial("tcp", addr)
		}
		dialOpts = append(dialOpts, grpc.WithContextDialer(grpcd))
	}
	tlsc := tls.Certificate{
		Certificate: [][]byte{ocert.Raw},
		PrivateKey:  opkey,
	}
	creds := rpc.NewAuthenticatedCredentials(tlsc, rpc.WantRemoteCluster(ca), rpc.WantRemoteNode(nodeId))
	dialOpts = append(dialOpts, grpc.WithTransportCredentials(creds))

	endpoint := net.JoinHostPort(nodeAddr, allocs.PortNodeManagement.PortString())
	return grpc.NewClient(endpoint, dialOpts...)
}

// GetNodes retrieves node records, filtered by the supplied node filter
// expression fexp.
func GetNodes(ctx context.Context, mgmt api.ManagementClient, fexp string) ([]*api.Node, error) {
	resN, err := mgmt.GetNodes(ctx, &api.GetNodesRequest{
		Filter: fexp,
	})
	if err != nil {
		return nil, err
	}

	var nodes []*api.Node
	for {
		node, err := resN.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}
