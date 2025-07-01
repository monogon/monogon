// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Package install allows planning and executing the installation of Metropolis
// to a block device.
package install

import (
	_ "embed"
	"fmt"
	"io"

	"github.com/google/uuid"

	"source.monogon.dev/osbase/blockdev"
	"source.monogon.dev/osbase/efivarfs"
	"source.monogon.dev/osbase/fat32"
	"source.monogon.dev/osbase/gpt"
	"source.monogon.dev/osbase/oci/osimage"
	"source.monogon.dev/osbase/structfs"
)

var (
	SystemAType = uuid.MustParse("ee96054b-f6d0-4267-aaaa-724b2afea74c")
	SystemBType = uuid.MustParse("ee96054b-f6d0-4267-bbbb-724b2afea74c")

	DataType = uuid.MustParse("9eeec464-6885-414a-b278-4305c51f7966")
)

const (
	SystemALabel = "METROPOLIS-SYSTEM-A"
	SystemBLabel = "METROPOLIS-SYSTEM-B"
	DataLabel    = "METROPOLIS-NODE-DATA"
	ESPLabel     = "ESP"

	EFIBootAPath   = "EFI/metropolis/boot-a.efi"
	EFIBootBPath   = "EFI/metropolis/boot-b.efi"
	nodeParamsPath = "metropolis/parameters.pb"
)

var EFIBootName = map[string]string{
	"x86_64":  "BOOTx64.EFI",
	"aarch64": "BOOTAA64.EFI",
}

// EFIBootPath returns the default file path according to the UEFI Specification
// v2.11 Section 3.5.1.1. This file is booted by any compliant UEFI firmware in
// absence of another bootable boot entry.
func EFIBootPath(architecture string) (string, error) {
	bootName, ok := EFIBootName[architecture]
	if !ok {
		return "", fmt.Errorf("unsupported architecture %q", architecture)
	}
	return "EFI/BOOT/" + bootName, nil
}

//go:embed metropolis/node/bios_bootcode/boot.bin
var BootcodeX86 []byte

// PartitionSizeInfo contains parameters used during partition table
// initialization and, in case of image files, space allocation.
type PartitionSizeInfo struct {
	// Size of the EFI System Partition (ESP), in mebibytes. The size must
	// not be zero.
	ESP int64
	// Size of the Metropolis system partition, in mebibytes. The partition
	// won't be created if the size is zero.
	System int64
	// Size of the Metropolis data partition, in mebibytes. The partition
	// won't be created if the size is zero. If the image is output to a
	// block device, the partition will be extended to fill the remaining
	// space.
	Data int64
}

// Params contains parameters used by Plan or Write to install Metropolis OS.
type Params struct {
	// Output is the block device to which the OS is installed.
	Output blockdev.BlockDev
	// OSImage is the image from which the OS is installed.
	OSImage *osimage.Image
	// UnverifiedPayloads disables verification of payloads if set.
	// This only works with uncompressed OS images.
	UnverifiedPayloads bool
	// ABLoader provides the A/B loader which then loads the EFI loader for the
	// correct slot.
	ABLoader structfs.Blob
	// NodeParameters provides contents of the node parameters file. If nil,
	// the node parameters file won't be created in the target ESP
	// filesystem.
	NodeParameters structfs.Blob
	// DiskGUID is a unique identifier of the image and a part of Table
	// header. It's optional and can be left blank if the identifier is
	// to be randomly generated. Setting it to a predetermined value can
	// help in implementing reproducible builds.
	DiskGUID uuid.UUID
	// PartitionSize specifies a size for the ESP, Metropolis System and
	// Metropolis data partition.
	PartitionSize PartitionSizeInfo
}

type plan struct {
	*Params
	systemImage      structfs.Blob
	efiBootPath      string
	efiRoot          structfs.Tree
	tbl              *gpt.Table
	efiPartition     *gpt.Partition
	systemPartitionA *gpt.Partition
	systemPartitionB *gpt.Partition
	dataPartition    *gpt.Partition
}

// Apply actually writes the planned installation to the blockdevice.
func (i *plan) Apply() (*efivarfs.LoadOption, error) {
	// Discard the entire device, we're going to write new data over it.
	// Ignore errors, this is only advisory.
	i.Output.Discard(0, i.Output.BlockCount()*i.Output.BlockSize())

	if err := fat32.WriteFS(blockdev.NewRWS(i.efiPartition), i.efiRoot, fat32.Options{
		BlockSize:  uint16(i.efiPartition.BlockSize()),
		BlockCount: uint32(i.efiPartition.BlockCount()),
		Label:      "MNGN_BOOT",
	}); err != nil {
		return nil, fmt.Errorf("failed to write FAT32: %w", err)
	}

	systemImage, err := i.systemImage.Open()
	if err != nil {
		return nil, fmt.Errorf("failed to open system image: %w", err)
	}
	if _, err := io.CopyN(blockdev.NewRWS(i.systemPartitionA), systemImage, i.systemImage.Size()); err != nil {
		systemImage.Close()
		return nil, fmt.Errorf("failed to write system partition A: %w", err)
	}
	systemImage.Close()

	if err := i.tbl.Write(); err != nil {
		return nil, fmt.Errorf("failed to write Table: %w", err)
	}

	// Build an EFI boot entry pointing to the image's ESP.
	return &efivarfs.LoadOption{
		Description: "Metropolis",
		FilePath: efivarfs.DevicePath{
			&efivarfs.HardDrivePath{
				PartitionNumber:     1,
				PartitionStartBlock: i.efiPartition.FirstBlock,
				PartitionSizeBlocks: i.efiPartition.SizeBlocks(),
				PartitionMatch: efivarfs.PartitionGPT{
					PartitionUUID: i.efiPartition.ID,
				},
			},
			efivarfs.FilePath("/" + i.efiBootPath),
		},
	}, nil
}

// Plan allows to prepare an installation without modifying any data on the
// system. To apply the planned installation, call Apply on the returned plan.
func Plan(p *Params) (*plan, error) {
	params := &plan{Params: p}

	payload := p.OSImage.Payload
	if p.UnverifiedPayloads {
		payload = p.OSImage.PayloadUnverified
	}
	efiPayload, err := payload("kernel.efi")
	if err != nil {
		return nil, fmt.Errorf("cannot open EFI payload in OS image: %w", err)
	}
	params.systemImage, err = payload("system")
	if err != nil {
		return nil, fmt.Errorf("cannot open system image in OS image: %w", err)
	}

	params.tbl, err = gpt.New(params.Output)
	if err != nil {
		return nil, fmt.Errorf("invalid block device: %w", err)
	}

	params.tbl.ID = params.DiskGUID
	architecture := p.OSImage.Config.ProductInfo.Architecture()
	if architecture == "x86_64" {
		params.tbl.BootCode = BootcodeX86
	}
	params.efiPartition = &gpt.Partition{
		Type: gpt.PartitionTypeEFISystem,
		Name: ESPLabel,
	}

	if err := params.tbl.AddPartition(params.efiPartition, params.PartitionSize.ESP*Mi); err != nil {
		return nil, fmt.Errorf("failed to allocate ESP: %w", err)
	}

	if err := params.efiRoot.PlaceFile(EFIBootAPath, efiPayload); err != nil {
		return nil, err
	}
	// Place the A/B loader at the EFI bootloader autodiscovery path.
	params.efiBootPath, err = EFIBootPath(architecture)
	if err != nil {
		return nil, err
	}
	if err := params.efiRoot.PlaceFile(params.efiBootPath, params.ABLoader); err != nil {
		return nil, err
	}
	if params.NodeParameters != nil {
		if err := params.efiRoot.PlaceFile(nodeParamsPath, params.NodeParameters); err != nil {
			return nil, err
		}
	}

	// Try to layout the fat32 partition. If it detects that the disk is too
	// small, an error will be returned.
	if _, err := fat32.SizeFS(params.efiRoot, fat32.Options{
		BlockSize:  uint16(params.efiPartition.BlockSize()),
		BlockCount: uint32(params.efiPartition.BlockCount()),
		Label:      "MNGN_BOOT",
	}); err != nil {
		return nil, fmt.Errorf("failed to calculate size of FAT32: %w", err)
	}

	// Create the system partition.
	params.systemPartitionA = &gpt.Partition{
		Type: SystemAType,
		Name: SystemALabel,
	}
	if err := params.tbl.AddPartition(params.systemPartitionA, params.PartitionSize.System*Mi); err != nil {
		return nil, fmt.Errorf("failed to allocate system partition A: %w", err)
	}
	params.systemPartitionB = &gpt.Partition{
		Type: SystemBType,
		Name: SystemBLabel,
	}
	if err := params.tbl.AddPartition(params.systemPartitionB, params.PartitionSize.System*Mi); err != nil {
		return nil, fmt.Errorf("failed to allocate system partition B: %w", err)
	}

	// Create the data partition only if its size is specified.
	if params.PartitionSize.Data != 0 {
		params.dataPartition = &gpt.Partition{
			Type: DataType,
			Name: DataLabel,
		}
		if err := params.tbl.AddPartition(params.dataPartition, -1); err != nil {
			return nil, fmt.Errorf("failed to allocate data partition: %w", err)
		}
	}

	return params, nil
}

const Mi = 1024 * 1024

// Write installs Metropolis OS to a block device.
func Write(params *Params) (*efivarfs.LoadOption, error) {
	p, err := Plan(params)
	if err != nil {
		return nil, err
	}

	return p.Apply()
}
