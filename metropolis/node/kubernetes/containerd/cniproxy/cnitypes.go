// Copyright The Monogon Project Authors.
// Copyright The containerd Authors.
// SPDX-License-Identifier: Apache-2.0

package cni

// This file contains types mostly or entirely lifted from go-cni but copied
// here to allow API compatibility. Redefining these is not viable as their
// references to other types would point to go-cni's types.

import (
	"context"
	"net"

	"github.com/containernetworking/cni/pkg/types"
)

type CNI interface {
	// Setup setup the network for the namespace
	Setup(ctx context.Context, id string, path string, opts ...NamespaceOpts) (*Result, error)
	// SetupSerially sets up each of the network interfaces for the namespace in serial
	SetupSerially(ctx context.Context, id string, path string, opts ...NamespaceOpts) (*Result, error)
	// Remove tears down the network of the namespace.
	Remove(ctx context.Context, id string, path string, opts ...NamespaceOpts) error
	// Check checks if the network is still in desired state
	Check(ctx context.Context, id string, path string, opts ...NamespaceOpts) error
	// Load loads the cni network config
	Load(opts ...Opt) error
	// Status checks the status of the cni initialization
	Status() error
	// GetConfig returns a copy of the CNI plugin configurations as parsed by CNI
	GetConfig() *ConfigResult
}

type PortMapping struct {
	HostPort      int32
	ContainerPort int32
	Protocol      string
	HostIP        string
}

// BandWidth defines the ingress/egress rate and burst limits
type BandWidth struct {
	IngressRate  uint64
	IngressBurst uint64
	EgressRate   uint64
	EgressBurst  uint64
}

// DNS defines the dns config
type DNS struct {
	// List of DNS servers of the cluster.
	Servers []string
	// List of DNS search domains of the cluster.
	Searches []string
	// List of DNS options.
	Options []string
}

type IPConfig struct {
	IP      net.IP
	Gateway net.IP
}

type Config struct {
	IPConfigs  []*IPConfig
	Mac        string
	Sandbox    string
	PciID      string
	SocketPath string
}

type Result struct {
	Interfaces map[string]*Config
	DNS        []types.DNS
	Routes     []*types.Route
}

// ConfigResult is not used by containerd and it's a complex type, leave it
// for now.
type ConfigResult struct{}
