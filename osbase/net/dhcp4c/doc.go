// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Package dhcp4c provides a client implementation of the DHCPv4 protocol
// (RFC2131) and a few extensions for Linux-based systems.
// The code is split into three main parts:
//   - The core DHCP state machine, which lives in dhcpc.go
//   - Mechanisms to send and receive DHCP messages, which live in transport/
//   - Standard callbacks which implement necessary kernel configuration steps in
//     a simple and standalone way living in callback/
//
// Since the DHCP protocol is ugly and underspecified (see
// https://tools.ietf.org/html/draft-ietf-dhc-implementation-02 for a subset of
// known issues), this client slightly bends the specification in the following
// cases:
//   - IP fragmentation for DHCP messages is not supported for both sending and
//     receiving messages This is because the major servers (ISC, dnsmasq, ...)
//     do not implement it and just drop fragmented packets, so it would be
//     counterproductive to try to send them. The client just attempts to send
//     the full message and hopes it passes through to the server.
//   - The suggested timeouts and wait periods have been tightened significantly.
//     When the standard was written 10Mbps Ethernet with hubs was a common
//     interconnect. Using these would make the client extremely slow on today's
//     1Gbps+ networks.
//   - Wrong data in DHCP responses is fixed up if possible. This fixing includes
//     dropping prohibited options, clamping semantically invalid data and
//     defaulting not set options as far as it's possible. Non-recoverable
//     responses (for example because a non-Unicast IP is handed out or lease
//     time is not set or zero) are still ignored.  All data which can be stored
//     in both DHCP fields and options is also normalized to the corresponding
//     option.
//   - Duplicate Address Detection is not implemented by default. It's slow, hard
//     to implement correctly and generally not necessary on modern networks as
//     the servers already waste time checking for duplicate addresses. It's
//     possible to hook it in via a LeaseCallback if necessary in a given
//     application.
//
// Operationally, there's one known caveat to using this client: If the lease
// offered during the select phase (in a DHCPOFFER) is not the same as the one
// sent in the following DHCPACK the first one might be acceptable, but the
// second one might not be. This can cause pathological behavior where the
// client constantly switches between discovering and requesting states.
// Depending on the reuse policies on the DHCP server this can cause the client
// to consume all available IP addresses. Sadly there's no good way of fixing
// this within the boundaries of the protocol. A DHCPRELEASE for the adresse
// would need to be unicasted so the unaccepable address would need to be
// configured which can be either impossible if it's not valid or not
// acceptable from a security standpoint (for example because it overlaps with
// a prefix used internally) and a DHCPDECLINE would cause the server to
// blacklist the IP thus also depleting the IP pool.
// This could be potentially avoided by originating DHCPRELEASE packages from a
// userspace transport, but said transport would need to be routing- and
// PMTU-aware which would make it even more complicated than the existing
// BroadcastTransport.
package dhcp4c
