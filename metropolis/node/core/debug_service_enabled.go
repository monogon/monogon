// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"

	ctr "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"source.monogon.dev/metropolis/node/allocs"
	"source.monogon.dev/metropolis/node/core/localstorage"
	"source.monogon.dev/metropolis/node/core/mgmt"
	"source.monogon.dev/metropolis/node/core/roleserve"
	"source.monogon.dev/osbase/logtree"
	"source.monogon.dev/osbase/supervisor"

	apb "source.monogon.dev/metropolis/proto/api"
)

const (
	logFilterMax = 1000
)

// runDebugService runs the debug service if this is a debug build. Otherwise
// it does nothing.
func runDebugService(ctx context.Context, rs *roleserve.Service, lt *logtree.LogTree, root *localstorage.Root) error {
	// This code is included in the debug build, so start the debug service.
	supervisor.Logger(ctx).Warningf("YOU ARE RUNNING A DEBUG VERSION OF METROPOLIS. THIS IS UNSAFE.")
	supervisor.Logger(ctx).Warningf("ANYONE WITH ACCESS TO THE MANAGEMENT ADDRESS OF THIS NODE CAN FULLY TAKE OVER THE CLUSTER, WITHOUT AUTHENTICATING.")
	supervisor.Logger(ctx).Infof("Starting debug service...")
	dbg := &debugService{
		roleserve:       rs,
		logtree:         lt,
		traceLock:       make(chan struct{}, 1),
		ephemeralVolume: &root.Ephemeral.Containerd,
	}
	dbgSrv := grpc.NewServer()
	apb.RegisterNodeDebugServiceServer(dbgSrv, dbg)
	dbgLis, err := net.Listen("tcp", fmt.Sprintf(":%d", allocs.PortDebugService))
	if err != nil {
		return fmt.Errorf("failed to listen on debug service: %w", err)
	}
	if err := supervisor.Run(ctx, "debug", supervisor.GRPCServer(dbgSrv, dbgLis, false)); err != nil {
		return fmt.Errorf("failed to start debug service: %w", err)
	}
	return nil
}

// debugService implements the Metropolis node debug API.
type debugService struct {
	roleserve       *roleserve.Service
	logtree         *logtree.LogTree
	ephemeralVolume *localstorage.EphemeralContainerdDirectory

	// traceLock provides exclusive access to the Linux tracing infrastructure
	// (ftrace)
	// This is a channel because Go's mutexes can't be cancelled or be acquired
	// in a non-blocking way.
	traceLock chan struct{}
}

func (s *debugService) GetDebugKubeconfig(ctx context.Context, req *apb.GetDebugKubeconfigRequest) (*apb.GetDebugKubeconfigResponse, error) {
	w := s.roleserve.KubernetesStatus.Watch()
	defer w.Close()
	for {
		v, err := w.Get(ctx)
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "could not get kubernetes status: %v", err)
		}
		if v.Controller == nil {
			continue
		}
		return v.Controller.GetDebugKubeconfig(ctx, req)
	}
}

func (s *debugService) Logs(req *apb.LogsRequest, srv apb.NodeDebugService_LogsServer) error {
	svc := mgmt.LogService{
		LogTree: s.logtree,
	}
	return svc.Logs(req, srv)
}

// Validate property names as they are used in path construction and we really
// don't want a path traversal vulnerability
var safeTracingPropertyNamesRe = regexp.MustCompile("^[a-z0-9_]+$")

func writeTracingProperty(name string, value string) error {
	if !safeTracingPropertyNamesRe.MatchString(name) {
		return fmt.Errorf("disallowed tracing property name received: \"%v\"", name)
	}
	return os.WriteFile("/sys/kernel/tracing/"+name, []byte(value+"\n"), 0)
}

func (s *debugService) Trace(req *apb.TraceRequest, srv apb.NodeDebugService_TraceServer) error {
	// Don't allow more than one trace as the kernel doesn't support this.
	select {
	case s.traceLock <- struct{}{}:
		defer func() {
			<-s.traceLock
		}()
	default:
		return status.Error(codes.FailedPrecondition, "a trace is already in progress")
	}

	if len(req.FunctionFilter) == 0 {
		req.FunctionFilter = []string{"*"} // For reset purposes
	}
	if len(req.GraphFunctionFilter) == 0 {
		req.GraphFunctionFilter = []string{"*"} // For reset purposes
	}

	defer writeTracingProperty("current_tracer", "nop")
	if err := writeTracingProperty("current_tracer", req.Tracer); err != nil {
		return status.Errorf(codes.InvalidArgument, "requested tracer not available: %v", err)
	}

	if err := writeTracingProperty("set_ftrace_filter", strings.Join(req.FunctionFilter, " ")); err != nil {
		return status.Errorf(codes.InvalidArgument, "setting ftrace filter failed: %v", err)
	}
	if err := writeTracingProperty("set_graph_function", strings.Join(req.GraphFunctionFilter, " ")); err != nil {
		return status.Errorf(codes.InvalidArgument, "setting graph filter failed: %v", err)
	}
	tracePipe, err := os.Open("/sys/kernel/tracing/trace_pipe")
	if err != nil {
		return status.Errorf(codes.Unavailable, "cannot open trace output pipe: %v", err)
	}
	defer tracePipe.Close()

	defer writeTracingProperty("tracing_on", "0")
	if err := writeTracingProperty("tracing_on", "1"); err != nil {
		return status.Errorf(codes.InvalidArgument, "requested tracer not available: %v", err)
	}

	go func() {
		<-srv.Context().Done()
		tracePipe.Close()
	}()

	eventScanner := bufio.NewScanner(tracePipe)
	for eventScanner.Scan() {
		if err := eventScanner.Err(); err != nil {
			return status.Errorf(codes.Unavailable, "event pipe read error: %v", err)
		}
		err := srv.Send(&apb.TraceEvent{
			RawLine: eventScanner.Text(),
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// imageReader is an adapter converting a gRPC stream into an io.Reader
type imageReader struct {
	srv        apb.NodeDebugService_LoadImageServer
	restOfPart []byte
}

func (i *imageReader) Read(p []byte) (n int, err error) {
	n1 := copy(p, i.restOfPart)
	if len(p) > len(i.restOfPart) {
		part, err := i.srv.Recv()
		if err != nil {
			return n1, err
		}
		n2 := copy(p[n1:], part.DataPart)
		i.restOfPart = part.DataPart[n2:]
		return n1 + n2, nil
	} else {
		i.restOfPart = i.restOfPart[n1:]
		return n1, nil
	}
}

// LoadImage loads an OCI image into the image cache of this node
func (s *debugService) LoadImage(srv apb.NodeDebugService_LoadImageServer) error {
	client, err := ctr.New(s.ephemeralVolume.ClientSocket.FullPath())
	if err != nil {
		return status.Errorf(codes.Unavailable, "failed to connect to containerd: %v", err)
	}
	ctxWithNS := namespaces.WithNamespace(srv.Context(), "k8s.io")
	reader := &imageReader{srv: srv}
	_, err = client.Import(ctxWithNS, reader)
	if err != nil {
		return status.Errorf(codes.Unknown, "failed to import image: %v", err)
	}
	return srv.SendAndClose(&apb.LoadImageResponse{})
}
