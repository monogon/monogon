// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package core

import (
	"errors"
	"fmt"
	"math"
	"os"

	"google.golang.org/protobuf/proto"

	"source.monogon.dev/metropolis/installer/install"
	"source.monogon.dev/metropolis/proto/api"
	"source.monogon.dev/osbase/blockdev"
	"source.monogon.dev/osbase/fat32"
	"source.monogon.dev/osbase/gpt"
	"source.monogon.dev/osbase/oci"
	"source.monogon.dev/osbase/oci/osimage"
	"source.monogon.dev/osbase/structfs"
)

type MakeInstallerImageArgs struct {
	// Path to either a file or a disk which will contain the installer data.
	TargetPath string

	// Reader for the installer EFI executable. Mandatory.
	Installer structfs.Blob

	// Optional NodeParameters to be embedded for use by the installer.
	NodeParams *api.NodeParameters

	// OS image for use by the installer.
	Image *oci.Image
}

// MakeInstallerImage generates an installer disk image containing a Table
// partition table and a single FAT32 partition with an installer and optionally
// with an OS image and/or Node Parameters.
func MakeInstallerImage(args MakeInstallerImageArgs) error {
	if args.Installer == nil {
		return errors.New("installer is mandatory")
	}

	osImage, err := osimage.Read(args.Image)
	if err != nil {
		return fmt.Errorf("failed to read OS image: %w", err)
	}
	architecture := osImage.Config.ProductInfo.Architecture()
	bootPath, err := install.EFIBootPath(architecture)
	if err != nil {
		return err
	}

	var espRoot structfs.Tree

	if err := espRoot.PlaceFile(bootPath, args.Installer); err != nil {
		return err
	}

	if args.NodeParams != nil {
		nodeParamsRaw, err := proto.Marshal(args.NodeParams)
		if err != nil {
			return fmt.Errorf("failed to marshal node params: %w", err)
		}
		if err := espRoot.PlaceFile("metropolis-installer/nodeparams.pb", structfs.Bytes(nodeParamsRaw)); err != nil {
			return err
		}
	}
	imageLayout, err := oci.CreateLayout(args.Image)
	if err != nil {
		return err
	}
	if err := espRoot.PlaceDir("metropolis-installer/osimage", imageLayout); err != nil {
		return err
	}
	var targetDev blockdev.BlockDev
	targetDev, err = blockdev.Open(args.TargetPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			targetDev, err = blockdev.CreateFile(args.TargetPath, 512, 1024*1024+4096)
		}
		if err != nil {
			return fmt.Errorf("unable to open target device: %w", err)
		}
	}
	partTable, err := gpt.New(targetDev)
	if err != nil {
		return fmt.Errorf("target device has invalid geometry: %w", err)
	}
	if architecture == "x86_64" {
		partTable.BootCode = install.BootcodeX86
	}
	esp := gpt.Partition{
		Type: gpt.PartitionTypeEFISystem,
		Name: "MetropolisInstaller",
	}
	fatOpts := fat32.Options{Label: "METRO_INST"}
	// TODO(#254): Build and use dynamically-grown block devices
	var espSize int64 = 512 * 1024 * 1024
	if err := partTable.AddPartition(&esp, espSize); err != nil {
		return fmt.Errorf("unable to create partition layout: %w", err)
	}
	if esp.BlockSize() > math.MaxUint16 {
		return fmt.Errorf("block size (%d) too large for FAT32", esp.BlockSize())
	}
	fatOpts.BlockSize = uint16(esp.BlockSize())
	fatOpts.BlockCount = uint32(esp.BlockCount())
	if err := fat32.WriteFS(blockdev.NewRWS(esp), espRoot, fatOpts); err != nil {
		return fmt.Errorf("failed to write FAT32: %w", err)
	}
	if err := partTable.Write(); err != nil {
		return fmt.Errorf("unable to write partition table: %w", err)
	}
	return nil
}
