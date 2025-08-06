// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package node

import "net"

// NetStatus is the current network status of the host. It will be updated by the
// network Service whenever the node's network configuration changes. Spurious
// changes might occur, consumers should ensure that the change that occured is
// meaningful to them.
type NetStatus struct {
	ExternalAddress net.IP
}
