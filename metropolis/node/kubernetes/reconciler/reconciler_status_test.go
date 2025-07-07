// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"testing"
	"time"

	"go.etcd.io/etcd/tests/v3/framework/integration"
	"google.golang.org/protobuf/proto"
	"k8s.io/client-go/kubernetes/fake"

	"source.monogon.dev/metropolis/node/core/consensus/client"
	"source.monogon.dev/metropolis/node/core/curator"
	ppb "source.monogon.dev/metropolis/node/core/curator/proto/private"
	"source.monogon.dev/metropolis/node/core/productinfo"
	cpb "source.monogon.dev/metropolis/proto/common"
	"source.monogon.dev/osbase/supervisor"
	"source.monogon.dev/version"
	vpb "source.monogon.dev/version/spec"
)

var productInfo = productinfo.Get()

// TestMinimumReleasesNotAboveMetropolisRelease tests that minimum releases
// are not above the metropolis release itself, because that would cause
// things to get stuck.
func TestMinimumReleasesNotAboveMetropolisRelease(t *testing.T) {
	if version.ReleaseLessThan(productInfo.Version.Release, minReconcilerRelease) {
		t.Errorf("Metropolis release %s is below the minimum reconciler release %s",
			version.Semver(productInfo.Version),
			version.Release(minReconcilerRelease),
		)
	}
	if version.ReleaseLessThan(productInfo.Version.Release, minApiserverRelease) {
		t.Errorf("Metropolis release %s is below the minimum apiserver release %s",
			version.Semver(productInfo.Version),
			version.Release(minApiserverRelease),
		)
	}
}

// startEtcd creates an etcd cluster and client for testing.
func startEtcd(t *testing.T) client.Namespaced {
	t.Helper()
	// Start a single-node etcd cluster.
	integration.BeforeTestExternal(t)
	cluster := integration.NewCluster(t, &integration.ClusterConfig{Size: 1})
	t.Cleanup(func() {
		cluster.Terminate(t)
	})
	// Create etcd client to test cluster.
	curEtcd, _ := client.NewLocal(cluster.Client(0)).Sub("curator")
	return curEtcd
}

func setStatus(t *testing.T, cl client.Namespaced, status *ppb.KubernetesReconcilerStatus) {
	t.Helper()
	ctx := context.Background()

	statusBytes, err := proto.Marshal(status)
	if err != nil {
		t.Fatalf("Failed to marshal status: %v", err)
	}

	_, err = cl.Put(ctx, statusKey, string(statusBytes))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
}

func makeNode(isController bool, release *vpb.Version_Release) *ppb.Node {
	node := &ppb.Node{
		Roles: &cpb.NodeRoles{},
		Status: &cpb.NodeStatus{
			Version: &vpb.Version{Release: release},
		},
	}
	if isController {
		node.Roles.KubernetesController = &cpb.NodeRoles_KubernetesController{}
	}
	return node
}

// putNode puts the node into etcd, or deletes if nil.
// It returns the etcd revision of the operation.
func putNode(t *testing.T, cl client.Namespaced, id string, node *ppb.Node) int64 {
	t.Helper()
	ctx := context.Background()

	nkey, err := curator.NodeEtcdPrefix.Key(id)
	if err != nil {
		t.Fatal(err)
	}
	if node != nil {
		nodeBytes, err := proto.Marshal(node)
		if err != nil {
			t.Fatalf("Failed to marshal node: %v", err)
		}
		resp, err := cl.Put(ctx, nkey, string(nodeBytes))
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		return resp.Header.Revision
	} else {
		resp, err := cl.Delete(ctx, nkey)
		if err != nil {
			t.Fatalf("Delete: %v", err)
		}
		return resp.Header.Revision
	}
}

// TestWaitReady tests that WaitReady does not return too early, and the test
// will time out if WaitReady fails to return when it is supposed to.
func TestWaitReady(t *testing.T) {
	cl := startEtcd(t)
	s := Service{
		Etcd: cl,
	}

	isReady := make(chan struct{})
	supervisor.TestHarness(t, func(ctx context.Context) error {
		err := s.WaitReady(ctx)
		if err != nil {
			t.Error(err)
		}
		close(isReady)
		supervisor.Signal(ctx, supervisor.SignalHealthy)
		supervisor.Signal(ctx, supervisor.SignalDone)
		return nil
	})

	// status does not exist.
	time.Sleep(10 * time.Millisecond)

	// Version is too old.
	setStatus(t, cl, &ppb.KubernetesReconcilerStatus{
		State: ppb.KubernetesReconcilerStatus_STATE_DONE,
		Version: &vpb.Version{
			Release: &vpb.Version_Release{Major: 0, Minor: 0, Patch: 0},
		},
		MinimumCompatibleRelease: &vpb.Version_Release{Major: 0, Minor: 0, Patch: 0},
	})
	time.Sleep(10 * time.Millisecond)

	// MinimumCompatibleRelease is too new.
	setStatus(t, cl, &ppb.KubernetesReconcilerStatus{
		State: ppb.KubernetesReconcilerStatus_STATE_DONE,
		Version: &vpb.Version{
			Release: &vpb.Version_Release{Major: 10000, Minor: 0, Patch: 0},
		},
		MinimumCompatibleRelease: &vpb.Version_Release{Major: 10000, Minor: 0, Patch: 0},
	})
	time.Sleep(10 * time.Millisecond)

	select {
	case <-isReady:
		t.Fatal("WaitReady returned too early.")
	default:
	}

	// Now set the status to something compatible.
	setStatus(t, cl, &ppb.KubernetesReconcilerStatus{
		State: ppb.KubernetesReconcilerStatus_STATE_DONE,
		Version: &vpb.Version{
			Release: &vpb.Version_Release{Major: 10000, Minor: 0, Patch: 0},
		},
		MinimumCompatibleRelease: productInfo.Version.Release,
	})

	<-isReady
}

// TestWatchNodes ensures that WatchNodes always updates releases correctly
// as nodes are changed in various ways.
func TestWatchNodes(t *testing.T) {
	ctx := context.Background()
	cl := startEtcd(t)
	s := Service{
		Etcd: cl,
	}
	w := s.releases.Watch()
	defer w.Close()

	expectReleases := func(expectMin, expectMax string, expectRev int64) {
		t.Helper()
		releases, err := w.Get(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if actualMin := version.Release(releases.minRelease); actualMin != expectMin {
			t.Fatalf("Expected minimum release %s, got %s", expectMin, actualMin)
		}
		if actualMax := version.Release(releases.maxRelease); actualMax != expectMax {
			t.Fatalf("Expected maximum release %s, got %s", expectMax, actualMax)
		}
		if releases.revision != expectRev {
			t.Fatalf("Expected revision %v, got %v", expectRev, releases.revision)
		}
	}

	putNode(t, cl, "a1", makeNode(true, &vpb.Version_Release{Major: 0, Minor: 0, Patch: 2}))
	putNode(t, cl, "a2", makeNode(true, &vpb.Version_Release{Major: 0, Minor: 0, Patch: 2}))
	putNode(t, cl, "a3", makeNode(true, &vpb.Version_Release{Major: 0, Minor: 0, Patch: 2}))
	putNode(t, cl, "b", makeNode(true, &vpb.Version_Release{Major: 0, Minor: 0, Patch: 3}))
	rev := putNode(t, cl, "c", makeNode(true, &vpb.Version_Release{Major: 10000, Minor: 0, Patch: 0}))

	supervisor.TestHarness(t, s.watchNodes)
	expectReleases("0.0.2", "10000.0.0", rev)
	// Node a1 is no longer a Kubernetes controller.
	rev = putNode(t, cl, "a1", makeNode(false, &vpb.Version_Release{Major: 0, Minor: 0, Patch: 2}))
	expectReleases("0.0.2", "10000.0.0", rev)
	// Node a2 is deleted.
	rev = putNode(t, cl, "a2", nil)
	expectReleases("0.0.2", "10000.0.0", rev)
	// Node a3 changes release. Now, the minimum should change.
	rev = putNode(t, cl, "a3", makeNode(true, &vpb.Version_Release{Major: 0, Minor: 0, Patch: 4}))
	expectReleases("0.0.3", "10000.0.0", rev)
}

// TestService tests the entire service, checking that it reconciles
// only in situations where it should.
func TestService(t *testing.T) {
	reconcileWait = 10 * time.Millisecond
	cl := startEtcd(t)
	clientset := fake.NewSimpleClientset()
	s := Service{
		Etcd:      cl,
		ClientSet: clientset,
		NodeID:    "testnode",
	}

	// This node is newer than the local node, election should not start.
	putNode(t, cl, "a", makeNode(true, &vpb.Version_Release{Major: 10000, Minor: 0, Patch: 0}))

	cancelService, _ := supervisor.TestHarness(t, s.Run)

	time.Sleep(50 * time.Millisecond)
	if len(clientset.Actions()) != 0 {
		t.Fatal("Actions shouldn't have been performed yet.")
	}

	// The status allows a too old node to start.
	setStatus(t, cl, &ppb.KubernetesReconcilerStatus{
		State: ppb.KubernetesReconcilerStatus_STATE_DONE,
		Version: &vpb.Version{
			Release: &vpb.Version_Release{Major: 0, Minor: 0, Patch: 2},
		},
		MinimumCompatibleRelease: &vpb.Version_Release{Major: 0, Minor: 0, Patch: 2},
	})

	// This node is too old, before minApiserverRelease.
	putNode(t, cl, "a", makeNode(true, &vpb.Version_Release{Major: 0, Minor: 0, Patch: 2}))

	// watch-releases restarts with 500 ms backoff + randomization, so wait 1s.
	time.Sleep(time.Second)
	if len(clientset.Actions()) != 0 {
		t.Fatal("Actions shouldn't have been performed yet.")
	}

	// Upgrade the node.
	putNode(t, cl, "a", makeNode(true, minApiserverRelease))

	// Wait for status to be set.
	waitForActions := func() {
		isReady := make(chan struct{})
		supervisor.TestHarness(t, func(ctx context.Context) error {
			err := s.WaitReady(ctx)
			if err != nil {
				t.Error(err)
			}
			close(isReady)
			supervisor.Signal(ctx, supervisor.SignalHealthy)
			supervisor.Signal(ctx, supervisor.SignalDone)
			return nil
		})
		<-isReady

		if len(clientset.Actions()) == 0 {
			t.Fatal("Actions should have been performed.")
		}
		clientset.ClearActions()
	}
	waitForActions()

	// The status does not allow a too old node to start.
	setStatus(t, cl, &ppb.KubernetesReconcilerStatus{
		State: ppb.KubernetesReconcilerStatus_STATE_DONE,
		Version: &vpb.Version{
			Release: &vpb.Version_Release{Major: 0, Minor: 0, Patch: 2},
		},
		MinimumCompatibleRelease: &vpb.Version_Release{Major: 10000, Minor: 0, Patch: 0},
	})

	// This node is too old, before minApiserverRelease. But because it is not
	// allowed to start, the reconciler is not blocked.
	putNode(t, cl, "a", makeNode(true, &vpb.Version_Release{Major: 0, Minor: 0, Patch: 2}))

	// Start another instance. The old node is still leader.
	supervisor.TestHarness(t, s.Run)

	time.Sleep(50 * time.Millisecond)
	if len(clientset.Actions()) != 0 {
		t.Fatal("Actions shouldn't have been performed yet.")
	}

	// Stop the first instance. Now the second instance should get elected.
	cancelService()
	waitForActions()
}
