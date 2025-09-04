// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package freeport

import (
	"io"
	"net"
)

// AllocateTCPPort allocates a TCP port on the looopback address, and starts a
// temporary listener on it. That listener is returned to the caller alongside with
// the allocated port number. The listener must be closed right before the port is
// used by the caller. This naturally still leaves a race condition window where
// that port number might be snatched up by some other process, but there doesn't
// seem to be a better way to do this.
func AllocateTCPPort() (uint16, io.Closer, error) {
	addr, err := net.ResolveTCPAddr("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, nil, err
	}

	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return 0, nil, err
	}
	return uint16(l.Addr().(*net.TCPAddr).Port), l, nil
}

// MustConsume takes the result of AllocateTCPPort, closes the listener and returns
// the allocated port. If anything goes wrong (port could not be allocated or
// closed) it will panic.
func MustConsume(port uint16, lis io.Closer, err error) int {
	if err != nil {
		panic(err)
	}
	if err := lis.Close(); err != nil {
		panic(err)
	}
	return int(port)
}
