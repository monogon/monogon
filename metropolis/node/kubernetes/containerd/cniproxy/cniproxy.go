// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Package cni implements an adapter between the go-cni interface and
// the Monogon gRPC Workload Attachment interface. As we do not intend to
// actually implement a CNI-compliant plugin it makes more sense to just cut
// out as much unnecessary logic and take over at the containerd API boundary.
package cni

import (
	"context"
	"fmt"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	wlapi "source.monogon.dev/metropolis/node/core/network/workloads/spec"
)

func New(_ ...Opt) (CNI, error) {
	conn, err := grpc.NewClient("unix:/ephemeral/workloadnet.sock", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		panic(err)
	}
	wlClient := wlapi.NewWorkloadNetworkingClient(conn)
	return &adapter{
		client: wlClient,
	}, nil
}

type NamespaceOpts func(n *Namespace) error

// Namespace differs significantly from upstream as we do not have the actual
// underlying CNI interface and thus we do not need to transform the data into
// JSON keys.
type Namespace struct {
	labels      map[string]string
	annotations map[string]string
	portMapping []PortMapping
	bandwidth   BandWidth
	dns         DNS
	cgroupPath  string
}

func WithLabels(labels map[string]string) NamespaceOpts {
	return func(n *Namespace) error {
		n.labels = labels
		return nil
	}
}

func WithCapability(name string, capability interface{}) NamespaceOpts {
	return func(n *Namespace) error {
		if name == "io.kubernetes.cri.pod-annotations" {
			n.annotations = capability.(map[string]string)
		}
		return nil
	}
}

func WithCapabilityPortMap(portMapping []PortMapping) NamespaceOpts {
	return func(c *Namespace) error {
		c.portMapping = portMapping
		return nil
	}
}

func WithCapabilityBandWidth(bandWidth BandWidth) NamespaceOpts {
	return func(c *Namespace) error {
		c.bandwidth = bandWidth
		return nil
	}
}

func WithCapabilityDNS(dns DNS) NamespaceOpts {
	return func(c *Namespace) error {
		c.dns = dns
		return nil
	}
}

func WithCapabilityCgroupPath(cgroupPath string) NamespaceOpts {
	return func(c *Namespace) error {
		c.cgroupPath = cgroupPath
		return nil
	}
}

type adapter struct {
	client wlapi.WorkloadNetworkingClient
}

func (s *adapter) Setup(ctx context.Context, id string, path string, opts ...NamespaceOpts) (*Result, error) {
	var n Namespace
	for _, opt := range opts {
		opt(&n)
	}
	res, err := s.client.Attach(ctx, &wlapi.AttachRequest{
		WorkloadId: n.labels["K8S_POD_UID"],
		Netns: &wlapi.NetNSAttachment{
			NetnsPath: path,
			IfName:    "eth0",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("while requesting workload network attachment: %w", err)
	}
	// Provide IP to containerd/CRI, rest is ignored anyways.
	var ipConfigs []*IPConfig
	for _, ip := range res.Ip {
		ipConfigs = append(ipConfigs, &IPConfig{IP: net.IP(ip)})
	}
	return &Result{
		Interfaces: map[string]*Config{
			"eth0": {
				IPConfigs: ipConfigs,
			},
		},
	}, nil
}

func (s *adapter) SetupSerially(ctx context.Context, id string, path string, opts ...NamespaceOpts) (*Result, error) {
	// We do not support multiple plugins, the distinction between serial or
	// parallel does not exist. Just forward the call.
	return s.Setup(ctx, id, path, opts...)
}

func (s *adapter) Remove(ctx context.Context, id string, path string, opts ...NamespaceOpts) error {
	var n Namespace
	for _, opt := range opts {
		opt(&n)
	}

	_, err := s.client.Detach(ctx, &wlapi.DetachRequest{
		WorkloadId: n.labels["K8S_POD_UID"],
		Netns: &wlapi.NetNSAttachment{
			NetnsPath: path,
			IfName:    "eth0",
		},
	})
	return err
}

func (s *adapter) Check(ctx context.Context, id string, path string, opts ...NamespaceOpts) error {
	return nil
}

func (s *adapter) Load(opts ...Opt) error {
	// Stub, we do not actually have any CNI config.
	return nil
}

func (s *adapter) Status() error {
	_, err := s.client.Status(context.Background(), &wlapi.StatusRequest{})
	return err
}

func (s *adapter) GetConfig() *ConfigResult {
	return &ConfigResult{}
}
