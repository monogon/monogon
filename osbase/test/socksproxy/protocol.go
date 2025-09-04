// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package socksproxy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

// readMethods implements RFC1928 3. “Procedure for TCP-based clients”,
// paragraph 3. It receives a 'version identifier/method selection message' from
// r and returns the methods supported by the client.
func readMethods(r io.Reader) ([]method, error) {
	var ver uint8
	if err := binary.Read(r, binary.BigEndian, &ver); err != nil {
		return nil, fmt.Errorf("when reading ver: %w", err)
	}
	if ver != 5 {
		return nil, fmt.Errorf("unimplemented version %d", ver)
	}
	var nmethods uint8
	if err := binary.Read(r, binary.BigEndian, &nmethods); err != nil {
		return nil, fmt.Errorf("when reading nmethods: %w", err)
	}
	methodBytes := make([]byte, nmethods)
	if _, err := io.ReadFull(r, methodBytes); err != nil {
		return nil, fmt.Errorf("while reading methods: %w", err)
	}
	methods := make([]method, nmethods)
	for i, m := range methodBytes {
		methods[i] = method(m)
	}
	return methods, nil
}

// writeMethod implements RFC1928 3. “Procedure for TCP-based clients”,
// paragraph 5. It sends a selected method to w.
func writeMethod(w io.Writer, m method) error {
	if err := binary.Write(w, binary.BigEndian, uint8(5)); err != nil {
		return fmt.Errorf("while writing version: %w", err)
	}
	if err := binary.Write(w, binary.BigEndian, uint8(m)); err != nil {
		return fmt.Errorf("while writing method: %w", err)
	}
	return nil
}

// method is an RFC1928 authentication method.
type method uint8

const (
	methodNoAuthenticationRequired method = 0
	methodNoAcceptableMethods      method = 0xff
)

// negotiateMethod implements the entire flow RFC1928 3. “Procedure for
// TCP-based clients” by negotiating for the 'NO AUTHENTICATION REQUIRED'
// authentication method, and failing otherwise.
func negotiateMethod(rw io.ReadWriter) error {
	methods, err := readMethods(rw)
	if err != nil {
		return fmt.Errorf("could not read methods: %w", err)
	}

	found := false
	for _, m := range methods {
		if m == methodNoAuthenticationRequired {
			found = true
			break
		}
	}
	if !found {
		// Discard error, as this connection is failed anyway.
		writeMethod(rw, methodNoAcceptableMethods)
		return fmt.Errorf("no acceptable methods found")
	}
	if err := writeMethod(rw, methodNoAuthenticationRequired); err != nil {
		return fmt.Errorf("could not respond with method: %w", err)
	}
	return nil
}

var (
	// errNotConnect is returned by readRequest when the request contained some
	// other request than CONNECT.
	errNotConnect = errors.New("not CONNECT")
	// errUnsupportedAddressType is returned by readRequest when the request
	// contained some unsupported address type (not IPv4 or IPv6).
	errUnsupportedAddressType = errors.New("unsupported address type")
)

// readRequest implements RFC1928 4. “Requests” by reading a SOCKS request from
// r and ensuring it's an IPv4/IPv6 CONNECT request. The parsed address/port
// pair is then returned.
func readRequest(r io.Reader) (*connectRequest, error) {
	header := struct {
		Ver  uint8
		Cmd  uint8
		Rsv  uint8
		Atyp uint8
	}{}
	if err := binary.Read(r, binary.BigEndian, &header); err != nil {
		return nil, fmt.Errorf("when reading request header: %w", err)
	}

	if header.Ver != 5 {
		return nil, fmt.Errorf("invalid version %d", header.Ver)
	}
	if header.Cmd != 1 {
		return nil, errNotConnect
	}

	var addrBytes []byte
	var hostnameBytes []byte
	switch header.Atyp {
	case 1:
		addrBytes = make([]byte, 4)
	case 3:
		// Variable-length string to resolve
		addrBytes = make([]byte, 1)
	case 4:
		addrBytes = make([]byte, 16)
	default:
		return nil, errUnsupportedAddressType
	}
	if _, err := io.ReadFull(r, addrBytes); err != nil {
		return nil, fmt.Errorf("when reading address: %w", err)
	}

	// Handle domain name addressing, required by for example Chrome
	if header.Atyp == 3 {
		hostnameBytes = make([]byte, addrBytes[0])
		if _, err := io.ReadFull(r, hostnameBytes); err != nil {
			return nil, fmt.Errorf("when reading address: %w", err)
		}
	}

	var port uint16
	if err := binary.Read(r, binary.BigEndian, &port); err != nil {
		return nil, fmt.Errorf("when reading port: %w", err)
	}

	return &connectRequest{
		address:  addrBytes,
		hostname: string(hostnameBytes),
		port:     port,
	}, nil
}

type connectRequest struct {
	address  net.IP
	hostname string
	port     uint16
}

// Reply is an RFC1928 6. “Replies” reply field value. It's returned to the
// client by internal socksproxy code or a Handler to signal a success or error
// condition within an RFC1928 reply.
type Reply uint8

const (
	ReplySucceeded               Reply = 0
	ReplyGeneralFailure          Reply = 1
	ReplyConnectionNotAllowed    Reply = 2
	ReplyNetworkUnreachable      Reply = 3
	ReplyHostUnreachable         Reply = 4
	ReplyConnectionRefused       Reply = 5
	ReplyTTLExpired              Reply = 6
	ReplyCommandNotSupported     Reply = 7
	ReplyAddressTypeNotSupported Reply = 8
)

// writeReply implements RFC1928 6. “Replies” by sending a given Reply, bind
// address and bind port to w. An error is returned if the given bind address is
// invaild, or if a communication error occurred.
func writeReply(w io.Writer, r Reply, bindAddr net.IP, bindPort uint16) error {
	var atyp uint8
	switch len(bindAddr) {
	case 4:
		atyp = 1
	case 16:
		atyp = 4
	default:
		return fmt.Errorf("unsupported bind address type")
	}

	header := struct {
		Ver   uint8
		Reply uint8
		Rsv   uint8
		Atyp  uint8
	}{
		Ver:   5,
		Reply: uint8(r),
		Rsv:   0,
		Atyp:  atyp,
	}
	if err := binary.Write(w, binary.BigEndian, &header); err != nil {
		return fmt.Errorf("when writing reply header: %w", err)
	}
	if _, err := w.Write(bindAddr); err != nil {
		return fmt.Errorf("when writing reply bind address: %w", err)
	}
	if err := binary.Write(w, binary.BigEndian, bindPort); err != nil {
		return fmt.Errorf("when writing reply bind port: %w", err)
	}
	return nil
}
