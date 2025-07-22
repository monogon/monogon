// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"source.monogon.dev/metropolis/installer/install"
	"source.monogon.dev/metropolis/node/core/network"
	"source.monogon.dev/metropolis/node/core/update"
	"source.monogon.dev/osbase/blockdev"
	"source.monogon.dev/osbase/bringup"
	"source.monogon.dev/osbase/gpt"
	"source.monogon.dev/osbase/supervisor"

	apb "source.monogon.dev/metropolis/proto/api"
)

var Variant = "U"

func main() {
	bringup.Runnable(testosRunnable).Run()
}

func testosRunnable(ctx context.Context) error {
	supervisor.Logger(ctx).Info("TESTOS_VARIANT=" + Variant)
	networkSvc := network.New(nil, nil, nil)
	networkSvc.DHCPVendorClassID = "dev.monogon.testos.v1"
	supervisor.Run(ctx, "networking", networkSvc.Run)

	vda, err := blockdev.Open("/dev/vda")
	if err != nil {
		return fmt.Errorf("unable to open root device: %w", err)
	}
	defer vda.Close()
	vdaParts, err := gpt.Read(vda)
	if err != nil {
		return fmt.Errorf("unable to read GPT from root device: %w", err)
	}

	updateSvc := update.Service{
		Logger: supervisor.MustSubLogger(ctx, "update"),
	}
	for pn, p := range vdaParts.Partitions {
		if p.IsUnused() {
			continue
		}
		switch p.Type {
		case gpt.PartitionTypeEFISystem:
			if err := unix.Mount(fmt.Sprintf("/dev/vda%d", pn+1), "/esp", "vfat", unix.MS_SYNC, ""); err != nil {
				return fmt.Errorf("unable to mkdir ESP mountpoint: %w", err)
			}
			updateSvc.ProvideESP("/esp", uint32(pn+1), p)
		case install.SystemAType:
			if err := unix.Symlink(fmt.Sprintf("/dev/vda%d", pn+1), "/dev/system-a"); err != nil {
				return fmt.Errorf("failed to symlink system-a: %w", err)
			}
		case install.SystemBType:
			if err := unix.Symlink(fmt.Sprintf("/dev/vda%d", pn+1), "/dev/system-b"); err != nil {
				return fmt.Errorf("failed to symlink system-b: %w", err)
			}
		}
	}
	if err := updateSvc.MarkBootSuccessful(); err != nil {
		supervisor.Logger(ctx).Errorf("error marking boot successful: %w", err)
	}

	_, err = os.Stat("/sys/firmware/qemu_fw_cfg/by_name/opt/use_kexec/raw")
	useKexec := err == nil
	supervisor.Logger(ctx).Infof("Kexec: %v", useKexec)

	nextVariantMap := map[string]string{
		"X": "y",
		"Y": "z",
	}
	nextVariant := nextVariantMap[Variant]

	if nextVariant != "" {
		nextDigest, err := os.ReadFile(fmt.Sprintf("/sys/firmware/qemu_fw_cfg/by_name/opt/testos_%s_digest/raw", nextVariant))
		if err != nil {
			return fmt.Errorf("unable to read next digest: %w", err)
		}
		imageRef := &apb.OSImageRef{
			Scheme:     "http",
			Host:       "10.42.0.5:80",
			Repository: "testos",
			Tag:        nextVariant,
			Digest:     string(nextDigest),
		}
		if err := updateSvc.InstallImage(ctx, imageRef, useKexec); err != nil {
			supervisor.Logger(ctx).Errorf("Error installing new image: %v", err)
		}
	}
	supervisor.Signal(ctx, supervisor.SignalHealthy)
	supervisor.Logger(ctx).Info("Installed image successfully, powering off")
	unix.Sync()
	time.Sleep(1 * time.Second)
	if useKexec && Variant != "Z" {
		unix.Reboot(unix.LINUX_REBOOT_CMD_KEXEC)
	} else {
		unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
	}
	return nil
}
