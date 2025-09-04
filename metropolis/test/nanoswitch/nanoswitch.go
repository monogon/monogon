// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// nanoswitch is a virtualized switch/router combo intended for testing.
// It uses the first interface as an external interface to connect to the host
// and pass traffic in and out. All other interfaces are switched together and
// served by a built-in DHCP server. Traffic from that network to the
// SLIRP/external network is SNATed as the host-side SLIRP ignores routed
// packets.
//
// It also has built-in userspace proxying support for accessing the first
// node's services, as well as a SOCKS proxy to access all nodes within the
// network.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"github.com/vishvananda/netlink"

	"source.monogon.dev/metropolis/node/allocs"
	"source.monogon.dev/osbase/bringup"
	"source.monogon.dev/osbase/net/dhcp4c"
	dhcpcb "source.monogon.dev/osbase/net/dhcp4c/callback"
	"source.monogon.dev/osbase/supervisor"
)

var (
	// HostInterfaceMAC is the MAC address the host SLIRP network interface has if it
	// is not disabled (see DisableHostNetworkInterface in MicroVMOptions)
	// ONCHANGE(//osbase/test/qemu:launch.go): constraints must be kept in sync with
	// HostInterfaceMAC.
	HostInterfaceMAC = net.HardwareAddr{0x02, 0x72, 0x82, 0xbf, 0xc3, 0x56}

	switchIP         = net.IP{10, 1, 0, 1}
	switchSubnetMask = net.CIDRMask(24, 32)
)

// defaultLeaseOptions sets the lease options needed to properly configure
// connectivity to nanoswitch.
func defaultLeaseOptions(reply *dhcpv4.DHCPv4) {
	reply.GatewayIPAddr = switchIP
	// SLIRP fake DNS server.
	reply.UpdateOption(dhcpv4.OptDNS(net.IPv4(10, 42, 0, 3)))
	reply.UpdateOption(dhcpv4.OptRouter(switchIP))
	// Make sure we exercise our DHCP client in E2E tests.
	reply.UpdateOption(dhcpv4.OptIPAddressLeaseTime(30 * time.Second))
	reply.UpdateOption(dhcpv4.OptSubnetMask(switchSubnetMask))
}

// runDHCPServer runs an extremely minimal DHCP server with most options
// hardcoded, a wrapping bump allocator for the IPs, 30 second lease timeout
// and no support for DHCP collision detection.
func runDHCPServer(link netlink.Link) supervisor.Runnable {
	currentIP := net.IP{10, 1, 0, 2}

	// Map from stringified MAC address to IP address, allowing handing out the
	// same IP to a given MAC on re-discovery.
	leases := make(map[string]net.IP)

	return func(ctx context.Context) error {
		laddr := net.UDPAddr{
			IP:   net.IPv4(0, 0, 0, 0),
			Port: 67,
		}
		server, err := server4.NewServer(link.Attrs().Name, &laddr, func(conn net.PacketConn, peer net.Addr, m *dhcpv4.DHCPv4) {
			if m == nil {
				return
			}
			reply, err := dhcpv4.NewReplyFromRequest(m)
			if err != nil {
				supervisor.Logger(ctx).Warningf("Failed to generate DHCP reply: %v", err)
				return
			}
			reply.UpdateOption(dhcpv4.OptServerIdentifier(switchIP))
			reply.ServerIPAddr = switchIP

			switch m.MessageType() {
			case dhcpv4.MessageTypeDiscover:
				reply.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeOffer))
				defaultLeaseOptions(reply)
				hwaddr := m.ClientHWAddr.String()
				// Either hand out already allocated address from leases, or allocate new.
				if ip, ok := leases[hwaddr]; ok {
					reply.YourIPAddr = ip
				} else {
					leases[hwaddr] = net.ParseIP(currentIP.String())
					reply.YourIPAddr = leases[hwaddr]
					currentIP[3]++ // Works only because it's a /24
				}
				supervisor.Logger(ctx).Infof("Replying with DHCP IP %s to %s", reply.YourIPAddr.String(), hwaddr)
			case dhcpv4.MessageTypeRequest:
				reply.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeAck))
				defaultLeaseOptions(reply)
				if m.RequestedIPAddress() != nil {
					reply.YourIPAddr = m.RequestedIPAddress()
				} else {
					reply.YourIPAddr = m.ClientIPAddr
				}
			case dhcpv4.MessageTypeRelease, dhcpv4.MessageTypeDecline:
				supervisor.Logger(ctx).Info("Ignoring Release/Decline")
			}
			if _, err := conn.WriteTo(reply.ToBytes(), peer); err != nil {
				supervisor.Logger(ctx).Warningf("Cannot reply to client: %v", err)
			}
		})
		if err != nil {
			return err
		}
		supervisor.Signal(ctx, supervisor.SignalHealthy)
		go func() {
			<-ctx.Done()
			server.Close()
		}()
		return server.Serve()
	}
}

// userspaceProxy listens on port and proxies all TCP connections to the same
// port on targetIP
func userspaceProxy(targetIP net.IP, port allocs.Port) supervisor.Runnable {
	return func(ctx context.Context) error {
		logger := supervisor.Logger(ctx)
		tcpListener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(0, 0, 0, 0), Port: int(port)})
		if err != nil {
			return err
		}
		supervisor.Signal(ctx, supervisor.SignalHealthy)
		go func() {
			<-ctx.Done()
			tcpListener.Close()
		}()
		for {
			conn, err := tcpListener.AcceptTCP()
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return err
			}
			go func(conn *net.TCPConn) {
				defer conn.Close()
				upstreamConn, err := net.DialTCP("tcp", nil, &net.TCPAddr{IP: targetIP, Port: int(port)})
				if err != nil {
					logger.Infof("Userspace proxy failed to connect to upstream: %v", err)
					return
				}
				defer upstreamConn.Close()
				go io.Copy(upstreamConn, conn)
				io.Copy(conn, upstreamConn)
			}(conn)
		}

	}
}

// addNetworkRoutes sets up routing from DHCP
func addNetworkRoutes(link netlink.Link, addr net.IPNet, gw net.IP) error {
	if err := netlink.AddrReplace(link, &netlink.Addr{IPNet: &addr}); err != nil {
		return fmt.Errorf("failed to add DHCP address to network interface \"%v\": %w", link.Attrs().Name, err)
	}

	if gw.IsUnspecified() {
		return nil
	}

	route := &netlink.Route{
		Dst:   &net.IPNet{IP: net.IPv4(0, 0, 0, 0), Mask: net.IPv4Mask(0, 0, 0, 0)},
		Gw:    gw,
		Scope: netlink.SCOPE_UNIVERSE,
	}
	if err := netlink.RouteAdd(route); err != nil {
		return fmt.Errorf("could not add default route: netlink.RouteAdd(%+v): %w", route, err)
	}
	return nil
}

// nfifname converts an interface name into 16 bytes padded with zeroes (for
// nftables)
func nfifname(n string) []byte {
	b := make([]byte, 16)
	copy(b, n+"\x00")
	return b
}

func main() {
	bringup.Runnable(root).Run()
}

func root(ctx context.Context) (err error) {
	logger := supervisor.Logger(ctx)
	logger.Info("Starting NanoSwitch, a tiny TOR switch emulator")

	c := &nftables.Conn{}

	links, err := netlink.LinkList()
	if err != nil {
		logger.Fatalf("Failed to list links: %v", err)
	}
	var externalLink netlink.Link
	var vmLinks []netlink.Link
	for _, link := range links {
		attrs := link.Attrs()
		if link.Type() == "device" && len(attrs.HardwareAddr) > 0 {
			if attrs.Flags&net.FlagUp != net.FlagUp {
				netlink.LinkSetUp(link) // Attempt to take up all ethernet links
			}
			if bytes.Equal(attrs.HardwareAddr, HostInterfaceMAC) {
				externalLink = link
			} else {
				vmLinks = append(vmLinks, link)
			}
		}
	}
	vmBridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "vmbridge", Flags: net.FlagUp}}
	if err := netlink.LinkAdd(vmBridgeLink); err != nil {
		logger.Fatalf("Failed to create vmbridge: %v", err)
	}
	for _, link := range vmLinks {
		if err := netlink.LinkSetMaster(link, vmBridgeLink); err != nil {
			logger.Fatalf("Failed to add VM interface to bridge: %v", err)
		}
		logger.Infof("Assigned interface %s to bridge", link.Attrs().Name)
	}
	if err := netlink.AddrReplace(vmBridgeLink, &netlink.Addr{IPNet: &net.IPNet{IP: switchIP, Mask: switchSubnetMask}}); err != nil {
		logger.Fatalf("Failed to assign static IP to vmbridge: %v", err)
	}
	if externalLink != nil {
		nat := c.AddTable(&nftables.Table{
			Family: nftables.TableFamilyIPv4,
			Name:   "nat",
		})

		postrouting := c.AddChain(&nftables.Chain{
			Name:     "postrouting",
			Hooknum:  nftables.ChainHookPostrouting,
			Priority: nftables.ChainPriorityNATSource,
			Table:    nat,
			Type:     nftables.ChainTypeNAT,
		})

		// Masquerade/SNAT all traffic going out of the external interface
		c.AddRule(&nftables.Rule{
			Table: nat,
			Chain: postrouting,
			Exprs: []expr.Any{
				&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
				&expr.Cmp{
					Op:       expr.CmpOpEq,
					Register: 1,
					Data:     nfifname(externalLink.Attrs().Name),
				},
				&expr.Masq{},
			},
		})

		if err := c.Flush(); err != nil {
			panic(err)
		}

		netIface := &net.Interface{
			Name:         externalLink.Attrs().Name,
			MTU:          externalLink.Attrs().MTU,
			Index:        externalLink.Attrs().Index,
			Flags:        externalLink.Attrs().Flags,
			HardwareAddr: externalLink.Attrs().HardwareAddr,
		}
		dhcpClient, err := dhcp4c.NewClient(netIface)
		if err != nil {
			logger.Fatalf("Failed to create DHCP client: %v", err)
		}
		dhcpClient.RequestedOptions = []dhcpv4.OptionCode{dhcpv4.OptionRouter}
		dhcpClient.LeaseCallback = dhcpcb.Compose(dhcpcb.ManageIP(externalLink), dhcpcb.ManageRoutes(externalLink))
		supervisor.Run(ctx, "dhcp-client", dhcpClient.Run)
		if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0644); err != nil {
			logger.Fatalf("Failed to write ip forwards: %v", err)
		}
	} else {
		logger.Info("No upstream interface detected")
	}
	supervisor.Run(ctx, "dhcp-server", runDHCPServer(vmBridgeLink))
	supervisor.Run(ctx, "proxy-cur1", userspaceProxy(net.IPv4(10, 1, 0, 2), allocs.PortCuratorService))
	supervisor.Run(ctx, "proxy-dbg1", userspaceProxy(net.IPv4(10, 1, 0, 2), allocs.PortDebugService))
	supervisor.Run(ctx, "proxy-k8s-api1", userspaceProxy(net.IPv4(10, 1, 0, 2), allocs.PortKubernetesAPI))
	supervisor.Run(ctx, "proxy-k8s-api-wrapped1", userspaceProxy(net.IPv4(10, 1, 0, 2), allocs.PortKubernetesAPIWrapped))
	supervisor.Run(ctx, "socks", runSOCKSProxy)
	supervisor.Signal(ctx, supervisor.SignalHealthy)
	supervisor.Signal(ctx, supervisor.SignalDone)
	return nil
}
