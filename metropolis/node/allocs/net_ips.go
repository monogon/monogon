// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package allocs

import "net"

// These are IP addresses used by various parts of Metropolis.
var (
	// Used by //metropolis/node/kubernetes as the DNS server IP for containers.
	// Link-local IP space, 77 for ASCII M(onogon), 53 for DNS port.
	IPContainerDNS = net.IPv4(169, 254, 77, 53)
)
