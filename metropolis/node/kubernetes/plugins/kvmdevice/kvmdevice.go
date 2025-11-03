// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Package kvmdevice implements a Kubernetes device plugin for the virtual KVM
// device. Using the device plugin API allows us to take advantage of the
// scheduler to locate pods on machines eligible for KVM and also allows
// granular access control to KVM using quotas instead of needing privileged
// access.
// Since KVM devices are virtual, this plugin emulates a huge number of them so
// that we never run out.
package kvmdevice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	deviceplugin "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	pluginregistration "k8s.io/kubelet/pkg/apis/pluginregistration/v1"

	"source.monogon.dev/go/logging"
	"source.monogon.dev/metropolis/node/core/localstorage"
	"source.monogon.dev/osbase/supervisor"
)

// Name is the name of the KVM devices this plugin exposes
var Name corev1.ResourceName = "devices.monogon.dev/kvm"

type Plugin struct {
	deviceplugin.UnimplementedDevicePluginServer
	pluginregistration.UnimplementedRegistrationServer
	KubeletDirectory *localstorage.DataKubernetesKubeletDirectory

	logger logging.Leveled
}

func (k *Plugin) GetInfo(context.Context, *pluginregistration.InfoRequest) (*pluginregistration.PluginInfo, error) {
	return &pluginregistration.PluginInfo{
		Type:              pluginregistration.DevicePlugin,
		Name:              string(Name),
		Endpoint:          k.KubeletDirectory.Plugins.KVM.FullPath(),
		SupportedVersions: []string{"v1beta1"},
	}, nil
}

func (k *Plugin) NotifyRegistrationStatus(ctx context.Context, req *pluginregistration.RegistrationStatus) (*pluginregistration.RegistrationStatusResponse, error) {
	if !req.PluginRegistered {
		k.logger.Errorf("KVM plugin failed to register: %v", req.Error)
	}
	return &pluginregistration.RegistrationStatusResponse{}, nil
}

func (k *Plugin) GetDevicePluginOptions(context.Context, *deviceplugin.Empty) (*deviceplugin.DevicePluginOptions, error) {
	return &deviceplugin.DevicePluginOptions{
		GetPreferredAllocationAvailable: false,
		PreStartRequired:                false,
	}, nil
}

func (k *Plugin) ListAndWatch(req *deviceplugin.Empty, s deviceplugin.DevicePlugin_ListAndWatchServer) error {
	var devs []*deviceplugin.Device

	// TODO(T963): Get this value from Kubelet configuration (or something higher-level?)
	for i := 0; i < 256; i++ {
		devs = append(devs, &deviceplugin.Device{
			ID:     fmt.Sprintf("kvm%v", i),
			Health: deviceplugin.Healthy,
		})
	}

	s.Send(&deviceplugin.ListAndWatchResponse{Devices: devs})

	<-s.Context().Done()
	return nil
}

func (k *Plugin) Allocate(ctx context.Context, req *deviceplugin.AllocateRequest) (*deviceplugin.AllocateResponse, error) {
	var response deviceplugin.AllocateResponse

	for _, req := range req.ContainerRequests {
		var devices []*deviceplugin.DeviceSpec
		for range req.DevicesIds {
			dev := new(deviceplugin.DeviceSpec)
			dev.HostPath = "/dev/kvm"
			dev.ContainerPath = "/dev/kvm"
			dev.Permissions = "rw"
			devices = append(devices, dev)
		}
		response.ContainerResponses = append(response.ContainerResponses, &deviceplugin.ContainerAllocateResponse{
			Devices: devices})
	}

	return &response, nil
}

// deviceNumberFromString gets a Linux device number from a string containing
// two decimal numbers representing the major and minor device numbers
// separated by a colon. Whitespace is ignored.
func deviceNumberFromString(s string) (uint64, error) {
	kvmDevParts := strings.Split(s, ":")
	if len(kvmDevParts) != 2 {
		return 0, fmt.Errorf("device file spec contains an invalid number of colons: `%v`", s)
	}
	major, err := strconv.ParseUint(strings.TrimSpace(kvmDevParts[0]), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("failed to convert major number to an integer: %w", err)
	}
	minor, err := strconv.ParseUint(strings.TrimSpace(kvmDevParts[1]), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("failed to convert minor number to an integer: %w", err)
	}

	return unix.Mkdev(uint32(major), uint32(minor)), nil
}

func (k *Plugin) Run(ctx context.Context) error {
	k.logger = supervisor.Logger(ctx)

	l1tfStatus, err := os.ReadFile("/sys/devices/system/cpu/vulnerabilities/l1tf")
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to query for CPU vulnerabilities: %w", err)
	}

	if bytes.Contains(l1tfStatus, []byte("vulnerable")) {
		k.logger.Warning("CPU is vulnerable to L1TF, not exposing KVM.")
		supervisor.Signal(ctx, supervisor.SignalHealthy)
		supervisor.Signal(ctx, supervisor.SignalDone)
		return nil
	}

	kvmDevRaw, err := os.ReadFile("/sys/devices/virtual/misc/kvm/dev")
	if err != nil {
		k.logger.Warning("KVM is not available. Check firmware settings and CPU.")
		supervisor.Signal(ctx, supervisor.SignalHealthy)
		supervisor.Signal(ctx, supervisor.SignalDone)
		//nolint:returnerrcheck
		return nil
	}

	kvmDevNode, err := deviceNumberFromString(string(kvmDevRaw))
	if err != nil {
		return fmt.Errorf("failed to parse KVM device node: %w", err)
	}

	err = unix.Mknod("/dev/kvm", 0666, int(kvmDevNode))
	if err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("failed to create KVM device node: %w", err)
	}
	err = unix.Chmod("/dev/kvm", 0666)
	if err != nil {
		return fmt.Errorf("failed to set KVM device node permissions: %w", err)
	}

	// Try to remove socket if an unclean shutdown happened
	os.Remove(k.KubeletDirectory.Plugins.KVM.FullPath())

	pluginListener, err := net.ListenUnix("unix", &net.UnixAddr{Name: k.KubeletDirectory.Plugins.KVM.FullPath(), Net: "unix"})
	if err != nil {
		return fmt.Errorf("failed to listen on device plugin socket: %w", err)
	}

	pluginServer := grpc.NewServer()
	deviceplugin.RegisterDevicePluginServer(pluginServer, k)
	if err := supervisor.Run(ctx, "kvm-device", supervisor.GRPCServer(pluginServer, pluginListener, false)); err != nil {
		return err
	}

	// Try to remove socket if an unclean shutdown happened
	os.Remove(k.KubeletDirectory.PluginsRegistry.KVMReg.FullPath())

	registrationListener, err := net.ListenUnix("unix", &net.UnixAddr{Name: k.KubeletDirectory.PluginsRegistry.KVMReg.FullPath(), Net: "unix"})
	if err != nil {
		return fmt.Errorf("failed to listen on registration socket: %w", err)
	}

	registrationServer := grpc.NewServer()
	pluginregistration.RegisterRegistrationServer(registrationServer, k)
	if err := supervisor.Run(ctx, "registration", supervisor.GRPCServer(registrationServer, registrationListener, true)); err != nil {
		return err
	}
	supervisor.Signal(ctx, supervisor.SignalHealthy)
	supervisor.Signal(ctx, supervisor.SignalDone)
	return nil
}
