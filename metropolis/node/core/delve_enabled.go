// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os/exec"

	"source.monogon.dev/metropolis/node/allocs"
	"source.monogon.dev/metropolis/node/core/network"
)

// initializeDebugger attaches Delve to ourselves and exposes it on
// allocs.PortDebugger
// This is coupled to compilation_mode=dbg because otherwise Delve doesn't have
// the necessary DWARF debug info
func initializeDebugger(networkSvc *network.Service) {
	go func() {
		// This is intentionally delayed until network becomes available since
		// Delve for some reason connects to itself and in early-boot no
		// network interface is available to do that through. Also external
		// access isn't possible early on anyways.
		watcher := networkSvc.Status.Watch()
		_, err := watcher.Get(context.Background())
		if err != nil {
			panic(err)
		}
		dlvCmd := exec.Command("/dlv", "--headless=true", fmt.Sprintf("--listen=:%v", allocs.PortDebugger),
			"--accept-multiclient", "--only-same-user=false", "attach", "--continue", "1", "/init")
		if err := dlvCmd.Start(); err != nil {
			panic(err)
		}
	}()
}
