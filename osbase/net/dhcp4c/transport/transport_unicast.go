// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"golang.org/x/sys/unix"
)

// UnicastTransport implements a DHCP transport based on a normal Linux UDP
// socket with some custom socket options to influence DSCP and routing.
type UnicastTransport struct {
	udpConn  *net.UDPConn
	targetIP net.IP
	iface    *net.Interface
}

func NewUnicastTransport(iface *net.Interface) *UnicastTransport {
	return &UnicastTransport{
		iface: iface,
	}
}

func (t *UnicastTransport) Open(serverIP, bindIP net.IP) error {
	if t.udpConn != nil {
		return errors.New("unicast transport already open")
	}
	rawFd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return fmt.Errorf("failed to get socket: %w", err)
	}
	if err := unix.BindToDevice(rawFd, t.iface.Name); err != nil {
		return fmt.Errorf("failed to bind UDP interface to device: %w", err)
	}
	if err := unix.SetsockoptByte(rawFd, unix.SOL_IP, unix.IP_TOS, dscpCS7<<2); err != nil {
		return fmt.Errorf("failed to set DSCP CS7: %w", err)
	}
	var addr [4]byte
	copy(addr[:], bindIP.To4())
	if err := unix.Bind(rawFd, &unix.SockaddrInet4{Addr: addr, Port: 68}); err != nil {
		return fmt.Errorf("failed to bind UDP unicast interface: %w", err)
	}
	filePtr := os.NewFile(uintptr(rawFd), "dhcp-udp")
	defer filePtr.Close()
	conn, err := net.FileConn(filePtr)
	if err != nil {
		return fmt.Errorf("failed to initialize runtime-supported UDP connection: %w", err)
	}
	realConn, ok := conn.(*net.UDPConn)
	if !ok {
		panic("UDP socket imported into Go runtime is no longer a UDP socket")
	}
	t.udpConn = realConn
	t.targetIP = serverIP
	return nil
}

func (t *UnicastTransport) Send(payload *dhcpv4.DHCPv4) error {
	if t.udpConn == nil {
		return errors.New("unicast transport closed")
	}
	_, _, err := t.udpConn.WriteMsgUDP(payload.ToBytes(), []byte{}, &net.UDPAddr{
		IP:   t.targetIP,
		Port: 67,
	})
	return err
}

func (t *UnicastTransport) SetReceiveDeadline(deadline time.Time) error {
	return t.udpConn.SetReadDeadline(deadline)
}

func (t *UnicastTransport) Receive() (*dhcpv4.DHCPv4, error) {
	if t.udpConn == nil {
		return nil, errors.New("unicast transport closed")
	}
	receiveBuf := make([]byte, math.MaxUint16)
	_, _, err := t.udpConn.ReadFromUDP(receiveBuf)
	if err != nil {
		return nil, deadlineFromTimeout(err)
	}
	msg, err := dhcpv4.FromBytes(receiveBuf)
	if err != nil {
		return nil, NewInvalidMessageError(err)
	}
	return msg, nil
}

func (t *UnicastTransport) Close() error {
	if t.udpConn == nil {
		return nil
	}
	err := t.udpConn.Close()
	t.udpConn = nil
	if err != nil && errors.Is(err, net.ErrClosed) {
		//nolint:returnerrcheck
		return nil
	}
	return err
}
