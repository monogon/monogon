// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Package socksproxy implements a limited subset of the SOCKS 5 (RFC1928)
// protocol in the form of a pluggable Proxy object. However, this
// implementation is _not_ RFC1928 compliant, as it does not implement GSSAPI
// (which is mandated by the spec). It currently only implements CONNECT
// requests to IPv4/IPv6 addresses. It also doesn't implement any
// timeout/keepalive system for killing inactive connections.
//
// The intended use of the library is internally within Metropolis development
// environments for contacting test clusters. The code is simple and robust, but
// not really productionized (as noted above - no timeouts and no authentication
// make it a bad idea to ever expose this proxy server publicly).
//
// There are multiple other, existing Go SOCKS4/5 server implementations, but
// many of them are either not context aware, part of a larger project (and thus
// difficult to extract) or are brand new/untested/bleeding edge code.
package socksproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
)

// Handler should be implemented by socksproxy users to allow SOCKS connections
// to be proxied in any other way than via the HostHandler.
type Handler interface {
	// Connect is called by the server any time a SOCKS client sends a CONNECT
	// request. The function should return a ConnectResponse describing some
	// 'backend' connection, ie. the connection that will then be exposed to the
	// SOCKS client.
	//
	// Connect should return with Error set to a non-default value to abort/deny the
	// connection request.
	//
	// The underlying incoming socket is managed by the proxy server and is not
	// visible to the client. However, any sockets/connections/files opened by the
	// Handler should be cleaned up by tying them to the given context, which will
	// be canceled whenever the connection is closed.
	Connect(context.Context, *ConnectRequest) *ConnectResponse
}

// ConnectRequest represents a pending CONNECT request from a client.
type ConnectRequest struct {
	// Address is an IPv4 or IPv6 address that the client requested to connect to.
	// This address might be invalid/malformed/internal, and the Connect method
	// should sanitize it before using it.
	Address net.IP
	// Hostname is a string that the client requested to connect to. Only set if
	// Address is empty. Format and resolution rules are up to the implementer,
	// a lot of clients only allow valid DNS labels.
	Hostname string
	// Port is the TCP port number that the client requested to connect to.
	Port uint16
}

// ConnectResponse indicates a 'backend' connection that the proxy should expose
// to the client, or an error if the connection cannot be made.
type ConnectResponse struct {
	// Error will cause an error to be returned if it is anything else than the
	// default value (ReplySucceeded).
	Error Reply

	// Backend is the ReadWriteCloser that will be bridged over to the connecting
	// client if no Error is set.
	Backend io.ReadWriteCloser
	// LocalAddress is the IP address that is returned to the client as the local
	// address of the newly established backend connection.
	LocalAddress net.IP
	// LocalPort is the local TCP port number that is returned to the client as the
	// local port of the newly established backend connection.
	LocalPort uint16
}

// ConnectResponseFromConn builds a ConnectResponse from a net.Conn. This can be
// used by custom Handlers to easily return a ConnectResponse for a newly
// established net.Conn, eg. from a Dial call.
//
// An error is returned if the given net.Conn does not carry a properly formed
// LocalAddr.
func ConnectResponseFromConn(c net.Conn) (*ConnectResponse, error) {
	laddr := c.LocalAddr().String()
	host, port, err := net.SplitHostPort(laddr)
	if err != nil {
		return nil, fmt.Errorf("could not parse LocalAddr %q: %w", laddr, err)
	}
	addr := net.ParseIP(host)
	if addr == nil {
		return nil, fmt.Errorf("could not parse LocalAddr host %q as IP", host)
	}
	portNum, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("could not parse LocalAddr port %q", port)
	}
	return &ConnectResponse{
		Backend:      c,
		LocalAddress: addr,
		LocalPort:    uint16(portNum),
	}, nil
}

type hostHandler struct{}

func (h *hostHandler) Connect(ctx context.Context, req *ConnectRequest) *ConnectResponse {
	port := fmt.Sprintf("%d", req.Port)
	var host string
	if req.Hostname != "" {
		host = req.Hostname
	} else {
		host = req.Address.String()
	}
	addr := net.JoinHostPort(host, port)
	s, err := net.Dial("tcp", addr)
	if err != nil {
		log.Printf("HostHandler could not dial %q: %v", addr, err)
		return &ConnectResponse{Error: ReplyConnectionRefused}
	}
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	res, err := ConnectResponseFromConn(s)
	if err != nil {
		log.Printf("HostHandler could not build response: %v", err)
		return &ConnectResponse{Error: ReplyGeneralFailure}
	}
	return res
}

var (
	// HostHandler is an unsafe SOCKS5 proxy Handler which passes all incoming
	// connections into the local network stack. The incoming addresses/ports are
	// not sanitized, and as the proxy does not perform authentication, this handler
	// is an open proxy. This handler should never be used in cases where the proxy
	// server is publicly available.
	HostHandler = &hostHandler{}
)

// Serve runs a SOCKS5 proxy server for a given Handler at a given listener.
//
// When the given context is canceled, the server will stop and the listener
// will be closed. All pending connections will also be canceled and their
// sockets closed.
func Serve(ctx context.Context, handler Handler, lis net.Listener) error {
	go func() {
		<-ctx.Done()
		lis.Close()
	}()

	for {
		con, err := lis.Accept()
		if err != nil {
			// Context cancellation will close listener socket with a generic 'use of closed
			// network connection' error, translate that back to context error.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		go handle(ctx, handler, con)
	}
}

// handle runs in a goroutine per incoming SOCKS connection. Its lifecycle
// corresponds to the lifecycle of a running proxy connection.
func handle(ctx context.Context, handler Handler, con net.Conn) {
	// ctxR is a per-request context, and will be canceled whenever the handler
	// exits or the server is stopped.
	ctxR, ctxRC := context.WithCancel(ctx)
	defer ctxRC()

	go func() {
		<-ctxR.Done()
		con.Close()
	}()

	// Perform method negotiation with the client.
	if err := negotiateMethod(con); err != nil {
		return
	}

	// Read request from the client and translate problems into early error replies.
	req, err := readRequest(con)
	switch {
	case errors.Is(err, errNotConnect):
		writeReply(con, ReplyCommandNotSupported, net.IPv4(0, 0, 0, 0), 0)
		return
	case errors.Is(err, errUnsupportedAddressType):
		writeReply(con, ReplyAddressTypeNotSupported, net.IPv4(0, 0, 0, 0), 0)
		return
	case err == nil:
	default:
		writeReply(con, ReplyGeneralFailure, net.IPv4(0, 0, 0, 0), 0)
		return
	}

	// Ask handler.Connect for a backend.
	conRes := handler.Connect(ctxR, &ConnectRequest{
		Address:  req.address,
		Hostname: req.hostname,
		Port:     req.port,
	})
	// Handle programming error when returned value is nil.
	if conRes == nil {
		writeReply(con, ReplyGeneralFailure, net.IPv4(0, 0, 0, 0), 0)
		return
	}
	// Handle returned errors.
	if conRes.Error != ReplySucceeded {
		writeReply(con, conRes.Error, net.IPv4(0, 0, 0, 0), 0)
		return
	}

	// Ensure Bound.* fields are set.
	if conRes.Backend == nil || conRes.LocalAddress == nil || conRes.LocalPort == 0 {
		writeReply(con, ReplyGeneralFailure, net.IPv4(0, 0, 0, 0), 0)
		return
	}
	// Send reply.
	if err := writeReply(con, ReplySucceeded, conRes.LocalAddress, conRes.LocalPort); err != nil {
		return
	}

	// Pipe returned backend into connection.
	go func() {
		io.Copy(conRes.Backend, con)
		conRes.Backend.Close()
	}()
	io.Copy(con, conRes.Backend)
	conRes.Backend.Close()
}
