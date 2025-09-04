// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package network

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/mdlayher/arp"
	"github.com/mdlayher/ethernet"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"source.monogon.dev/osbase/net/dhcp4c"
)

var ethernetNull = net.HardwareAddr{0, 0, 0, 0, 0, 0}

// sendARPAnnouncements sends an ARP announcement (a form of gratuitous ARP)
// for every permanent IPv4 address configured on the interface.
// Non-permanent IPs are handled via arpAnnounceCB. This is done to update ARP
// tables on all attached hosts, which commonly has very large (hours) timeouts
// otherwise.
// The packets are crafted to bypass EVPN ARP suppression to ensure every
// attached host gets the update.
func sendARPAnnouncements(l netlink.Link) error {
	ac, err := arp.Dial(netlinkLinkToNetInterface(l))
	if err != nil {
		// If no IPv4 address is found this is not an error, just return as
		// there is nothing to do. Sadly errNoIPv4Addr is not exported, so a
		// string match has to be used.
		if strings.Contains(err.Error(), "no IPv4 address available for interface") {
			return nil
		}
		return fmt.Errorf("while creating ARP socket: %w", err)
	}
	ac.SetWriteDeadline(time.Now().Add(5 * time.Second))
	defer ac.Close()
	addrs, err := netlink.AddrList(l, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("while listing configured IPs: %w", err)
	}
	for _, addr := range addrs {
		if addr.Flags&unix.IFA_F_PERMANENT != 0 && (addr.IP.IsGlobalUnicast() || addr.IP.IsLinkLocalUnicast()) {
			addrIP, ok := netip.AddrFromSlice(addr.IP)
			if !ok {
				continue
			}
			garpPkt, err := arp.NewPacket(arp.OperationRequest, l.Attrs().HardwareAddr, addrIP, ethernetNull, addrIP)
			if err != nil {
				continue
			}
			if err := ac.WriteTo(garpPkt, ethernet.Broadcast); err != nil {
				continue
			}
		}
	}
	return nil
}

// A DHCPv4 callback function which announces acquired IPv4 addresses via
// ARP announcement.
func arpAnnounceCB(l netlink.Link) dhcp4c.LeaseCallback {
	var lastIP net.IP
	return func(lease *dhcp4c.Lease) error {
		var currentIP net.IP
		if lease != nil {
			currentIP = lease.AssignedIP
		}
		needsAnnounce := !lastIP.Equal(currentIP) && (currentIP.IsGlobalUnicast() || currentIP.IsLinkLocalUnicast())
		lastIP = currentIP
		if needsAnnounce {
			// This function is best-effort, do not return an error as that
			// can impair DHCP function.
			ac, err := arp.Dial(netlinkLinkToNetInterface(l))
			if err != nil {
				//nolint:returnerrcheck
				return nil
			}
			ac.SetWriteDeadline(time.Now().Add(5 * time.Second))
			defer ac.Close()
			addrIP, ok := netip.AddrFromSlice(currentIP)
			if !ok {
				//nolint:returnerrcheck
				return nil
			}
			garpPkt, err := arp.NewPacket(arp.OperationRequest, l.Attrs().HardwareAddr, addrIP, ethernetNull, addrIP)
			if err != nil {
				//nolint:returnerrcheck
				return nil
			}
			if err := ac.WriteTo(garpPkt, ethernet.Broadcast); err != nil {
				//nolint:returnerrcheck
				return nil
			}
		}
		return nil
	}
}
