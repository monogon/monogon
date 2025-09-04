// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package allocs

// These are netlink protocol numbers used internally for various netlink
// resource (e.g. route) owners/manager.
const (
	// ProtocolOverlay is used by //metropolis/node/core/network/overlay
	// when creating/removing routes pointing to the overlay interface.
	ProtocolOverlay int = 129
)

// Netlink link groups used for interface classification and traffic matching.
const (
	// LinkGroupK8sPod is set on all host side PtP interfaces going to K8s
	// pods.
	LinkGroupK8sPod uint32 = 8
	// LinkGroupOverlay is set on all interfaces which are part of the overlay
	// network and thus exempt from SNATing of workload traffic.
	LinkGroupOverlay uint32 = 9
)
