// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package dhcp4c

import (
	"bytes"
	"net"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/stretchr/testify/assert"
)

func TestLeaseDHCPServers(t *testing.T) {
	var tests = []struct {
		name     string
		lease    *Lease
		expected DNSServers
	}{{
		name:     "ReturnsNilWithNoLease",
		lease:    nil,
		expected: nil,
	}, {
		name: "DiscardsInvalidIPs",
		lease: &Lease{
			Options: dhcpv4.OptionsFromList(dhcpv4.OptDNS(net.IP{0, 0, 0, 0})),
		},
		expected: nil,
	}, {
		name: "DeduplicatesIPs",
		lease: &Lease{
			Options: dhcpv4.OptionsFromList(dhcpv4.OptDNS(net.IP{192, 0, 2, 1}, net.IP{192, 0, 2, 2}, net.IP{192, 0, 2, 1})),
		},
		expected: DNSServers{net.IP{192, 0, 2, 1}, net.IP{192, 0, 2, 2}},
	}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			res := test.lease.DNSServers()
			assert.Equal(t, test.expected, res)
		})
	}
}

func makeIPNet(cidr string) *net.IPNet {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	return n
}

type testRoute struct {
	dest     string
	via      string
	expected bool
}

func (t testRoute) toRealRoute() *dhcpv4.Route {
	ip, n, err := net.ParseCIDR(t.dest)
	if err != nil {
		panic(err)
	}
	if !ip.Equal(n.IP) {
		panic("testRoute is not aligned to route boundary")
	}
	routerIP := net.ParseIP(t.via)
	if routerIP == nil && t.via != "" {
		panic("routerIP is not valid")
	}
	return &dhcpv4.Route{
		Dest:   n,
		Router: routerIP,
	}
}

func TestSanitizeRoutes(t *testing.T) {
	var tests = []struct {
		name        string
		assignedNet *net.IPNet
		routes      []testRoute
	}{{
		name:        "SimpleAdditionalRoute",
		assignedNet: makeIPNet("10.0.5.0/24"),
		routes: []testRoute{
			{"10.0.7.0/24", "10.0.5.1", true},
		},
	}, {
		name:        "OutOfNetworkGateway",
		assignedNet: makeIPNet("10.5.0.2/32"),
		routes: []testRoute{
			{"10.0.7.1/32", "", true},
			{"0.0.0.0/0", "10.0.7.1", true},
		},
	}, {
		name:        "InvalidRouter",
		assignedNet: makeIPNet("10.0.5.0/24"),
		routes: []testRoute{
			// Router is localhost
			{"10.0.7.0/24", "127.0.0.1", false},
			// Router is unreachable
			{"10.0.8.0/24", "10.254.0.1", false},
		},
	}, {
		name:        "SameDestinationRoutes",
		assignedNet: makeIPNet("10.0.5.0/24"),
		routes: []testRoute{
			{"0.0.0.0/0", "10.0.5.1", true},
			{"10.0.7.0/24", "10.0.5.1", true},
			{"0.0.0.0/0", "10.0.7.1", false},
		},
	}, {
		name:        "RoutesShadowingLoopback",
		assignedNet: makeIPNet("10.0.5.0/24"),
		routes: []testRoute{
			// Default route, technically covers 127.0.0.0/8, but less-specific
			{"0.0.0.0/0", "10.0.5.1", true},
			// 127.0.0.0/8 is still more-specific
			{"126.0.0.0/7", "10.0.5.1", true},
			// Duplicate of 127.0.0.0/8, behavior undefined, disallowed
			{"127.0.0.0/8", "10.0.5.1", false},
			// Shadows localhost, disallowed
			{"127.0.0.1/32", "10.0.5.1", false},
		},
	}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var routes []*dhcpv4.Route
			var expectedRoutes []*dhcpv4.Route
			for _, r := range test.routes {
				routes = append(routes, r.toRealRoute())
				if r.expected {
					expectedRoutes = append(expectedRoutes, r.toRealRoute())
				}
			}
			out := sanitizeRoutes(routes, test.assignedNet)
			if len(out) != len(expectedRoutes) {
				t.Errorf("expected %d routes, got %d", len(expectedRoutes), len(out))
				t.Error("Expected:")
				for _, r := range expectedRoutes {
					t.Errorf("\t%s via %s", r.Dest, r.Router)
				}
				t.Error("Actual:")
				for _, r := range out {
					t.Errorf("\t%s via %s", r.Dest, r.Router)
				}
				return
			}
			for i, r := range expectedRoutes {
				if !out[i].Router.Equal(r.Router) || !out[i].Dest.IP.Equal(r.Dest.IP) || !bytes.Equal(out[i].Dest.Mask, r.Dest.Mask) {
					t.Errorf("expected %s via %s, got %s via %s", r.Dest, r.Router, out[i].Dest, out[i].Router)
				}
			}
		})
	}
}
