// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"

	"source.monogon.dev/metropolis/cli/flagdefs"
	metroctl "source.monogon.dev/metropolis/cli/metroctl/core"
	"source.monogon.dev/metropolis/node/allocs"
	cpb "source.monogon.dev/metropolis/proto/common"
	mlaunch "source.monogon.dev/metropolis/test/launch"
)

var (
	// These are filled by bazel at linking time with the canonical path of
	// their corresponding file. Inside the init function we resolve it
	// with the rules_go runfiles package to the real path.
	xMetroctlPath string
)

func init() {
	var err error
	for _, path := range []*string{
		&xMetroctlPath,
	} {
		*path, err = runfiles.Rlocation(*path)
		if err != nil {
			panic(err)
		}
	}
}

const maxNodes = 256

func nodeSetFlag(p *[]int, name string, usage string) {
	flag.Func(name, usage, func(val string) error {
		for _, part := range strings.Split(val, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			startStr, endStr, ok := strings.Cut(part, "-")
			if !ok {
				endStr = startStr
			}
			start, err := strconv.Atoi(startStr)
			if err != nil {
				return err
			}
			end, err := strconv.Atoi(endStr)
			if err != nil {
				return err
			}
			if end >= maxNodes {
				return fmt.Errorf("node index %v out of range, there can be at most %v nodes", end, maxNodes)
			}
			if end < start {
				return fmt.Errorf("invalid range %q, end is smaller than start", part)
			}
			for i := start; i <= end; i++ {
				*p = append(*p, i)
			}
		}
		return nil
	})
}

func memoryMiBFlag(p *int, name string, usage string) {
	flag.Func(name, usage, func(val string) error {
		multiplier := 1
		switch {
		case strings.HasSuffix(val, "M"):
		case strings.HasSuffix(val, "G"):
			multiplier = 1024
		default:
			return errors.New("must have suffix M for MiB or G for GiB")
		}
		intVal, err := strconv.Atoi(val[:len(val)-1])
		if err != nil {
			return err
		}
		*p = multiplier * intVal
		return nil
	})
}

func diskBytesFlag(p *uint64, name string, usage string) {
	flag.Func(name, usage, func(val string) error {
		var multiplier uint64
		switch {
		case strings.HasSuffix(val, "M"):
			multiplier = 1024 * 1024
		case strings.HasSuffix(val, "G"):
			multiplier = 1024 * 1024 * 1024
		case strings.HasSuffix(val, "T"):
			multiplier = 1024 * 1024 * 1024 * 1024
		default:
			return errors.New("must have suffix M for MiB, G for GiB or T for TiB")
		}
		intVal, err := strconv.ParseUint(val[:len(val)-1], 10, 64)
		if err != nil {
			return err
		}
		*p = multiplier * intVal
		return nil
	})
}

func main() {
	clusterConfig := cpb.ClusterConfiguration{
		Kubernetes: &cpb.ClusterConfiguration_Kubernetes{
			NodeLabelsToSynchronize: []*cpb.ClusterConfiguration_Kubernetes_NodeLabelsToSynchronize{
				{Regexp: "^" + mlaunch.NodeNumberKey + "$"},
			},
		},
	}
	opts := mlaunch.ClusterOptions{
		NodeLogsToFiles:             true,
		InitialClusterConfiguration: &clusterConfig,
	}
	var consensusMemberList, kubernetesControllerList, kubernetesWorkerList []int

	flag.IntVar(&opts.NumNodes, "num-nodes", 3, "Number of cluster nodes")
	flag.StringVar(&clusterConfig.ClusterDomain, "cluster-domain", "cluster.internal", "Cluster domain")
	flagdefs.TPMModeVar(flag.CommandLine, &clusterConfig.TpmMode, "tpm-mode", cpb.ClusterConfiguration_TPM_MODE_REQUIRED, "TPM mode to set on cluster")
	flagdefs.StorageSecurityPolicyVar(flag.CommandLine, &clusterConfig.StorageSecurityPolicy, "storage-security", cpb.ClusterConfiguration_STORAGE_SECURITY_POLICY_NEEDS_INSECURE, "Storage security policy to set on cluster")
	flag.IntVar(&opts.Node.CPUs, "cpu", 1, "Number of virtual CPUs of each node")
	flag.IntVar(&opts.Node.ThreadsPerCPU, "threads-per-cpu", 1, "Number of threads per CPU")
	memoryMiBFlag(&opts.Node.MemoryMiB, "ram", "RAM size of each node, with suffix M for MiB or G for GiB")
	diskBytesFlag(&opts.Node.DiskBytes, "disk", "Disk size of each node, with suffix M for MiB, G for GiB or T for TiB")
	flag.BoolVar(&opts.Node.AllowReboot, "allow-reboot", false, "Allow nodes to reboot. This is off by default, to make it more noticeable when a node has crashed.")
	nodeSetFlag(&consensusMemberList, "consensus-member", "List of nodes which get the Consensus Member role. Example: 0,3-5")
	nodeSetFlag(&kubernetesControllerList, "kubernetes-controller", "List of nodes which get the Kubernetes Controller role. Example: 0,3-5")
	nodeSetFlag(&kubernetesWorkerList, "kubernetes-worker", "List of nodes which get the Kubernetes Worker role. Example: 0,3-5")
	flag.Parse()

	if opts.NumNodes >= maxNodes {
		log.Fatalf("num-nodes (%v) is too large, there can be at most %v nodes", opts.NumNodes, maxNodes)
	}
	for _, list := range [][]int{consensusMemberList, kubernetesControllerList, kubernetesWorkerList} {
		for i := len(list) - 1; i >= 0; i-- {
			if list[i] >= opts.NumNodes {
				log.Fatalf("Node index %v out of range, can be at most %v", list[i], opts.NumNodes-1)
			}
		}
	}

	ctx, _ := signal.NotifyContext(context.Background(), os.Interrupt)
	cl, err := mlaunch.LaunchCluster(ctx, opts)
	if err != nil {
		log.Fatalf("LaunchCluster: %v", err)
	}

	for _, node := range consensusMemberList {
		cl.MakeConsensusMember(ctx, cl.NodeIDs[node])
	}
	for _, node := range kubernetesControllerList {
		cl.MakeKubernetesController(ctx, cl.NodeIDs[node])
	}
	for _, node := range kubernetesWorkerList {
		cl.MakeKubernetesWorker(ctx, cl.NodeIDs[node])
	}

	wpath, err := cl.MakeMetroctlWrapper()
	if err != nil {
		log.Fatalf("MakeWrapper: %v", err)
	}

	apiserver := cl.Nodes[cl.NodeIDs[0]].ManagementAddress
	// Wait for the API server to start listening.
	for {
		conn, err := cl.DialNode(ctx, net.JoinHostPort(apiserver, allocs.PortKubernetesAPIWrapped.PortString()))
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	configName := "launch-cluster"
	if err := metroctl.InstallKubeletConfig(ctx, xMetroctlPath, cl.ConnectOptions(), configName, apiserver); err != nil {
		log.Fatalf("InstallKubeletConfig: %v", err)
	}

	log.Printf("Launch: Cluster running!")
	log.Printf("  To access cluster use: metroctl %s", cl.MetroctlFlags())
	log.Printf("  Or use this handy wrapper: %s", wpath)
	log.Printf("  To access Kubernetes, use kubectl --context=%s", configName)

	<-ctx.Done()
	cl.Close()
}
