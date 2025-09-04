// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Package callback contains minimal callbacks for configuring the kernel with
// options received over DHCP.
//
// These directly configure the relevant kernel subsytems and need to own
// certain parts of them as documented on a per- callback basis to make sure
// that they can recover from restarts and crashes of the DHCP client.
// The callbacks in here are not suitable for use in advanced network scenarios
// like running multiple DHCP clients per interface via ClientIdentifier or
// when running an external FIB manager. In these cases it's advised to extract
// the necessary information from the lease in your own callback and
// communicate it directly to the responsible entity.
package callback

import (
	"fmt"
	"math"
	"net"
	"os"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"source.monogon.dev/osbase/net/dhcp4c"
)

// Compose can be used to chain multiple callbacks
func Compose(callbacks ...dhcp4c.LeaseCallback) dhcp4c.LeaseCallback {
	return func(lease *dhcp4c.Lease) error {
		for _, cb := range callbacks {
			if err := cb(lease); err != nil {
				return err
			}
		}
		return nil
	}
}

func isIPNetEqual(a, b *net.IPNet) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	aOnes, aBits := a.Mask.Size()
	bOnes, bBits := b.Mask.Size()
	return a.IP.Equal(b.IP) && aOnes == bOnes && aBits == bBits
}

// ManageIP sets up and tears down the assigned IP address. It takes exclusive
// ownership of all IPv4 addresses on the given interface which do not have
// IFA_F_PERMANENT set, so it's not possible to run multiple dynamic addressing
// clients on a single interface.
func ManageIP(iface netlink.Link) dhcp4c.LeaseCallback {
	return func(lease *dhcp4c.Lease) error {
		newNet := lease.IPNet()

		addrs, err := netlink.AddrList(iface, netlink.FAMILY_V4)
		if err != nil {
			return fmt.Errorf("netlink failed to list addresses: %w", err)
		}

		for _, addr := range addrs {
			if addr.Flags&unix.IFA_F_PERMANENT == 0 {
				// Linux identifies addreses by IP, mask and peer (see
				// net/ipv4/devinet.find_matching_ifa in Linux 5.10).
				// So don't touch addresses which match on these properties as
				// AddrReplace will atomically reconfigure them anyways without
				// interrupting things.
				if isIPNetEqual(addr.IPNet, newNet) && addr.Peer == nil && lease != nil {
					continue
				}

				if err := netlink.AddrDel(iface, &addr); !os.IsNotExist(err) && err != nil {
					return fmt.Errorf("failed to delete address: %w", err)
				}
			}
		}

		if lease != nil {
			remainingLifetimeSecs := int(math.Ceil(time.Until(lease.ExpiresAt).Seconds()))
			newBroadcastIP := dhcpv4.GetIP(dhcpv4.OptionBroadcastAddress, lease.Options)
			if err := netlink.AddrReplace(iface, &netlink.Addr{
				IPNet:       newNet,
				ValidLft:    remainingLifetimeSecs,
				PreferedLft: remainingLifetimeSecs,
				Broadcast:   newBroadcastIP,
			}); err != nil {
				return fmt.Errorf("failed to update address: %w", err)
			}
		}
		return nil
	}
}

// ManageRoutes installs and removes routes according to the current DHCP lease,
// including the default route (if any).
// It takes ownership of all RTPROTO_DHCP routes on the given interface, so it's
// not possible to run multiple DHCP clients on the given interface.
func ManageRoutes(iface netlink.Link) dhcp4c.LeaseCallback {
	return func(lease *dhcp4c.Lease) error {
		newRoutes := lease.Routes()

		dhcpRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{
			Protocol:  unix.RTPROT_DHCP,
			LinkIndex: iface.Attrs().Index,
		}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_PROTOCOL)
		if err != nil {
			return fmt.Errorf("netlink failed to list routes: %w", err)
		}
		for _, route := range dhcpRoutes {
			// Don't remove routes which can be atomically replaced by
			// RouteReplace to prevent potential traffic disruptions.
			//
			// This is O(n^2) but the number of routes is bounded by the size
			// of a DHCP packet (around 100 routes). Sorting both would be
			// be marginally faster for large amounts of routes only and in 99%
			// of cases it's going to be <5 routes.
			var found bool
			for _, newRoute := range newRoutes {
				if isIPNetEqual(newRoute.Dest, route.Dst) {
					found = true
					break
				}
			}
			if !found {
				err := netlink.RouteDel(&route)
				if !os.IsNotExist(err) && err != nil {
					return fmt.Errorf("failed to delete DHCP route: %w", err)
				}
			}
		}

		for _, route := range newRoutes {
			newRoute := netlink.Route{
				Protocol:  unix.RTPROT_DHCP,
				Dst:       route.Dest,
				Gw:        route.Router,
				Src:       lease.AssignedIP,
				LinkIndex: iface.Attrs().Index,
				Scope:     netlink.SCOPE_UNIVERSE,
			}
			// Routes with a non-L3 gateway are link-scoped
			if route.Router.IsUnspecified() {
				newRoute.Scope = netlink.SCOPE_LINK
			}
			err := netlink.RouteReplace(&newRoute)
			if err != nil {
				return fmt.Errorf("failed to add %s: %w", route, err)
			}
		}
		return nil
	}
}
