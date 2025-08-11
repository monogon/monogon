// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Package ipam is just a stub right now which contains a type for
// describing prefix lists.
package ipam

import (
	"net/netip"
	"sort"
	"strings"

	cpb "source.monogon.dev/metropolis/proto/common"
)

// Prefixes are network prefixes that should be announced by a node to the
// Cluster Networking mesh.
type Prefixes []netip.Prefix

func (p *Prefixes) Proto() (res []*cpb.NodeClusterNetworking_Prefix) {
	for _, prefix := range *p {
		res = append(res, &cpb.NodeClusterNetworking_Prefix{
			Cidr: prefix.String(),
		})
	}
	return
}

// Update by copying all prefixes from o into p, merging duplicates as necessary.
func (p *Prefixes) Update(o *Prefixes) {
	// Gather prefixes we already have.
	cur := make(map[netip.Prefix]bool)
	for _, pp := range *p {
		cur[pp] = true
	}

	// Copy over any prefix that we don't yet have.
	for _, pp := range *o {
		if cur[pp] {
			continue
		}
		cur[pp] = true
		*p = append(*p, pp)
	}
}

// String returns a stringified, comma-dalimited representation of the prefixes.
func (p *Prefixes) String() string {
	if p == nil {
		return ""
	}

	var strs []string
	for _, pp := range *p {
		strs = append(strs, pp.String())
	}
	sort.Strings(strs)
	return strings.Join(strs, ", ")
}

func (p *Prefixes) Equal(o *Prefixes) bool {
	return p.String() == o.String()
}
