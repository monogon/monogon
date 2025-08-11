// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package overlay

import (
	"fmt"
	"net"
	"os"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	common "source.monogon.dev/metropolis/node"
	"source.monogon.dev/metropolis/node/core/localstorage"
	"source.monogon.dev/metropolis/node/core/localstorage/declarative"
	"source.monogon.dev/metropolis/node/core/network/ipam"
	"source.monogon.dev/metropolis/test/util"
	"source.monogon.dev/osbase/event/memory"
	"source.monogon.dev/osbase/supervisor"

	apb "source.monogon.dev/metropolis/node/core/curator/proto/api"
	cpb "source.monogon.dev/metropolis/proto/common"
)

// fakeWireguard implements wireguard while keeping peer information internally.
type fakeWireguard struct {
	k wgtypes.Key

	muNodes        sync.Mutex
	nodes          map[string]*apb.Node
	failNextUpdate bool
}

func (f *fakeWireguard) ensureOnDiskKey(_ *localstorage.DataKubernetesClusterNetworkingDirectory) error {
	f.k, _ = wgtypes.GeneratePrivateKey()
	return nil
}

func (f *fakeWireguard) setup(clusterNet *net.IPNet) error {
	f.muNodes.Lock()
	defer f.muNodes.Unlock()
	f.nodes = make(map[string]*apb.Node)
	return nil
}

func (f *fakeWireguard) configurePeers(nodes []*apb.Node) error {
	f.muNodes.Lock()
	defer f.muNodes.Unlock()

	if f.failNextUpdate {
		f.failNextUpdate = false
		return fmt.Errorf("synthetic test failure")
	}

	for _, n := range nodes {
		f.nodes[n.Id] = n
	}
	return nil
}

func (f *fakeWireguard) unconfigurePeer(node *apb.Node) error {
	f.muNodes.Lock()
	defer f.muNodes.Unlock()
	delete(f.nodes, node.Id)
	return nil
}

func (f *fakeWireguard) key() wgtypes.Key {
	return f.k
}

func (f *fakeWireguard) close() {
}

// TestClusternetBasic exercises clusternet with a fake curator and fake
// wireguard, trying to exercise as many edge cases as possible.
func TestClusternetBasic(t *testing.T) {
	key1, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("Failed to generate private key: %v", err)
	}
	key2, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("Failed to generate private key: %v", err)
	}

	cur, cl := util.MakeTestCurator(t)
	defer cl.Close()
	curator := apb.NewCuratorClient(cl)

	var nval memory.Value[*common.NetStatus]

	var podNetwork memory.Value[*ipam.Prefixes]
	wg := &fakeWireguard{}
	svc := Service{
		Curator: curator,
		ClusterNet: net.IPNet{
			IP:   net.IP([]byte{10, 10, 0, 0}),
			Mask: net.IPv4Mask(255, 255, 0, 0),
		},
		DataDirectory:             nil,
		LocalKubernetesPodNetwork: &podNetwork,
		Network:                   &nval,

		wg: wg,
	}
	supervisor.TestHarness(t, svc.Run)

	checkState := func(nodes map[string]*apb.Node) error {
		t.Helper()
		wg.muNodes.Lock()
		defer wg.muNodes.Unlock()
		for nid, n := range nodes {
			n2, ok := wg.nodes[nid]
			if !ok {
				return fmt.Errorf("node %q missing in programmed peers", nid)
			}
			if got, want := n2.Clusternet.WireguardPubkey, n.Clusternet.WireguardPubkey; got != want {
				return fmt.Errorf("node %q pubkey mismatch: %q in programmed peers, %q wanted", nid, got, want)
			}
			if got, want := n2.Status.ExternalAddress, n.Status.ExternalAddress; got != want {
				return fmt.Errorf("node %q address mismatch: %q in programmed peers, %q wanted", nid, got, want)
			}
			var p, p2 []string
			for _, prefix := range n.Clusternet.Prefixes {
				p = append(p, prefix.Cidr)
			}
			for _, prefix := range n2.Clusternet.Prefixes {
				p2 = append(p2, prefix.Cidr)
			}
			sort.Strings(p)
			sort.Strings(p2)
			if !slices.Equal(p, p2) {
				return fmt.Errorf("node %q prefixes mismatch: %v in programmed peers, %v wanted", nid, p2, p)
			}
		}
		for nid := range wg.nodes {
			if _, ok := nodes[nid]; !ok {
				return fmt.Errorf("node %q present in programmed peers", nid)
			}
		}
		return nil
	}

	assertStateEventual := func(nodes map[string]*apb.Node) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := checkState(nodes)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal(err)
			}
		}

	}

	// Start with a single node.
	cur.NodeWithPrefixes(key1, "metropolis-fake-1", "1.2.3.4")
	assertStateEventual(map[string]*apb.Node{
		"metropolis-fake-1": {
			Status: &cpb.NodeStatus{
				ExternalAddress: "1.2.3.4",
			},
			Clusternet: &cpb.NodeClusterNetworking{
				WireguardPubkey: key1.PublicKey().String(),
			},
		},
	})
	// Change the node's peer address.
	cur.NodeWithPrefixes(key1, "metropolis-fake-1", "1.2.3.5")
	assertStateEventual(map[string]*apb.Node{
		"metropolis-fake-1": {
			Status: &cpb.NodeStatus{
				ExternalAddress: "1.2.3.5",
			},
			Clusternet: &cpb.NodeClusterNetworking{
				WireguardPubkey: key1.PublicKey().String(),
			},
		},
	})
	// Add another node.
	cur.NodeWithPrefixes(key2, "metropolis-fake-2", "1.2.3.6")
	assertStateEventual(map[string]*apb.Node{
		"metropolis-fake-1": {
			Status: &cpb.NodeStatus{
				ExternalAddress: "1.2.3.5",
			},
			Clusternet: &cpb.NodeClusterNetworking{
				WireguardPubkey: key1.PublicKey().String(),
			},
		},
		"metropolis-fake-2": {
			Status: &cpb.NodeStatus{
				ExternalAddress: "1.2.3.6",
			},
			Clusternet: &cpb.NodeClusterNetworking{
				WireguardPubkey: key2.PublicKey().String(),
			},
		},
	})
	// Add some prefixes to both nodes, but fail the next configurePeers call.
	wg.muNodes.Lock()
	wg.failNextUpdate = true
	wg.muNodes.Unlock()
	cur.NodeWithPrefixes(key1, "metropolis-fake-1", "1.2.3.5", "10.100.10.0/24", "10.100.20.0/24")
	cur.NodeWithPrefixes(key2, "metropolis-fake-2", "1.2.3.6", "10.100.30.0/24", "10.100.40.0/24")
	assertStateEventual(map[string]*apb.Node{
		"metropolis-fake-1": {
			Status: &cpb.NodeStatus{
				ExternalAddress: "1.2.3.5",
			},
			Clusternet: &cpb.NodeClusterNetworking{
				WireguardPubkey: key1.PublicKey().String(),
				// No prefixes as the call failed.
			},
		},
		"metropolis-fake-2": {
			Status: &cpb.NodeStatus{
				ExternalAddress: "1.2.3.6",
			},
			Clusternet: &cpb.NodeClusterNetworking{
				WireguardPubkey: key2.PublicKey().String(),
				Prefixes: []*cpb.NodeClusterNetworking_Prefix{
					{Cidr: "10.100.30.0/24"},
					{Cidr: "10.100.40.0/24"},
				},
			},
		},
	})
	// Delete one of the nodes.
	cur.DeleteNode("metropolis-fake-1")
	assertStateEventual(map[string]*apb.Node{
		"metropolis-fake-2": {
			Status: &cpb.NodeStatus{
				ExternalAddress: "1.2.3.6",
			},
			Clusternet: &cpb.NodeClusterNetworking{
				WireguardPubkey: key2.PublicKey().String(),
				Prefixes: []*cpb.NodeClusterNetworking_Prefix{
					{Cidr: "10.100.30.0/24"},
					{Cidr: "10.100.40.0/24"},
				},
			},
		},
	})
}

// TestWireguardImplementation makes sure localWireguard behaves as expected.
func TestWireguardIntegration(t *testing.T) {
	if os.Getenv("IN_KTEST") != "true" {
		t.Skip("Not in ktest")
	}

	root := &localstorage.Root{}
	tmp, err := os.MkdirTemp("", "clusternet")
	if err != nil {
		t.Fatal(err)
	}
	err = declarative.PlaceFS(root, tmp)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(root.Data.Kubernetes.ClusterNetworking.FullPath(), 0700)
	wg := &localWireguard{}

	// Ensure key once and make note of it.
	if err := wg.ensureOnDiskKey(&root.Data.Kubernetes.ClusterNetworking); err != nil {
		t.Fatalf("Could not ensure wireguard key: %v", err)
	}
	key := wg.key().String()
	// Do it again, and make sure the key hasn't changed.
	wg = &localWireguard{}
	if err := wg.ensureOnDiskKey(&root.Data.Kubernetes.ClusterNetworking); err != nil {
		t.Fatalf("Could not ensure wireguard key second time: %v", err)
	}
	if want, got := key, wg.key().String(); want != got {
		t.Fatalf("Key changed, was %q, became %q", want, got)
	}

	// Setup the interface.
	cnet := net.IPNet{
		IP:   net.IP([]byte{10, 10, 0, 0}),
		Mask: net.IPv4Mask(255, 255, 0, 0),
	}
	if err := wg.setup(&cnet); err != nil {
		t.Fatalf("Failed to setup interface: %v", err)
	}
	// Do it again.
	wg.close()
	if err := wg.setup(&cnet); err != nil {
		t.Fatalf("Failed to setup interface second time: %v", err)
	}

	// Check that the key and listen port are configured correctly.
	wgClient, err := wgctrl.New()
	if err != nil {
		t.Fatalf("Failed to create wireguard client: %v", err)
	}
	wgDev, err := wgClient.Device(clusterNetDeviceName)
	if err != nil {
		t.Fatalf("Failed to connect to netlink's WireGuard config endpoint: %v", err)
	}
	if want, got := key, wgDev.PrivateKey.String(); want != got {
		t.Errorf("Wireguard key mismatch, wanted %q, got %q", want, got)
	}
	if want, got := int(common.WireGuardPort), wgDev.ListenPort; want != got {
		t.Errorf("Wireguard port mismatch, wanted %d, got %d", want, got)
	}

	// Add some peers and check that we got them.
	pkeys := make([]wgtypes.Key, 2)
	pkeys[0], err = wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("Failed to generate private key: %v", err)
	}
	pkeys[1], err = wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("Failed to generate private key: %v", err)
	}
	err = wg.configurePeers([]*apb.Node{
		{
			Id: "test-0",
			Status: &cpb.NodeStatus{
				ExternalAddress: "10.100.0.1",
			},
			Clusternet: &cpb.NodeClusterNetworking{
				WireguardPubkey: pkeys[0].PublicKey().String(),
				Prefixes: []*cpb.NodeClusterNetworking_Prefix{
					{Cidr: "10.0.0.0/24"},
					{Cidr: "10.0.1.0/24"},
				},
			},
		},
		{
			Id: "test-1",
			Status: &cpb.NodeStatus{
				ExternalAddress: "10.100.1.1",
			},
			Clusternet: &cpb.NodeClusterNetworking{
				WireguardPubkey: pkeys[1].PublicKey().String(),
				Prefixes: []*cpb.NodeClusterNetworking_Prefix{
					{Cidr: "10.1.0.0/24"},
					{Cidr: "10.1.1.0/24"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Configuring peers failed: %v", err)
	}

	wgDev, err = wgClient.Device(clusterNetDeviceName)
	if err != nil {
		t.Fatalf("Failed to connect to netlink's WireGuard config endpoint: %v", err)
	}
	if want, got := 2, len(wgDev.Peers); want != got {
		t.Errorf("Wanted %d peers, got %d", want, got)
	} else {
		for i := 0; i < 2; i++ {
			if want, got := pkeys[i].PublicKey().String(), wgDev.Peers[i].PublicKey.String(); want != got {
				t.Errorf("Peer %d should have key %q, got %q", i, want, got)
			}
			if want, got := fmt.Sprintf("10.100.%d.1:%s", i, common.WireGuardPort.PortString()), wgDev.Peers[i].Endpoint.String(); want != got {
				t.Errorf("Peer %d should have endpoint %q, got %q", i, want, got)
			}
			if want, got := 2, len(wgDev.Peers[i].AllowedIPs); want != got {
				t.Errorf("Peer %d should have %d peers, got %d", i, want, got)
			} else {
				for j := 0; j < 2; j++ {
					if want, got := fmt.Sprintf("10.%d.%d.0/24", i, j), wgDev.Peers[i].AllowedIPs[j].String(); want != got {
						t.Errorf("Peer %d should have allowed ip %d %q, got %q", i, j, want, got)
					}
				}
			}
		}
	}

	// Update one of the peers and check that things got applied.
	err = wg.configurePeers([]*apb.Node{
		{
			Id: "test-0",
			Status: &cpb.NodeStatus{
				ExternalAddress: "10.100.0.3",
			},
			Clusternet: &cpb.NodeClusterNetworking{
				WireguardPubkey: pkeys[0].PublicKey().String(),
				Prefixes: []*cpb.NodeClusterNetworking_Prefix{
					{Cidr: "10.0.0.0/24"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Failed to update peer: %v", err)
	}
	wgDev, err = wgClient.Device(clusterNetDeviceName)
	if err != nil {
		t.Fatalf("Failed to connect to netlink's WireGuard config endpoint: %v", err)
	}
	if want, got := 2, len(wgDev.Peers); want != got {
		t.Errorf("Wanted %d peers, got %d", want, got)
	} else {
		if want, got := pkeys[0].PublicKey().String(), wgDev.Peers[0].PublicKey.String(); want != got {
			t.Errorf("Peer 0 should have key %q, got %q", want, got)
		}
		if want, got := fmt.Sprintf("10.100.0.3:%s", common.WireGuardPort.PortString()), wgDev.Peers[0].Endpoint.String(); want != got {
			t.Errorf("Peer 0 should have endpoint %q, got %q", want, got)
		}
		if want, got := 1, len(wgDev.Peers[0].AllowedIPs); want != got {
			t.Errorf("Peer 0 should have %d peers, got %d", want, got)
		} else {
			if want, got := "10.0.0.0/24", wgDev.Peers[0].AllowedIPs[0].String(); want != got {
				t.Errorf("Peer 0 should have allowed ip 0 %q, got %q", want, got)
			}
		}
	}

	// Remove one of the peers and make sure it's gone.
	err = wg.unconfigurePeer(&apb.Node{
		Clusternet: &cpb.NodeClusterNetworking{
			WireguardPubkey: pkeys[0].PublicKey().String(),
		},
	})
	if err != nil {
		t.Fatalf("Failed to unconfigure peer: %v", err)
	}
	err = wg.unconfigurePeer(&apb.Node{
		Clusternet: &cpb.NodeClusterNetworking{
			WireguardPubkey: pkeys[0].PublicKey().String(),
		},
	})
	if err != nil {
		t.Fatalf("Failed to unconfigure peer a second time: %v", err)
	}
	wgDev, err = wgClient.Device(clusterNetDeviceName)
	if err != nil {
		t.Fatalf("Failed to connect to netlink's WireGuard config endpoint: %v", err)
	}
	if want, got := 1, len(wgDev.Peers); want != got {
		t.Errorf("Wanted %d peer, got %d", want, got)
	}
}
