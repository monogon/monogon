// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cenkalti/backoff/v4"
	"google.golang.org/protobuf/proto"

	apb "source.monogon.dev/cloud/agent/api"
	npb "source.monogon.dev/osbase/net/proto"

	metropolisInstall "source.monogon.dev/metropolis/installer/install"
	"source.monogon.dev/osbase/blockdev"
	"source.monogon.dev/osbase/efivarfs"
	"source.monogon.dev/osbase/oci/osimage"
	"source.monogon.dev/osbase/oci/registry"
	"source.monogon.dev/osbase/structfs"
	"source.monogon.dev/osbase/supervisor"
)

//go:embed metropolis/node/abloader/abloader.efi
var abloader []byte

// install dispatches OSInstallationRequests to the appropriate installer
// method
func install(ctx context.Context, req *apb.OSInstallationRequest, netConfig *npb.Net) error {
	switch reqT := req.Type.(type) {
	case *apb.OSInstallationRequest_Metropolis:
		return installMetropolis(ctx, reqT.Metropolis, netConfig)
	default:
		return errors.New("unknown installation request type")
	}
}

func installMetropolis(ctx context.Context, req *apb.MetropolisInstallationRequest, netConfig *npb.Net) error {
	l := supervisor.Logger(ctx)
	// Validate we are running via EFI.
	if _, err := os.Stat("/sys/firmware/efi"); os.IsNotExist(err) {
		// nolint:ST1005
		return errors.New("Monogon OS can only be installed on EFI-booted machines, this one is not")
	}

	// Override the NodeParameters.NetworkConfig with the current NetworkConfig
	// if it's missing.
	if req.NodeParameters.NetworkConfig == nil {
		req.NodeParameters.NetworkConfig = netConfig
	}

	if req.OsImage == nil {
		return fmt.Errorf("missing OS image in OS installation request")
	}
	if req.OsImage.Digest == "" {
		return fmt.Errorf("missing digest in OS installation request")
	}

	client := &registry.Client{
		GetBackOff: func() backoff.BackOff {
			return backoff.NewExponentialBackOff()
		},
		RetryNotify: func(err error, d time.Duration) {
			l.Warningf("Error while fetching OS image, retrying in %v: %v", d, err)
		},
		UserAgent:  "Monogon-Cloud-Agent",
		Scheme:     req.OsImage.Scheme,
		Host:       req.OsImage.Host,
		Repository: req.OsImage.Repository,
	}

	image, err := client.Read(ctx, req.OsImage.Tag, req.OsImage.Digest)
	if err != nil {
		return fmt.Errorf("failed to fetch OS image: %w", err)
	}

	osImage, err := osimage.Read(image)
	if err != nil {
		return fmt.Errorf("failed to fetch OS image: %w", err)
	}

	l.Info("OS image config downloaded")

	nodeParamsRaw, err := proto.Marshal(req.NodeParameters)
	if err != nil {
		return fmt.Errorf("failed marshaling: %w", err)
	}

	rootDev, err := blockdev.Open(filepath.Join("/dev", req.RootDevice))
	if err != nil {
		return fmt.Errorf("failed to open root device: %w", err)
	}

	installParams := metropolisInstall.Params{
		PartitionSize: metropolisInstall.PartitionSizeInfo{
			ESP:    384,
			System: 4096,
			Data:   128,
		},
		OSImage:        osImage,
		ABLoader:       structfs.Bytes(abloader),
		NodeParameters: structfs.Bytes(nodeParamsRaw),
		Output:         rootDev,
	}

	be, err := metropolisInstall.Write(&installParams)
	if err != nil {
		return err
	}
	bootEntryIdx, err := efivarfs.AddBootEntry(be)
	if err != nil {
		return fmt.Errorf("error creating EFI boot entry: %w", err)
	}
	if err := efivarfs.SetBootOrder(efivarfs.BootOrder{uint16(bootEntryIdx)}); err != nil {
		return fmt.Errorf("error setting EFI boot order: %w", err)
	}
	l.Info("Metropolis installation completed")
	return nil
}
