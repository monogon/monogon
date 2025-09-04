// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"errors"
	"fmt"
	"math"
	"net"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/mdlayher/packet"
	"golang.org/x/net/bpf"
)

const (
	// RFC2474 Section 4.2.2.1 with reference to RFC791 Section 3.1 (Network
	// Control Precedence)
	dscpCS7 = 0x7 << 3

	// IPv4 MTU
	maxIPv4MTU = math.MaxUint16 // IPv4 "Total Length" field is an unsigned 16 bit integer
)

// mustAssemble calls bpf.Assemble and panics if it retuns an error.
func mustAssemble(insns []bpf.Instruction) []bpf.RawInstruction {
	rawInsns, err := bpf.Assemble(insns)
	if err != nil {
		panic("mustAssemble failed to assemble BPF: " + err.Error())
	}
	return rawInsns
}

// BPF filter for UDP in IPv4 with destination port 68 (DHCP Client)
//
// This is used to make the kernel drop non-DHCP traffic for us so that we
// don't have to handle excessive unrelated traffic flowing on a given link
// which might overwhelm the single-threaded receiver.
var bpfFilterInstructions = []bpf.Instruction{
	// Check IP protocol version equals 4 (first 4 bits of the first byte)
	// With Ethernet II framing, this is more of a sanity check. We already
	// request the kernel to only return EtherType 0x0800 (IPv4) frames.
	bpf.LoadAbsolute{Off: 0, Size: 1},
	bpf.ALUOpConstant{Op: bpf.ALUOpAnd, Val: 0xf0}, // SubnetMask second 4 bits
	bpf.JumpIf{Cond: bpf.JumpEqual, Val: 4 << 4, SkipTrue: 1},
	bpf.RetConstant{Val: 0}, // Discard

	// Check IPv4 Protocol byte (offset 9) equals UDP
	bpf.LoadAbsolute{Off: 9, Size: 1},
	bpf.JumpIf{Cond: bpf.JumpEqual, Val: uint32(layers.IPProtocolUDP), SkipTrue: 1},
	bpf.RetConstant{Val: 0}, // Discard

	// Check if IPv4 fragment offset is all-zero (this is not a fragment)
	bpf.LoadAbsolute{Off: 6, Size: 2},
	bpf.JumpIf{Cond: bpf.JumpBitsSet, Val: 0x1fff, SkipFalse: 1},
	bpf.RetConstant{Val: 0}, // Discard

	// Load IPv4 header size from offset zero and store it into X
	bpf.LoadMemShift{Off: 0},

	// Check if UDP header destination port equals 68
	bpf.LoadIndirect{Off: 2, Size: 2}, // Offset relative to header size in register X
	bpf.JumpIf{Cond: bpf.JumpEqual, Val: 68, SkipTrue: 1},
	bpf.RetConstant{Val: 0}, // Discard

	// Accept packet and pass through up maximum IP packet size
	bpf.RetConstant{Val: maxIPv4MTU},
}

var bpfFilter = mustAssemble(bpfFilterInstructions)

// BroadcastTransport implements a DHCP transport based on a custom IP/UDP
// stack fulfilling the specific requirements for broadcasting DHCP packets
// (like all-zero source address, no ARP, ...)
type BroadcastTransport struct {
	rawConn *packet.Conn
	iface   *net.Interface
}

func NewBroadcastTransport(iface *net.Interface) *BroadcastTransport {
	return &BroadcastTransport{iface: iface}
}

func (t *BroadcastTransport) Open() error {
	if t.rawConn != nil {
		return errors.New("broadcast transport already open")
	}
	rawConn, err := packet.Listen(t.iface, packet.Datagram, int(layers.EthernetTypeIPv4), &packet.Config{
		Filter: bpfFilter,
	})
	if err != nil {
		return fmt.Errorf("failed to create raw listener: %w", err)
	}
	t.rawConn = rawConn
	return nil
}

func (t *BroadcastTransport) Send(payload *dhcpv4.DHCPv4) error {
	if t.rawConn == nil {
		return errors.New("broadcast transport closed")
	}
	pkt := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{
		ComputeChecksums: true,
		FixLengths:       true,
	}

	ipLayer := &layers.IPv4{
		Version: 4,
		// Shift left of ECN field
		TOS: dscpCS7 << 2,
		// These packets should never be routed (their IP headers contain
		// garbage)
		TTL:      1,
		Protocol: layers.IPProtocolUDP,
		// Most DHCP servers don't support fragmented packets.
		Flags: layers.IPv4DontFragment,
		DstIP: net.IPv4bcast,
		SrcIP: net.IPv4zero,
	}
	udpLayer := &layers.UDP{
		DstPort: 67,
		SrcPort: 68,
	}
	if err := udpLayer.SetNetworkLayerForChecksum(ipLayer); err != nil {
		panic("Invalid layer stackup encountered")
	}

	err := gopacket.SerializeLayers(pkt, opts,
		ipLayer,
		udpLayer,
		gopacket.Payload(payload.ToBytes()))

	if err != nil {
		return fmt.Errorf("failed to assemble packet: %w", err)
	}

	_, err = t.rawConn.WriteTo(pkt.Bytes(), &packet.Addr{HardwareAddr: layers.EthernetBroadcast})
	if err != nil {
		return fmt.Errorf("failed to transmit broadcast packet: %w", err)
	}
	return nil
}

func (t *BroadcastTransport) Receive() (*dhcpv4.DHCPv4, error) {
	if t.rawConn == nil {
		return nil, errors.New("broadcast transport closed")
	}
	buf := make([]byte, math.MaxUint16) // Maximum IP packet size
	n, _, err := t.rawConn.ReadFrom(buf)
	if err != nil {
		return nil, deadlineFromTimeout(err)
	}
	respPacket := gopacket.NewPacket(buf[:n], layers.LayerTypeIPv4, gopacket.Default)
	ipLayer := respPacket.Layer(layers.LayerTypeIPv4)
	if ipLayer == nil {
		return nil, NewInvalidMessageError(errors.New("got invalid IP packet"))
	}
	ip := ipLayer.(*layers.IPv4)
	if ip.Flags&layers.IPv4MoreFragments != 0 {
		return nil, NewInvalidMessageError(errors.New("got fragmented message"))
	}

	udpLayer := respPacket.Layer(layers.LayerTypeUDP)
	if udpLayer == nil {
		return nil, NewInvalidMessageError(errors.New("got non-UDP packet"))
	}
	udp := udpLayer.(*layers.UDP)
	if udp.DstPort != 68 {
		return nil, NewInvalidMessageError(errors.New("message not for DHCP client port"))
	}
	msg, err := dhcpv4.FromBytes(udp.Payload)
	if err != nil {
		return nil, NewInvalidMessageError(fmt.Errorf("failed to decode DHCPv4 message: %w", err))
	}
	return msg, nil
}

func (t *BroadcastTransport) Close() error {
	if t.rawConn == nil {
		return nil
	}
	if err := t.rawConn.Close(); err != nil {
		return err
	}
	t.rawConn = nil
	return nil
}

func (t *BroadcastTransport) SetReceiveDeadline(deadline time.Time) error {
	if t.rawConn == nil {
		return errors.New("broadcast transport closed")
	}
	return t.rawConn.SetReadDeadline(deadline)
}
