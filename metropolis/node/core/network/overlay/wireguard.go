// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package overlay

import (
	"fmt"
	"net"
	"os"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	common "source.monogon.dev/metropolis/node"
	ipb "source.monogon.dev/metropolis/node/core/curator/proto/api"
	"source.monogon.dev/metropolis/node/core/localstorage"
)

const (
	// clusterNetDevicename is the name of the WireGuard interface that will be
	// created in the host network namespace.
	clusterNetDeviceName = "clusternet"
)

// wireguard decouples the cluster networking service from actual mutations
// performed in the local Linux networking namespace. This is mostly done to help
// in testing the cluster networking service.
//
// Because it's effectively just a mockable interface, see the actual
// localWireguard method implementations for documentation.
type wireguard interface {
	ensureOnDiskKey(dir *localstorage.DataKubernetesClusterNetworkingDirectory) error
	setup(clusterNet *net.IPNet) error
	configurePeers(nodes []*ipb.Node) error
	unconfigurePeer(n *ipb.Node) error
	key() wgtypes.Key
	close()
}

type localWireguard struct {
	wgClient *wgctrl.Client
	privKey  wgtypes.Key
}

// ensureOnDiskKey loads the private key from disk or (if none exists) generates
// one and persists it. The resulting key is then saved into the localWireguard
// instance.
func (s *localWireguard) ensureOnDiskKey(dir *localstorage.DataKubernetesClusterNetworkingDirectory) error {
	keyRaw, err := dir.Key.Read()
	if os.IsNotExist(err) {
		key, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			return fmt.Errorf("when generating key: %w", err)
		}
		if err := dir.Key.Write([]byte(key.String()), 0600); err != nil {
			return fmt.Errorf("save failed: %w", err)
		}
		s.privKey = key
		return nil
	} else if err != nil {
		return fmt.Errorf("load failed: %w", err)
	}

	key, err := wgtypes.ParseKey(string(keyRaw))
	if err != nil {
		return fmt.Errorf("invalid private key in file: %w", err)
	}
	s.privKey = key
	return nil
}

// setup the local network namespace by creating a WireGuard interface and adding
// a clusterNet route to it. If a matching WireGuard interface already exists in
// the system, it is first deleted.
//
// ensureOnDiskKey must be called before calling this function.
func (s *localWireguard) setup(clusterNet *net.IPNet) error {
	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("could not list links: %w", err)
	}
	for _, link := range links {
		if link.Attrs().Name != clusterNetDeviceName {
			continue
		}
		if err := netlink.LinkDel(link); err != nil {
			return fmt.Errorf("could not remove existing clusternet link: %w", err)
		}
	}

	wgInterface := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: clusterNetDeviceName, Flags: net.FlagUp, Group: common.LinkGroupOverlay}}
	if err := netlink.LinkAdd(wgInterface); err != nil {
		return fmt.Errorf("when adding network interface: %w", err)
	}

	wgClient, err := wgctrl.New()
	if err != nil {
		return fmt.Errorf("when creating wireguard client: %w", err)
	}
	s.wgClient = wgClient

	listenPort := int(common.WireGuardPort)
	if err := s.wgClient.ConfigureDevice(clusterNetDeviceName, wgtypes.Config{
		PrivateKey: &s.privKey,
		ListenPort: &listenPort,
	}); err != nil {
		return fmt.Errorf("when setting up device: %w", err)
	}

	if err := netlink.RouteAdd(&netlink.Route{
		Dst:       clusterNet,
		LinkIndex: wgInterface.Index,
		Protocol:  netlink.RouteProtocol(common.ProtocolOverlay),
	}); err != nil && !os.IsExist(err) {
		return fmt.Errorf("when creating cluster route: %w", err)
	}
	return nil
}

// configurePeers creates or updates peers on the local wireguard interface
// based on the given nodes.
func (s *localWireguard) configurePeers(nodes []*ipb.Node) error {
	var configs []wgtypes.PeerConfig
	for i, n := range nodes {
		if s.privKey.PublicKey().String() == n.Clusternet.WireguardPubkey {
			// Node doesn't need to connect to itself
			continue
		}
		pubkeyParsed, err := wgtypes.ParseKey(n.Clusternet.WireguardPubkey)
		if err != nil {
			return fmt.Errorf("node %d: failed to parse public-key %q: %w", i, n.Clusternet.WireguardPubkey, err)
		}
		addressParsed := net.ParseIP(n.Status.ExternalAddress)
		if addressParsed == nil {
			return fmt.Errorf("node %d: failed to parse address %q: %w", i, n.Status.ExternalAddress, err)
		}
		var allowedIPs []net.IPNet
		for _, prefix := range n.Clusternet.Prefixes {
			_, podNet, err := net.ParseCIDR(prefix.Cidr)
			if err != nil {
				// Just eat the parse error. Not much we can do here. We have enough validation
				// in the rest of the system that we shouldn't ever reach this.
				continue
			}
			allowedIPs = append(allowedIPs, *podNet)
		}
		endpoint := net.UDPAddr{Port: int(common.WireGuardPort), IP: addressParsed}
		configs = append(configs, wgtypes.PeerConfig{
			PublicKey:         pubkeyParsed,
			Endpoint:          &endpoint,
			ReplaceAllowedIPs: true,
			AllowedIPs:        allowedIPs,
		})
	}

	err := s.wgClient.ConfigureDevice(clusterNetDeviceName, wgtypes.Config{
		Peers: configs,
	})
	if err != nil {
		return fmt.Errorf("failed to configure WireGuard peers: %w", err)
	}
	return nil
}

// unconfigurePeer removes the peer from the local WireGuard interface based on
// the given node. If no peer existed matching the given node, this operation is
// a no-op.
func (s *localWireguard) unconfigurePeer(n *ipb.Node) error {
	pubkeyParsed, err := wgtypes.ParseKey(n.Clusternet.WireguardPubkey)
	if err != nil {
		return fmt.Errorf("failed to parse public-key %q: %w", n.Clusternet.WireguardPubkey, err)
	}

	err = s.wgClient.ConfigureDevice(clusterNetDeviceName, wgtypes.Config{
		Peers: []wgtypes.PeerConfig{{
			PublicKey: pubkeyParsed,
			Remove:    true,
		}},
	})
	if err != nil {
		return fmt.Errorf("failed to delete WireGuard peer: %w", err)
	}
	return nil
}

func (s *localWireguard) key() wgtypes.Key {
	return s.privKey
}

// close cleans up after the wireguard client, but does _not_ remove the
// interface or peers.
func (s *localWireguard) close() {
	s.wgClient.Close()
	s.wgClient = nil
}
