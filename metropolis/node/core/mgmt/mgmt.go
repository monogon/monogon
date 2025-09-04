// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Package mgmt implements the node-local management service, a.k.a.
// metropolis.proto.api.NodeManagement.
package mgmt

import (
	"context"
	"fmt"
	"net"
	"sync"

	"google.golang.org/grpc"

	"source.monogon.dev/metropolis/node/allocs"
	"source.monogon.dev/metropolis/node/core/identity"
	"source.monogon.dev/metropolis/node/core/rpc"
	"source.monogon.dev/metropolis/node/core/update"
	"source.monogon.dev/osbase/logtree"
	"source.monogon.dev/osbase/supervisor"

	apb "source.monogon.dev/metropolis/proto/api"
)

// Service implements metropolis.proto.api.NodeManagement.
type Service struct {
	// NodeCredentials used to set up gRPC server.
	NodeCredentials *identity.NodeCredentials
	// LogTree from which NodeManagement.Logs will be served.
	LogTree *logtree.LogTree
	// Update service handle for performing updates via the API.
	UpdateService *update.Service
	// Serialized UpdateNode RPCs
	updateMutex sync.Mutex

	// Automatically populated on Run.
	LogService
}

// Run the Servie as a supervisor runnable.
func (s *Service) Run(ctx context.Context) error {
	if s.NodeCredentials == nil {
		return fmt.Errorf("NodeCredentials missing")
	}
	if s.LogTree == nil {
		return fmt.Errorf("LogTree missing")
	}

	s.LogService.LogTree = s.LogTree

	sec := rpc.ServerSecurity{
		NodeCredentials: s.NodeCredentials,
	}
	logger := supervisor.MustSubLogger(ctx, "rpc")
	opts := sec.GRPCOptions(logger)
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", allocs.PortNodeManagement))
	if err != nil {
		return fmt.Errorf("failed to listen on node management socket socket: %w", err)
	}
	defer lis.Close()

	srv := grpc.NewServer(opts...)
	apb.RegisterNodeManagementServer(srv, s)

	runnable := supervisor.GRPCServer(srv, lis, false)
	if err := supervisor.Run(ctx, "server", runnable); err != nil {
		return fmt.Errorf("could not run server: %w", err)
	}
	supervisor.Signal(ctx, supervisor.SignalHealthy)
	<-ctx.Done()
	return ctx.Err()
}
