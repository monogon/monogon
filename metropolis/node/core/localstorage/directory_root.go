// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package localstorage

import (
	"context"
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"source.monogon.dev/metropolis/node/core/localstorage/crypt"
	"source.monogon.dev/metropolis/node/core/localstorage/declarative"
	"source.monogon.dev/metropolis/node/core/update"
)

func (r *Root) Start(ctx context.Context, updateSvc *update.Service) error {
	r.Data.flagLock.Lock()
	defer r.Data.flagLock.Unlock()
	if r.Data.canMount {
		return fmt.Errorf("cannot re-start root storage")
	}
	// TODO(q3k): turn this into an Ensure call
	err := crypt.MakeBlockDevices(ctx, updateSvc)
	if err != nil {
		return fmt.Errorf("MakeBlockDevices: %w", err)
	}

	if err := unix.Mount(crypt.ESPDevicePath, r.ESP.FullPath(), "vfat", unix.MS_NOEXEC|unix.MS_NODEV|unix.MS_SYNCHRONOUS, ""); err != nil {
		return fmt.Errorf("mounting ESP partition: %w", err)
	}

	r.Data.canMount = true

	if err := unix.Mount("tmpfs", r.Tmp.FullPath(), "tmpfs", unix.MS_NOEXEC|unix.MS_NODEV, ""); err != nil {
		return fmt.Errorf("mounting /tmp: %w", err)
	}

	if err := unix.Mount("tmpfs", r.Ephemeral.FullPath(), "tmpfs", unix.MS_NODEV, ""); err != nil {
		return fmt.Errorf("mounting /ephemeral: %w", err)
	}

	if err := unix.Mount("tmpfs", r.Run.FullPath(), "tmpfs", unix.MS_NOEXEC|unix.MS_NODEV, ""); err != nil {
		return fmt.Errorf("mounting /run: %w", err)
	}

	// TODO(q3k): do this automatically?
	for _, d := range []declarative.DirectoryPlacement{
		r.Ephemeral.Consensus,
		r.Ephemeral.Containerd, r.Ephemeral.Containerd.Tmp, r.Ephemeral.Containerd.RunSC,
		r.Ephemeral.FlexvolumePlugins,
		r.ESP.Metropolis,
	} {
		err := d.MkdirAll(0700)
		if err != nil {
			return fmt.Errorf("creating directory failed: %w", err)
		}
	}

	for _, d := range []declarative.DirectoryPlacement{
		r.Ephemeral, r.Ephemeral.Containerd, r.Ephemeral.Containerd.Tmp,
	} {
		if err := os.Chmod(d.FullPath(), 0755); err != nil {
			return fmt.Errorf("failed to chmod containerd tmp path: %w", err)
		}
	}

	return nil
}
