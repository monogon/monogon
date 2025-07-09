// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Installer creates a Metropolis image at a suitable block device based on the
// OS image present in the installation medium's ESP, after which it reboots.
// It's meant to be used as an init process.
package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"source.monogon.dev/go/logging"
	"source.monogon.dev/metropolis/installer/install"
	"source.monogon.dev/metropolis/node/core/devmgr"
	"source.monogon.dev/osbase/blockdev"
	"source.monogon.dev/osbase/bringup"
	"source.monogon.dev/osbase/efivarfs"
	"source.monogon.dev/osbase/oci"
	"source.monogon.dev/osbase/oci/osimage"
	"source.monogon.dev/osbase/structfs"
	"source.monogon.dev/osbase/supervisor"
	"source.monogon.dev/osbase/sysfs"
)

//go:embed metropolis/node/abloader/abloader.efi
var abloader []byte

//go:embed build/copyright_line.txt
var copyrightLine string

const mib = 1024 * 1024

// mountInstallerESP mounts the filesystem the installer was loaded from based
// on espPath, which must point to the appropriate partition block device. The
// filesystem is mounted at /installer.
func mountInstallerESP(espPath string) error {
	// Create the mountpoint.
	if err := unix.Mkdir("/installer", 0700); err != nil {
		return fmt.Errorf("couldn't create the installer mountpoint: %w", err)
	}
	// Mount the filesystem.
	if err := unix.Mount(espPath, "/installer", "vfat", unix.MS_NOEXEC|unix.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("couldn't mount the installer ESP (%q -> %q): %w", espPath, "/installer", err)
	}
	return nil
}

// findInstallableBlockDevices returns names of all the block devices suitable
// for hosting a Metropolis installation, limited by the size expressed in
// bytes minSize. The install medium espDev will be excluded from the result.
func findInstallableBlockDevices(l logging.Leveled, espDev string, minSize uint64) ([]string, error) {
	// Use the partition's name to find and return the name of its parent
	// device. It will be excluded from the list of suitable target devices.
	srcDev, err := sysfs.ParentBlockDevice(espDev)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch parent device: %w", err)
	}
	// Build the exclusion list containing forbidden handle prefixes.
	exclude := []string{"dm-", "zram", "ram", "loop", srcDev}

	// Get the block device handles by looking up directory contents.
	const blkDirPath = "/sys/class/block"
	blkDevs, err := os.ReadDir(blkDirPath)
	if err != nil {
		return nil, fmt.Errorf("couldn't read %q: %w", blkDirPath, err)
	}
	// Iterate over the handles, skipping any block device that either points to
	// a partition, matches the exclusion list, or is smaller than minSize.
	var suitable []string
probeLoop:
	for _, devInfo := range blkDevs {
		// Skip devices according to the exclusion list.
		for _, prefix := range exclude {
			if strings.HasPrefix(devInfo.Name(), prefix) {
				continue probeLoop
			}
		}

		// Skip partition symlinks.
		if _, err := os.Stat(filepath.Join(blkDirPath, devInfo.Name(), "partition")); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("while probing sysfs: %w", err)
		}

		// Skip devices of insufficient size.
		devPath := filepath.Join("/dev", devInfo.Name())
		dev, err := blockdev.Open(devPath, blockdev.WithReadonly)
		if err != nil {
			l.Warningf("couldn't open a block device at %q, ignoring: %v", devPath, err)
			continue
		}
		devSize := uint64(dev.BlockCount() * dev.BlockSize())
		dev.Close()
		if devSize < minSize {
			continue
		}

		suitable = append(suitable, devInfo.Name())
	}
	return suitable, nil
}

func main() {
	bringup.Runnable(installerRunnable).Run()
}

func installerRunnable(ctx context.Context) error {
	l := supervisor.Logger(ctx)

	l.Info("Metropolis Installer")
	l.Info(copyrightLine)
	l.Info("")

	devmgrSvc := devmgr.New()
	supervisor.Run(ctx, "devmgr", devmgrSvc.Run)

	// Validate we are running via EFI.
	if _, err := os.Stat("/sys/firmware/efi"); os.IsNotExist(err) {
		// nolint:ST1005
		return errors.New("Monogon OS can only be installed on EFI-booted machines, this one is not")
	}

	// Read the installer ESP UUID from efivarfs.
	espUuid, err := efivarfs.ReadLoaderDevicePartUUID()
	if err != nil {
		return fmt.Errorf("while reading the installer ESP UUID: %w", err)
	}
	// Wait for up to 30 tries @ 1s (30s) for the ESP to show up
	var espDev string
	var retries = 30
	for {
		// Look up the installer partition based on espUuid.
		espDev, err = sysfs.DeviceByPartUUID(espUuid)
		if err == nil {
			break
		} else if errors.Is(err, sysfs.ErrDevNotFound) && retries > 0 {
			time.Sleep(1 * time.Second)
			retries--
		} else {
			return fmt.Errorf("while resolving the installer device handle: %w", err)
		}
	}
	espPath := filepath.Join("/dev", espDev)
	// Mount the installer partition. The OS image will be read from it.
	if err := mountInstallerESP(espPath); err != nil {
		return fmt.Errorf("while mounting the installer ESP: %w", err)
	}

	nodeParameters, err := structfs.OSPathBlob("/installer/metropolis-installer/nodeparams.pb")
	if err != nil {
		return fmt.Errorf("failed to open node parameters from ESP: %w", err)
	}

	ociImage, err := oci.ReadLayout("/installer/metropolis-installer/osimage")
	if err != nil {
		return fmt.Errorf("failed to read OS image from ESP: %w", err)
	}
	osImage, err := osimage.Read(ociImage)
	if err != nil {
		return fmt.Errorf("failed to read OS image from ESP: %w", err)
	}

	// Build the install parameters.
	installParams := install.Params{
		PartitionSize: install.PartitionSizeInfo{
			// ESP is the size of the node ESP partition, expressed in mebibytes.
			ESP: 384,
			// System is the size of the node system partition, expressed in
			// mebibytes.
			System: 4096,
			// Data must be nonzero in order for the data partition to be created.
			// install will extend the data partition to fill all the available space
			// whenever it's writing to block devices, such as now.
			Data: 128,
		},
		OSImage:        osImage,
		ABLoader:       structfs.Bytes(abloader),
		NodeParameters: nodeParameters,
	}
	// Calculate the minimum target size based on the installation parameters.
	minSize := uint64((installParams.PartitionSize.ESP +
		installParams.PartitionSize.System*2 +
		installParams.PartitionSize.Data + 1) * mib)

	// Look for suitable block devices, given the minimum size.
	blkDevs, err := findInstallableBlockDevices(l, espDev, minSize)
	if err != nil {
		return err
	}
	if len(blkDevs) == 0 {
		return fmt.Errorf("couldn't find a suitable block device")
	}
	// Set the first suitable block device found as the installation target.
	tgtBlkdevName := blkDevs[0]
	// Update the install parameters with a path pointing at the target device.
	tgtBlkdevPath := filepath.Join("/dev", tgtBlkdevName)

	tgtBlockDev, err := blockdev.Open(tgtBlkdevPath)
	if err != nil {
		return fmt.Errorf("error opening target device: %w", err)
	}
	installParams.Output = tgtBlockDev

	// Use install to partition the target block device and set up its ESP.
	// Write will return an EFI boot entry on success.
	l.Infof("Installing to %s...", tgtBlkdevPath)
	be, err := install.Write(&installParams)
	if err != nil {
		return fmt.Errorf("while installing: %w", err)
	}

	// Create an EFI boot entry for Metropolis.
	en, err := efivarfs.AddBootEntry(be)
	if err != nil {
		return fmt.Errorf("while creating a boot entry: %w", err)
	}
	// Erase the preexisting boot order, leaving Metropolis as the only option.
	if err := efivarfs.SetBootOrder(efivarfs.BootOrder{uint16(en)}); err != nil {
		return fmt.Errorf("while adjusting the boot order: %w", err)
	}

	// Reboot.
	tgtBlockDev.Close()
	unix.Sync()
	l.Info("Installation completed. Rebooting.")
	unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
	return nil
}
