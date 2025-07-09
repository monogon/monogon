// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	"source.monogon.dev/go/logging"
	"source.monogon.dev/metropolis/installer/install"
	"source.monogon.dev/osbase/blockdev"
	"source.monogon.dev/osbase/efivarfs"
	"source.monogon.dev/osbase/oci"
	"source.monogon.dev/osbase/oci/osimage"
	"source.monogon.dev/osbase/structfs"
)

//go:embed metropolis/node/abloader/abloader.efi
var abloader []byte

// EnvInstallTarget environment variable which tells the takeover binary where
// to install to
const EnvInstallTarget = "TAKEOVER_INSTALL_TARGET"

func installMetropolis(l logging.Leveled) error {
	// Validate we are running via EFI.
	if _, err := os.Stat("/sys/firmware/efi"); os.IsNotExist(err) {
		// nolint:ST1005
		return fmt.Errorf("Monogon OS can only be installed on EFI-booted machines, this one is not")
	}

	metropolisSpecRaw, err := os.ReadFile("/params.pb")
	if err != nil {
		return err
	}

	image, err := oci.ReadLayout("/osimage")
	if err != nil {
		return fmt.Errorf("failed to read OS image: %w", err)
	}

	installParams, err := setupOSImageParams(image, metropolisSpecRaw, os.Getenv(EnvInstallTarget))
	if err != nil {
		return err
	}

	be, err := install.Write(installParams)
	if err != nil {
		return fmt.Errorf("failed to apply installation: %w", err)
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

func setupOSImageParams(image *oci.Image, metropolisSpecRaw []byte, installTarget string) (*install.Params, error) {
	rootDev, err := blockdev.Open(filepath.Join("/dev", installTarget))
	if err != nil {
		return nil, fmt.Errorf("failed to open root device: %w", err)
	}

	osImage, err := osimage.Read(image)
	if err != nil {
		return nil, fmt.Errorf("failed to read OS image: %w", err)
	}

	return &install.Params{
		PartitionSize: install.PartitionSizeInfo{
			ESP:    384,
			System: 4096,
			Data:   128,
		},
		OSImage:        osImage,
		ABLoader:       structfs.Bytes(abloader),
		NodeParameters: structfs.Bytes(metropolisSpecRaw),
		Output:         rootDev,
	}, nil
}
