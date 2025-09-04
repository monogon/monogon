// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package test_core

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"google.golang.org/grpc"

	apb "source.monogon.dev/metropolis/proto/api"
	cpb "source.monogon.dev/metropolis/proto/common"

	"source.monogon.dev/metropolis/node/allocs"
	"source.monogon.dev/metropolis/node/core/rpc"
	mlaunch "source.monogon.dev/metropolis/test/launch"
	"source.monogon.dev/metropolis/test/localregistry"
	"source.monogon.dev/metropolis/test/util"
)

var (
	// These are filled by bazel at linking time with the canonical path of
	// their corresponding file. Inside the init function we resolve it
	// with the rules_go runfiles package to the real path.
	xTestImagesManifestPath string
)

func init() {
	var err error
	for _, path := range []*string{
		&xTestImagesManifestPath,
	} {
		*path, err = runfiles.Rlocation(*path)
		if err != nil {
			panic(err)
		}
	}
}

const (
	// Timeout for the global test context.
	//
	// Bazel would eventually time out the test after 900s ("large") if, for
	// some reason, the context cancellation fails to abort it.
	globalTestTimeout = 600 * time.Second

	// Timeouts for individual end-to-end tests of different sizes.
	smallTestTimeout = 60 * time.Second
	largeTestTimeout = 120 * time.Second
)

// TestE2ECore exercisees the core functionality of Metropolis: maintaining a
// control plane, changing node roles, ...
//
// The tests are performed against an in-memory cluster.
func TestE2ECore(t *testing.T) {
	// Set a global timeout to make sure this terminates
	ctx, cancel := context.WithTimeout(context.Background(), globalTestTimeout)
	defer cancel()

	df, err := os.ReadFile(xTestImagesManifestPath)
	if err != nil {
		t.Fatalf("Reading registry manifest failed: %v", err)
	}
	lr, err := localregistry.FromBazelManifest(df)
	if err != nil {
		t.Fatalf("Creating test image registry failed: %v", err)
	}
	// Launch cluster.
	clusterOptions := mlaunch.ClusterOptions{
		NumNodes:      2,
		LocalRegistry: lr,
		InitialClusterConfiguration: &cpb.ClusterConfiguration{
			ClusterDomain:         "cluster.test",
			TpmMode:               cpb.ClusterConfiguration_TPM_MODE_DISABLED,
			StorageSecurityPolicy: cpb.ClusterConfiguration_STORAGE_SECURITY_POLICY_NEEDS_INSECURE,
		},
	}
	cluster, err := mlaunch.LaunchCluster(ctx, clusterOptions)
	if err != nil {
		t.Fatalf("LaunchCluster failed: %v", err)
	}
	defer func() {
		err := cluster.Close()
		if err != nil {
			t.Fatalf("cluster Close failed: %v", err)
		}
	}()

	// Dial first node's curator.
	creds := rpc.NewAuthenticatedCredentials(cluster.Owner, rpc.WantInsecure())
	remote := net.JoinHostPort(cluster.Nodes[cluster.NodeIDs[0]].ManagementAddress, allocs.PortCuratorService.PortString())
	cl, err := grpc.NewClient(remote, grpc.WithContextDialer(cluster.DialNode), grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatalf("failed to create first node's curator client: %v", err)
	}
	defer cl.Close()
	mgmt := apb.NewManagementClient(cl)

	util.TestEventual(t, "Retrieving cluster directory sucessful", ctx, 60*time.Second, func(ctx context.Context) error {
		res, err := mgmt.GetClusterInfo(ctx, &apb.GetClusterInfoRequest{})
		if err != nil {
			return fmt.Errorf("GetClusterInfo: %w", err)
		}

		// Ensure that the expected node count is present.
		nodes := res.ClusterDirectory.Nodes
		if want, got := clusterOptions.NumNodes, len(nodes); want != got {
			return fmt.Errorf("wanted %d nodes in cluster directory, got %d", want, got)
		}

		// Ensure the nodes have the expected addresses.
		addresses := make(map[string]bool)
		for _, n := range nodes {
			if len(n.Addresses) != 1 {
				return fmt.Errorf("node %s has no addresss", n.Id)
			}
			address := n.Addresses[0].Host
			addresses[address] = true
		}

		for _, address := range []string{"10.1.0.2", "10.1.0.3"} {
			if !addresses[address] {
				return fmt.Errorf("address %q not found in directory", address)
			}
		}
		return nil
	})
	util.TestEventual(t, "Heartbeat test successful", ctx, 20*time.Second, cluster.AllNodesHealthy)
	util.TestEventual(t, "Node rejoin successful", ctx, 60*time.Second, func(ctx context.Context) error {
		// Ensure nodes rejoin the cluster after a reboot by reboting the 1st node.
		if err := cluster.RebootNode(ctx, 1); err != nil {
			return fmt.Errorf("while rebooting a node: %w", err)
		}
		return nil
	})
	util.TestEventual(t, "Heartbeat test successful", ctx, 20*time.Second, cluster.AllNodesHealthy)
	util.TestEventual(t, "Prometheus node metrics retrieved", ctx, smallTestTimeout, func(ctx context.Context) error {
		pool := x509.NewCertPool()
		pool.AddCert(cluster.CACertificate)
		cl := http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					Certificates: []tls.Certificate{cluster.Owner},
					RootCAs:      pool,
				},
				DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
					return cluster.DialNode(ctx, addr)
				},
			},
		}
		u := url.URL{
			Scheme: "https",
			Host:   net.JoinHostPort(cluster.NodeIDs[0], allocs.PortMetrics.PortString()),
			Path:   "/metrics/node",
		}
		res, err := cl.Get(u.String())
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			return fmt.Errorf("status code %d", res.StatusCode)
		}

		body, err := io.ReadAll(res.Body)
		if err != nil {
			return err
		}
		needle := "node_uname_info"
		if !strings.Contains(string(body), needle) {
			return util.Permanent(fmt.Errorf("could not find %q in returned response", needle))
		}
		return nil
	})
}
