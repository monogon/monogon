// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"

	"source.monogon.dev/go/logging"
	"source.monogon.dev/metropolis/node/core/cluster"
	"source.monogon.dev/metropolis/node/core/devmgr"
	"source.monogon.dev/metropolis/node/core/localstorage"
	"source.monogon.dev/metropolis/node/core/localstorage/declarative"
	"source.monogon.dev/metropolis/node/core/metrics"
	"source.monogon.dev/metropolis/node/core/network"
	"source.monogon.dev/metropolis/node/core/network/ipam"
	"source.monogon.dev/metropolis/node/core/productinfo"
	"source.monogon.dev/metropolis/node/core/roleserve"
	"source.monogon.dev/metropolis/node/core/rpc/resolver"
	"source.monogon.dev/metropolis/node/core/tconsole"
	timesvc "source.monogon.dev/metropolis/node/core/time"
	"source.monogon.dev/metropolis/node/core/update"
	"source.monogon.dev/osbase/bringup"
	"source.monogon.dev/osbase/event/memory"
	"source.monogon.dev/osbase/logtree"
	"source.monogon.dev/osbase/net/dns"
	"source.monogon.dev/osbase/supervisor"
	"source.monogon.dev/osbase/sysctl"
	"source.monogon.dev/osbase/tpm"
)

func main() {
	bringup.Runnable(root).RunWith(bringup.Config{
		Console: bringup.ConsoleConfig{
			ShortenDictionary: logtree.MetropolisShortenDict,
			Filter:            consoleFilter,
		},
		Supervisor: bringup.SupervisorConfig{
			Metrics: []supervisor.Metrics{
				supervisor.NewMetricsPrometheus(metrics.CoreRegistry),
			},
		},
	})
}

// consoleFilter is used to filter out some uselessly verbose logs from the
// console.
//
// This should be limited to external services, our internal services should
// instead just have good logging by default.
func consoleFilter(p *logtree.LogEntry) bool {
	if p.Raw != nil {
		return false
	}
	if p.Leveled == nil {
		return false
	}
	s := string(p.DN)
	if strings.HasPrefix(s, "root.role.controlplane.launcher.consensus.etcd") {
		return p.Leveled.Severity().AtLeast(logging.WARNING)
	}
	// TODO(q3k): turn off RPC traces instead
	if strings.HasPrefix(s, "root.role.controlplane.launcher.curator.listener.rpc") {
		return false
	}
	if strings.HasPrefix(s, "root.role.kubernetes.run.kubernetes.networked.kubelet") {
		return p.Leveled.Severity().AtLeast(logging.WARNING)
	}
	if strings.HasPrefix(s, "root.role.kubernetes.run.kubernetes.networked.apiserver") {
		return p.Leveled.Severity().AtLeast(logging.WARNING)
	}
	if strings.HasPrefix(s, "root.role.kubernetes.run.kubernetes.controller-manager") {
		return p.Leveled.Severity().AtLeast(logging.WARNING)
	}
	if strings.HasPrefix(s, "root.role.kubernetes.run.kubernetes.scheduler") {
		return p.Leveled.Severity().AtLeast(logging.WARNING)
	}
	if strings.HasPrefix(s, "root.kernel") {
		// Linux writes high-severity logs directly to the console anyways and
		// its low-severity logs are too verbose.
		return false
	}
	if strings.HasPrefix(s, "supervisor") {
		return p.Leveled.Severity().AtLeast(logging.WARNING)
	}
	return true
}

// Function which performs core, one-way initialization of the node. This means
// waiting for the network, starting the cluster manager, and then starting all
// services related to the node's roles.
func root(ctx context.Context) error {
	logger := supervisor.Logger(ctx)

	productInfo := productinfo.Get()
	logger.Infof("Starting %s init", productInfo.Info.Name)
	logger.Infof("Version: %s", productInfo.VersionString)
	logger.Infof("Variant: %s", productInfo.Info.Variant)
	if productInfo.Info.BuildTreeDirty {
		logger.Warning("Build tree dirty")
	}
	if productInfo.Info.CommitHash != "" {
		logger.Infof("Commit: %s", productInfo.Info.CommitHash)
		logger.Infof("Commit date: %s", productInfo.HumanCommitDate)
	}

	// Linux kernel default is 4096 which is far too low. Raise it to 1M which
	// is what gVisor suggests.
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &unix.Rlimit{Cur: 1048576, Max: 1048576}); err != nil {
		logger.Fatalf("Failed to raise rlimits: %v", err)
	}

	haveTPM := true
	if err := tpm.Initialize(logger); err != nil {
		logger.Warningf("Failed to initialize TPM 2.0: %v", err)
		haveTPM = false
	}

	metrics.CoreRegistry.MustRegister(dns.MetricsRegistry)
	var podNetwork memory.Value[*ipam.Prefixes]
	networkSvc := network.New(nil, []string{"hosts", "kubernetes"}, &podNetwork)
	networkSvc.DHCPVendorClassID = "dev.monogon.metropolis.node.v1"
	timeSvc := timesvc.New()
	devmgrSvc := devmgr.New()

	// This function initializes a headless Delve if this is a debug build or
	// does nothing if it's not
	initializeDebugger(networkSvc)

	// Prepare local storage.
	root := &localstorage.Root{}
	if err := declarative.PlaceFS(root, "/"); err != nil {
		panic(fmt.Errorf("when placing root FS: %w", err))
	}

	updateSvc := &update.Service{
		Logger: supervisor.MustSubLogger(ctx, "update"),
	}
	// Make node-wide cluster resolver.
	res := resolver.New(ctx, resolver.WithLogger(supervisor.MustSubLogger(ctx, "resolver")))

	// Start storage and network - we need this to get anything else done.
	if err := root.Start(ctx, updateSvc); err != nil {
		return fmt.Errorf("cannot start root FS: %w", err)
	}

	localNodeParams, err := getLocalNodeParams(ctx, root)
	if err != nil {
		return fmt.Errorf("cannot get local node parameters: %w", err)
	}

	if localNodeParams.NetworkConfig != nil {
		networkSvc.StaticConfig = localNodeParams.NetworkConfig
		if err := root.ESP.Metropolis.NetworkConfiguration.Marshal(localNodeParams.NetworkConfig); err != nil {
			logger.Errorf("Error writing back network_config from NodeParameters: %v", err)
		}
	}
	if networkSvc.StaticConfig == nil {
		staticConfig, err := root.ESP.Metropolis.NetworkConfiguration.Unmarshal()
		if err == nil {
			networkSvc.StaticConfig = staticConfig
		} else {
			logger.Errorf("Unable to load static config, proceeding without it: %v", err)
		}
	}

	if err := supervisor.Run(ctx, "devmgr", devmgrSvc.Run); err != nil {
		return fmt.Errorf("when starting devmgr: %w", err)
	}
	if err := supervisor.Run(ctx, "network", networkSvc.Run); err != nil {
		return fmt.Errorf("when starting network: %w", err)
	}
	if err := supervisor.Run(ctx, "time", timeSvc.Run); err != nil {
		return fmt.Errorf("when starting time: %w", err)
	}
	if err := supervisor.Run(ctx, "sysctl", nodeSysctls); err != nil {
		return fmt.Errorf("when applying sysctls: %w", err)
	}

	// The kernel does of course not run in this runnable, only the log pipe
	// runs in it.
	if err := supervisor.Run(ctx, "kernel", func(ctx context.Context) error {
		return logtree.KmsgPipe(ctx, supervisor.Logger(ctx))
	}); err != nil {
		return fmt.Errorf("when starting kernel log pipe: %w", err)
	}

	// Start the role service. The role service connects to the curator and runs
	// all node-specific role code (eg. Kubernetes services).
	logger.Infof("Starting role service...")
	rs := roleserve.New(roleserve.Config{
		StorageRoot: root,
		Network:     networkSvc,
		Resolver:    res,
		LogTree:     supervisor.LogTree(ctx),
		Update:      updateSvc,
		PodNetwork:  &podNetwork,
	})
	if err := supervisor.Run(ctx, "role", rs.Run); err != nil {
		return fmt.Errorf("failed to start role service: %w", err)
	}

	if err := runDebugService(ctx, rs, supervisor.LogTree(ctx), root); err != nil {
		return fmt.Errorf("when starting debug service: %w", err)
	}

	// Initialize interactive consoles.
	interactiveConsoles := []string{"/dev/tty0"}
	consoleConfig := tconsole.Config{
		Terminal:    tconsole.TerminalLinux,
		LogTree:     supervisor.LogTree(ctx),
		Network:     &networkSvc.Status,
		Roles:       &rs.LocalRoles,
		CuratorConn: &rs.CuratorConnection,
	}
	for _, c := range interactiveConsoles {
		console, err := tconsole.New(consoleConfig, c)
		if err != nil {
			logger.Infof("Failed to initialize interactive console at %s: %v", c, err)
		} else {
			logger.Infof("Started interactive console at %s", c)
			supervisor.Run(ctx, "console-"+c, console.Run)
		}
	}

	// Now that we have consoles, set console logging level to 1 (KERNEL_EMERG,
	// minimum possible). This prevents the TUI console from being polluted by
	// random printks.
	opts := sysctl.Options{
		"kernel.printk": "1",
	}
	if err := opts.Apply(); err != nil {
		logger.Errorf("Failed to configure printk logging: %v", err)
	}

	nodeParams, err := getNodeParams(ctx, root)
	if err != nil {
		return fmt.Errorf("cannot get node parameters: %w", err)
	}

	// Start cluster manager. This kicks off cluster membership machinery,
	// which will either start a new cluster, enroll into one or join one.
	m := cluster.NewManager(root, networkSvc, rs, updateSvc, nodeParams, haveTPM)
	if err := supervisor.Run(ctx, "cluster-manager", m.Run); err != nil {
		return fmt.Errorf("when starting cluster manager: %w", err)
	}

	supervisor.Signal(ctx, supervisor.SignalHealthy)
	supervisor.Signal(ctx, supervisor.SignalDone)
	return nil
}
