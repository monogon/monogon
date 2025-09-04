// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package allocs

import (
	"strconv"
)

// Port is a TCP and/or UDP port number reserved for and used by Metropolis
// node code.
type Port uint16

const (
	// PortCuratorService is the TCP port on which the Curator listens for gRPC
	// calls and services Management/AAA/Curator RPCs.
	PortCuratorService Port = 7835
	// PortConsensus is the TCP port on which etcd listens for peer traffic.
	PortConsensus Port = 7834
	// PortDebugService is the TCP port on which the debug service serves gRPC
	// traffic. This is only available in debug builds.
	PortDebugService Port = 7837
	// PortWireGuard is the UDP port on which the Wireguard Kubernetes network
	// overlay listens for incoming peer traffic.
	PortWireGuard Port = 7838
	// PortNodeManagement is the TCP port on which the node-local management service
	// serves gRPC traffic for NodeManagement.
	PortNodeManagement Port = 7839
	// PortMetrics is the TCP port on which the Metrics Service exports
	// Prometheus-compatible metrics for this node, secured using TLS and the
	// Cluster/Node certificates.
	PortMetrics Port = 7840
	// PortMetricsNodeListener is the TCP port on which the Prometheus node_exporter
	// runs, bound to 127.0.0.1. The Metrics Service proxies traffic to it from the
	// public PortMetrics.
	PortMetricsNodeListener Port = 7841
	// PortMetricsEtcdListener is the TCP port on which the etcd exporter
	// runs, bound to 127.0.0.1. The metrics service proxies traffic to it from the
	// public PortMetrics.
	PortMetricsEtcdListener Port = 7842
	// PortMetricsKubeSchedulerListener is the TCP port on which the proxy for
	// the kube-scheduler runs, bound to 127.0.0.1. The metrics service proxies
	// traffic to it from the public PortMetrics.
	PortMetricsKubeSchedulerListener Port = 7843
	// PortMetricsKubeControllerManagerListener is the TCP port on which the
	// proxy for the controller-manager runs, bound to 127.0.0.1. The metrics
	// service proxies traffic to it from the public PortMetrics.
	PortMetricsKubeControllerManagerListener Port = 7844
	// PortMetricsKubeAPIServerListener is the TCP port on which the
	// proxy for the api-server runs, bound to 127.0.0.1. The metrics
	// service proxies traffic to it from the public PortMetrics.
	PortMetricsKubeAPIServerListener Port = 7845
	// PortMetricsContainerdListener is the TCP port on which the
	// containerd metrics endpoint, bound to 127.0.0.1, is exposed.
	PortMetricsContainerdListener Port = 7846
	// PortKubernetesAPI is the TCP port on which the Kubernetes API is
	// exposed.
	PortKubernetesAPI Port = 6443
	// PortKubernetesAPIWrapped is the TCP port on which the Metropolis
	// authenticating proxy for the Kubernetes API is exposed.
	PortKubernetesAPIWrapped Port = 6444
	// PortKubernetesWorkerLocalAPI is the TCP port on which Kubernetes worker nodes
	// run a loadbalancer to access the cluster's API servers before cluster
	// networking is available. This port is only bound to 127.0.0.1.
	PortKubernetesWorkerLocalAPI Port = 6445
	// PortDebugger is the port on which the delve debugger runs (on debug
	// builds only). Not to be confused with PortDebugService.
	PortDebugger Port = 2345
)

var SystemPorts = []Port{
	PortCuratorService,
	PortConsensus,
	PortDebugService,
	PortWireGuard,
	PortNodeManagement,
	PortMetrics,
	PortMetricsNodeListener,
	PortMetricsEtcdListener,
	PortMetricsKubeSchedulerListener,
	PortMetricsKubeControllerManagerListener,
	PortMetricsKubeAPIServerListener,
	PortMetricsContainerdListener,
	PortKubernetesAPI,
	PortKubernetesAPIWrapped,
	PortKubernetesWorkerLocalAPI,
	PortDebugger,
}

func (p Port) String() string {
	switch p {
	case PortCuratorService:
		return "curator"
	case PortConsensus:
		return "consensus"
	case PortDebugService:
		return "debug"
	case PortWireGuard:
		return "wireguard"
	case PortNodeManagement:
		return "node-mgmt"
	case PortMetrics:
		return "metrics"
	case PortMetricsNodeListener:
		return "metrics-node-exporter"
	case PortMetricsEtcdListener:
		return "metrics-etcd"
	case PortMetricsKubeSchedulerListener:
		return "metrics-kubernetes-scheduler"
	case PortMetricsKubeControllerManagerListener:
		return "metrics-kubernetes-controller-manager"
	case PortMetricsKubeAPIServerListener:
		return "metrics-kubernetes-api-server"
	case PortMetricsContainerdListener:
		return "metrics-containerd"
	case PortKubernetesAPI:
		return "kubernetes-api"
	case PortKubernetesAPIWrapped:
		return "kubernetes-api-wrapped"
	case PortKubernetesWorkerLocalAPI:
		return "kubernetes-worker-local-api"
	case PortDebugger:
		return "delve"
	}
	return "unknown"
}

func (p Port) PortString() string {
	return strconv.Itoa(int(p))
}
