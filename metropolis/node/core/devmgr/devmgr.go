// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Package devmgr is the userspace pendant to the kernel device management
// system. It talks to the kernel and the performs any further userspace actions
// related to device events. It corresponds very roughly to systemd-udevd on
// more conventional Linux distros. It currently only handles dynamic module
// loading, but will be extended to provide better device handling in other
// parts of the system.
package devmgr

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/mdlayher/kobject"

	"source.monogon.dev/osbase/kmod"
	"source.monogon.dev/osbase/supervisor"
)

type Service struct{}

func New() *Service {
	return &Service{}
}

func (s *Service) Run(ctx context.Context) error {
	c, err := kobject.New()
	if err != nil {
		return fmt.Errorf("unable to create kobject uevent socket: %w", err)
	}
	defer c.Close()
	// The default buffer is very small, use a larger one to not constantly
	// run out of buffer space needing a restart of this runnable.
	c.SetReadBuffer(4 * 1024 * 1024)

	l := supervisor.Logger(ctx)

	modMgr, err := kmod.NewManagerFromPath("/lib/modules")
	if err != nil {
		return fmt.Errorf("error creating module manager: %w", err)
	}

	// Start goroutine which instructs the kernel to generate "synthetic"
	// uevents for all preexisting devices. This allows the kobject netlink
	// listener below to "catch up" on devices added before it was created.
	// This functionality is minimally documented in the Linux kernel, the
	// closest we have is
	// https://www.kernel.org/doc/Documentation/ABI/testing/sysfs-uevent which
	// contains documentation on how to trigger synthetic events.
	go func() {
		err = filepath.WalkDir("/sys/devices", func(path string, d fs.DirEntry, err error) error {
			if !d.IsDir() && d.Name() == "uevent" {
				if err := os.WriteFile(path, []byte("add"), 0); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			l.Errorf("failed to load initial device database: %v", err)
		} else {
			l.Info("Initial device loading done")
		}
	}()

	for {
		e, err := c.Receive()
		if err != nil {
			return fmt.Errorf("error receiving kobject uevent: %w", err)
		}
		if e.Action == kobject.Add {
			if e.Values["MODALIAS"] != "" {
				go func(e *kobject.Event) {
					if err := modMgr.LoadModulesForDevice(e.Values["MODALIAS"]); err != nil {
						l.Errorf("Error loading kernel modules: %w", err)
					}
				}(e)
			}
		}
	}
}
