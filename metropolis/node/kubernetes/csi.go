// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
	pluginregistration "k8s.io/kubelet/pkg/apis/pluginregistration/v1"

	"source.monogon.dev/go/logging"
	"source.monogon.dev/metropolis/node/core/localstorage"
	"source.monogon.dev/osbase/fsquota"
	"source.monogon.dev/osbase/loop"
	"source.monogon.dev/osbase/supervisor"
)

// Derived from K8s spec for acceptable names, but shortened to 130 characters
// to avoid issues with maximum path length. We don't provision longer names so
// this applies only if you manually create a volume with a name of more than
// 130 characters.
var acceptableNames = regexp.MustCompile("^[a-z][a-z0-9-.]{0,128}[a-z0-9]$")

type csiPluginServer struct {
	*csi.UnimplementedNodeServer
	KubeletDirectory *localstorage.DataKubernetesKubeletDirectory
	VolumesDirectory *localstorage.DataVolumesDirectory

	logger logging.Leveled
}

func (s *csiPluginServer) Run(ctx context.Context) error {
	s.logger = supervisor.Logger(ctx)

	// Try to remove socket if an unclean shutdown happened.
	os.Remove(s.KubeletDirectory.Plugins.VFS.FullPath())

	pluginListener, err := net.ListenUnix("unix", &net.UnixAddr{Name: s.KubeletDirectory.Plugins.VFS.FullPath(), Net: "unix"})
	if err != nil {
		return fmt.Errorf("failed to listen on CSI socket: %w", err)
	}

	pluginServer := grpc.NewServer()
	csi.RegisterIdentityServer(pluginServer, s)
	csi.RegisterNodeServer(pluginServer, s)
	// Enable graceful shutdown since we don't have long-running RPCs and most
	// of them shouldn't and can't be cancelled anyways.
	if err := supervisor.Run(ctx, "csi-node", supervisor.GRPCServer(pluginServer, pluginListener, true)); err != nil {
		return err
	}

	r := pluginRegistrationServer{
		regErr:           make(chan error, 1),
		KubeletDirectory: s.KubeletDirectory,
	}

	if err := supervisor.Run(ctx, "registration", r.Run); err != nil {
		return err
	}
	supervisor.Signal(ctx, supervisor.SignalHealthy)
	supervisor.Signal(ctx, supervisor.SignalDone)
	return nil
}

func (s *csiPluginServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if !acceptableNames.MatchString(req.VolumeId) {
		return nil, status.Error(codes.InvalidArgument, "invalid characters in volume id")
	}

	// TODO(q3k): move this logic to localstorage?
	volumePath := filepath.Join(s.VolumesDirectory.FullPath(), req.VolumeId)

	switch req.VolumeCapability.AccessMode.Mode {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER:
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY:
	default:
		return nil, status.Error(codes.InvalidArgument, "unsupported access mode")
	}
	switch req.VolumeCapability.AccessType.(type) {
	case *csi.VolumeCapability_Mount:
		if err := os.MkdirAll(req.TargetPath, 0700); err != nil {
			return nil, status.Errorf(codes.Internal, "unable to create requested target path: %v", err)
		}

		err := unix.Mount(volumePath, req.TargetPath, "", unix.MS_BIND, "")
		switch {
		case errors.Is(err, unix.ENOENT):
			return nil, status.Error(codes.NotFound, "volume not found")
		case err != nil:
			return nil, status.Errorf(codes.Unavailable, "failed to bind-mount volume: %v", err)
		}

		var flags uintptr = unix.MS_REMOUNT | unix.MS_BIND
		if req.Readonly {
			flags |= unix.MS_RDONLY
		}
		if err := unix.Mount("", req.TargetPath, "", flags, ""); err != nil {
			_ = unix.Unmount(req.TargetPath, 0) // Best-effort
			return nil, status.Errorf(codes.Internal, "unable to set mount-point flags: %v", err)
		}
	case *csi.VolumeCapability_Block:
		f, err := os.OpenFile(volumePath, os.O_RDWR, 0)
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "failed to open block volume: %v", err)
		}
		defer f.Close()
		var flags uint32 = loop.FlagDirectIO
		if req.Readonly {
			flags |= loop.FlagReadOnly
		}
		loopdev, err := loop.Create(f, loop.Config{Flags: flags})
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "failed to create loop device: %v", err)
		}
		loopdevNum, err := loopdev.Dev()
		if err != nil {
			loopdev.Remove()
			return nil, status.Errorf(codes.Internal, "device number not available: %v", err)
		}
		if err := unix.Mknod(req.TargetPath, unix.S_IFBLK|0640, int(loopdevNum)); err != nil {
			loopdev.Remove()
			return nil, status.Errorf(codes.Unavailable, "failed to create device node at target path: %v", err)
		}
		loopdev.Close()
	default:
		return nil, status.Error(codes.InvalidArgument, "unsupported access type")
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (s *csiPluginServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	loopdev, err := loop.Open(req.TargetPath)
	if err == nil {
		defer loopdev.Close()
		// We have a block device
		if err := loopdev.Remove(); err != nil {
			return nil, status.Errorf(codes.Unavailable, "failed to remove loop device: %v", err)
		}
		if err := os.Remove(req.TargetPath); err != nil && !os.IsNotExist(err) {
			return nil, status.Errorf(codes.Unavailable, "failed to remove device inode: %v", err)
		}
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}
	// Otherwise try a normal unmount
	if err := unix.Unmount(req.TargetPath, 0); err != nil {
		return nil, status.Errorf(codes.Unavailable, "failed to unmount volume: %v", err)
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (*csiPluginServer) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	quota, err := fsquota.GetQuota(req.VolumePath)
	if os.IsNotExist(err) {
		return nil, status.Error(codes.NotFound, "volume does not exist at this path")
	} else if err != nil {
		return nil, status.Errorf(codes.Unavailable, "failed to get quota: %v", err)
	}

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Total:     int64(quota.Bytes),
				Unit:      csi.VolumeUsage_BYTES,
				Used:      int64(quota.BytesUsed),
				Available: int64(quota.Bytes - quota.BytesUsed),
			},
			{
				Total:     int64(quota.Inodes),
				Unit:      csi.VolumeUsage_INODES,
				Used:      int64(quota.InodesUsed),
				Available: int64(quota.Inodes - quota.InodesUsed),
			},
		},
	}, nil
}

func (s *csiPluginServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	info, err := os.Stat(req.VolumePath)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "failed to stat volume: %v", err)
	}
	if info.IsDir() {
		// Mount volume. Nothing to do here.
	} else {
		// Block volume.
		loopdev, err := loop.Open(req.VolumePath)
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "failed to open loop device: %v", err)
		}
		defer loopdev.Close()
		if err := loopdev.RefreshSize(); err != nil {
			return nil, status.Errorf(codes.Unavailable, "failed to refresh loop device size: %v", err)
		}
	}
	return &csi.NodeExpandVolumeResponse{CapacityBytes: req.CapacityRange.LimitBytes}, nil
}

func rpcCapability(cap csi.NodeServiceCapability_RPC_Type) *csi.NodeServiceCapability {
	return &csi.NodeServiceCapability{
		Type: &csi.NodeServiceCapability_Rpc{
			Rpc: &csi.NodeServiceCapability_RPC{Type: cap},
		},
	}
}

func (*csiPluginServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			rpcCapability(csi.NodeServiceCapability_RPC_EXPAND_VOLUME),
			rpcCapability(csi.NodeServiceCapability_RPC_GET_VOLUME_STATS),
		},
	}, nil
}

func (*csiPluginServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "failed to get node identity: %v", err)
	}
	return &csi.NodeGetInfoResponse{
		NodeId: hostname,
	}, nil
}

// CSI Identity endpoints
func (*csiPluginServer) GetPluginInfo(ctx context.Context, req *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{
		Name:          "dev.monogon.metropolis.vfs",
		VendorVersion: "0.0.1", // TODO(lorenz): Maybe stamp?
	}, nil
}

func (*csiPluginServer) GetPluginCapabilities(ctx context.Context, req *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{
				Type: &csi.PluginCapability_VolumeExpansion_{
					VolumeExpansion: &csi.PluginCapability_VolumeExpansion{
						Type: csi.PluginCapability_VolumeExpansion_ONLINE,
					},
				},
			},
		},
	}, nil
}

func (s *csiPluginServer) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{Ready: &wrapperspb.BoolValue{Value: true}}, nil
}

// pluginRegistrationServer implements the pluginregistration.Registration
// service. It has a special restart mechanic to accomodate a design issue
// in Kubelet which requires it to remove and recreate its gRPC socket for
// every new registration attempt.
type pluginRegistrationServer struct {
	pluginregistration.UnimplementedRegistrationServer

	// regErr has a buffer of 1, so that at least one error can always be
	// sent into it in a non-blocking way. There is a race if
	// NotifyRegistrationStatus is called twice with an error as the buffered
	// item might have been received but not fully processed yet.
	// As distinguishing between calls on different socket iterations is
	// hard, doing it this way errs on the side of caution, i.e.
	// generating too many restarts. This way is better as if we miss one
	// such error the registration will not be available until the node
	// gets restarted.
	regErr chan error

	KubeletDirectory *localstorage.DataKubernetesKubeletDirectory
}

func (r *pluginRegistrationServer) Run(ctx context.Context) error {
	// Remove registration socket if it exists
	os.Remove(r.KubeletDirectory.PluginsRegistry.VFSReg.FullPath())

	registrationListener, err := net.ListenUnix("unix", &net.UnixAddr{Name: r.KubeletDirectory.PluginsRegistry.VFSReg.FullPath(), Net: "unix"})
	if err != nil {
		return fmt.Errorf("failed to listen on CSI registration socket: %w", err)
	}
	defer registrationListener.Close()

	grpcS := grpc.NewServer()
	pluginregistration.RegisterRegistrationServer(grpcS, r)

	supervisor.Run(ctx, "rpc", supervisor.GRPCServer(grpcS, registrationListener, true))
	supervisor.Signal(ctx, supervisor.SignalHealthy)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err = <-r.regErr:
		return err
	}
}

func (r *pluginRegistrationServer) GetInfo(ctx context.Context, req *pluginregistration.InfoRequest) (*pluginregistration.PluginInfo, error) {
	return &pluginregistration.PluginInfo{
		Type:              pluginregistration.CSIPlugin,
		Name:              "dev.monogon.metropolis.vfs",
		Endpoint:          r.KubeletDirectory.Plugins.VFS.FullPath(),
		SupportedVersions: []string{"1.2"}, // Keep in sync with container-storage-interface/spec package version
	}, nil
}

func (r *pluginRegistrationServer) NotifyRegistrationStatus(ctx context.Context, req *pluginregistration.RegistrationStatus) (*pluginregistration.RegistrationStatusResponse, error) {
	if !req.PluginRegistered {
		select {
		case r.regErr <- fmt.Errorf("registration failed: %v", req.Error):
		default:
		}
	}
	return &pluginregistration.RegistrationStatusResponse{}, nil
}
