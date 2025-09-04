// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package dhcp4c

import (
	"encoding/binary"
	"net"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

// Lease represents a DHCPv4 lease. It only consists of an IP, an expiration
// timestamp and options as all other relevant parts of the message have been
// normalized into their respective options. It also contains some smart
// getters for commonly-used options which extract only valid information from
// options.
type Lease struct {
	AssignedIP net.IP
	ExpiresAt  time.Time
	Options    dhcpv4.Options
}

// SubnetMask returns the SubnetMask option or the default mask if not set or
// invalid.
// It returns nil if the lease is nil.
func (l *Lease) SubnetMask() net.IPMask {
	if l == nil {
		return nil
	}
	mask := net.IPMask(dhcpv4.GetIP(dhcpv4.OptionSubnetMask, l.Options))
	// If given mask is not valid, use the default mask.
	if _, bits := mask.Size(); bits != 32 {
		mask = l.AssignedIP.DefaultMask()
	}
	return mask
}

// IPNet returns an IPNet from the assigned IP and subnet mask.
// It returns nil if the lease is nil.
func (l *Lease) IPNet() *net.IPNet {
	if l == nil {
		return nil
	}
	return &net.IPNet{
		IP:   l.AssignedIP,
		Mask: l.SubnetMask(),
	}
}

// Routes returns all routes assigned by a DHCP answer. It combines and
// normalizes data from the Router, StaticRoutingTable and ClasslessStaticRoute
// options.
func (l *Lease) Routes() []*dhcpv4.Route {
	if l == nil {
		return nil
	}

	// Note that this is different from l.IPNet() because we care about the
	// network base address of the network instead of the assigned IP.
	assignedNet := &net.IPNet{IP: l.AssignedIP.Mask(l.SubnetMask()), Mask: l.SubnetMask()}

	// RFC 3442 Section DHCP Client Behavior:
	// If the DHCP server returns both a Classless Static Routes option and
	// a Router option, the DHCP client MUST ignore the Router option.
	// Similarly, if the DHCP server returns both a Classless Static Routes
	// option and a Static Routes option, the DHCP client MUST ignore the
	// Static Routes option.
	var routes dhcpv4.Routes
	rawCIDRRoutes := l.Options.Get(dhcpv4.OptionClasslessStaticRoute)
	if rawCIDRRoutes != nil {
		// TODO(#96): This needs a logging story
		// Ignore errors intentionally and just return what has been parsed
		_ = routes.FromBytes(rawCIDRRoutes)
		return sanitizeRoutes(routes, assignedNet)
	}
	// The Static Routes option contains legacy classful routes (i.e. routes
	// whose mask is determined by the IP of the network).
	// Each static route is expressed as a pair of IPs, the first one being
	// the destination network and the second one being the router IP.
	// See RFC 2132 Section 5.8 for further details.
	legacyRouteIPs := dhcpv4.GetIPs(dhcpv4.OptionStaticRoutingTable, l.Options)
	// Routes are only valid in pairs, cut the last one off if necessary
	if len(legacyRouteIPs)%2 != 0 {
		legacyRouteIPs = legacyRouteIPs[:len(legacyRouteIPs)-1]
	}
	for i := 0; i < len(legacyRouteIPs)/2; i++ {
		dest := legacyRouteIPs[i*2]
		if dest.IsUnspecified() {
			// RFC 2132 Section 5.8:
			// The default route (0.0.0.0) is an illegal destination for a
			// static route.
			continue
		}
		via := legacyRouteIPs[i*2+1]
		destNet := net.IPNet{
			// Apply the default mask just to make sure this is a valid route
			IP:   dest.Mask(dest.DefaultMask()),
			Mask: dest.DefaultMask(),
		}
		routes = append(routes, &dhcpv4.Route{Dest: &destNet, Router: via})
	}
	for _, r := range dhcpv4.GetIPs(dhcpv4.OptionRouter, l.Options) {
		if r.IsGlobalUnicast() || r.IsLinkLocalUnicast() {
			routes = append(routes, &dhcpv4.Route{
				Dest:   &net.IPNet{IP: net.IPv4zero, Mask: net.IPv4Mask(0, 0, 0, 0)},
				Router: r,
			})
			// Only one default router can exist, exit after the first one
			break
		}
	}
	return sanitizeRoutes(routes, assignedNet)
}

// sanitizeRoutes filters the list of routes by removing routes that are
// obviously invalid. It filters out routes according to the following criteria:
//  1. The route is not an interface route and its router is not a unicast or
//     link-local address.
//  2. Each route's router must be reachable according to the routes listed
//     before it and the assigned network.
//  3. The network mask must consist of all-ones followed by all-zeros. Non-
//     contiguous routes are not allowed.
//  4. If multiple routes match the same destination, only the first one is kept.
//  5. Routes covering the loopback IP space (127.0.0.0/8) will be ignored if
//     they are smaller than a /9 to prevent them from interfering with loopback
//     IPs.
func sanitizeRoutes(routes []*dhcpv4.Route, assignedNet *net.IPNet) []*dhcpv4.Route {
	var saneRoutes []*dhcpv4.Route
	for _, route := range routes {
		if route.Router != nil && !route.Router.IsUnspecified() {
			if !route.Router.IsGlobalUnicast() && !route.Router.IsLinkLocalUnicast() {
				// Ignore non-unicast routers
				continue
			}
			reachable := false
			for _, r := range saneRoutes {
				if r.Dest.Contains(route.Router) {
					reachable = true
					break
				}
			}
			if assignedNet.Contains(route.Router) {
				reachable = true
			}
			if !reachable {
				continue
			}
		}
		ones, bits := route.Dest.Mask.Size()
		if bits == 0 && len(route.Dest.Mask) > 0 {
			// Bitmask is not ones followed by zeros, i.e. invalid
			continue
		}
		// Ignore routes that would be able to redirect loopback IPs
		if route.Dest.IP.IsLoopback() && ones >= 8 {
			continue
		}
		// Ignore routes that would shadow the implicit interface route
		assignedOnes, _ := assignedNet.Mask.Size()
		if assignedNet.IP.Equal(route.Dest.IP) && assignedOnes == ones {
			continue
		}
		validDest := true
		for _, r := range saneRoutes {
			rOnes, _ := r.Dest.Mask.Size()
			if r.Dest.IP.Equal(route.Dest.IP) && ones == rOnes {
				// Exact duplicate, ignore
				validDest = false
				break
			}
		}
		if validDest {
			saneRoutes = append(saneRoutes, route)
		}
	}
	return saneRoutes
}

// DNSServers represents an ordered collection of DNS servers
type DNSServers []net.IP

func (a DNSServers) Equal(b DNSServers) bool {
	if len(a) == len(b) {
		if len(a) == 0 {
			return true // both are empty or nil
		}
		for i, aVal := range a {
			if !aVal.Equal(b[i]) {
				return false
			}
		}
		return true
	}
	return false

}

func ip4toInt(ip net.IP) uint32 {
	ip4 := ip.To4()
	if ip4 == nil {
		return 0
	}
	return binary.BigEndian.Uint32(ip4)
}

// DNSServers returns all unique valid DNS servers from the DHCP
// DomainNameServers options.
// It returns nil if the lease is nil.
func (l *Lease) DNSServers() DNSServers {
	if l == nil {
		return nil
	}
	rawServers := dhcpv4.GetIPs(dhcpv4.OptionDomainNameServer, l.Options)
	var servers DNSServers
	serversSeenMap := make(map[uint32]bool)
	for _, s := range rawServers {
		ip4Num := ip4toInt(s)
		if s.IsGlobalUnicast() || s.IsLinkLocalUnicast() {
			if serversSeenMap[ip4Num] {
				continue
			}
			serversSeenMap[ip4Num] = true
			servers = append(servers, s)
		}
	}
	return servers
}
