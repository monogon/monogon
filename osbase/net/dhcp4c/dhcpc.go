// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Package dhcp4c implements a DHCPv4 Client as specified in RFC2131 (with some
// notable deviations).  It implements only the DHCP state machine itself, any
// configuration other than the interface IP address (which is always assigned
// in DHCP and necessary for the protocol to work) is exposed as
// [informers/observables/watchable variables/???] to consumers who then deal
// with it.
package dhcp4c

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"

	"source.monogon.dev/osbase/net/dhcp4c/transport"
	"source.monogon.dev/osbase/supervisor"
)

type state int

const (
	// stateDiscovering sends broadcast DHCPDISCOVER messages to the network
	// and waits for either a DHCPOFFER or (in case of Rapid Commit) DHCPACK.
	stateDiscovering state = iota
	// stateRequesting sends broadcast DHCPREQUEST messages containing the
	// server identifier for the selected lease and waits for a DHCPACK or a
	// DHCPNAK. If it doesn't get either it transitions back into discovering.
	stateRequesting
	// stateBound just waits until RenewDeadline (derived from RenewTimeValue,
	// half the lifetime by default) expires.
	stateBound
	// stateRenewing sends unicast DHCPREQUEST messages to the
	// currently-selected server and waits for either a DHCPACK or DHCPNAK
	// message. On DHCPACK it transitions to bound, otherwise to discovering.
	stateRenewing
	// stateRebinding sends broadcast DHCPREQUEST messages to the network and
	// waits for either a DHCPACK or DHCPNAK from any server. Response
	// processing is identical to stateRenewing.
	stateRebinding
)

func (s state) String() string {
	switch s {
	case stateDiscovering:
		return "DISCOVERING"
	case stateRequesting:
		return "REQUESTING"
	case stateBound:
		return "BOUND"
	case stateRenewing:
		return "RENEWING"
	case stateRebinding:
		return "REBINDING"
	default:
		return "INVALID"
	}
}

// This only requests SubnetMask and IPAddressLeaseTime as renewal and
// rebinding times are fine if they are just defaulted. They are respected (if
// valid, otherwise they are clamped to the nearest valid value) if sent by the
// server.
var internalOptions = dhcpv4.OptionCodeList{dhcpv4.OptionSubnetMask, dhcpv4.OptionIPAddressLeaseTime}

// Transport represents a mechanism over which DHCP messages can be exchanged
// with a server.
type Transport interface {
	// Send attempts to send the given DHCP payload message to the transport
	// target once. An empty return value does not indicate that the message
	// was successfully received.
	Send(payload *dhcpv4.DHCPv4) error
	// SetReceiveDeadline sets a deadline for Receive() calls after which they
	// return with ErrDeadlineExceeded
	SetReceiveDeadline(time.Time) error
	// Receive waits for a DHCP message to arrive and returns it. If the
	// deadline expires without a message arriving it will return
	// ErrDeadlineExceeded. If the message is completely malformed it will an
	// instance of InvalidMessageError.
	Receive() (*dhcpv4.DHCPv4, error)
	// Close closes the given transport. Calls to any of the above methods will
	// fail if the transport is closed.  Specific transports can be reopened
	// after being closed.
	Close() error
}

// UnicastTransport represents a mechanism over which DHCP messages can be
// exchanged with a single server over an arbitrary IPv4-based network.
// Implementers need to support servers running outside the local network via a
// router.
type UnicastTransport interface {
	Transport
	// Open connects the transport to a new unicast target. Can only be called
	// after calling Close() or after creating a new transport.
	Open(serverIP, bindIP net.IP) error
}

// BroadcastTransport represents a mechanism over which DHCP messages can be
// exchanged with all servers on a Layer 2 broadcast domain. Implementers need
// to support sending and receiving messages without any IP being configured on
// the interface.
type BroadcastTransport interface {
	Transport
	// Open connects the transport. Can only be called after calling Close() or
	// after creating a new transport.
	Open() error
}

type LeaseCallback func(*Lease) error

// Client implements a DHCPv4 client.
//
// Note that the size of all data sent to the server (RequestedOptions,
// ClientIdentifier, VendorClassIdentifier and ExtraRequestOptions) should be
// kept reasonably small (<500 bytes) in order to maximize the chance that
// requests can be properly transmitted.
type Client struct {
	// RequestedOptions contains a list of extra options this client is
	// interested in
	RequestedOptions dhcpv4.OptionCodeList

	// ClientIdentifier is used by the DHCP server to identify this client.
	// If empty, on Ethernet the MAC address is used instead.
	ClientIdentifier []byte

	// VendorClassIdentifier is used by the DHCP server to identify options
	// specific to this type of clients and to populate the vendor-specific
	// option (43).
	VendorClassIdentifier string

	// ExtraRequestOptions are extra options sent to the server.
	ExtraRequestOptions dhcpv4.Options

	// Backoff strategies for each state. These all have sane defaults,
	// override them only if necessary.
	DiscoverBackoff    backoff.BackOff
	AcceptOfferBackoff backoff.BackOff
	RenewBackoff       backoff.BackOff
	RebindBackoff      backoff.BackOff

	state state

	iface *net.Interface

	// now can be used to override time for testing
	now func() time.Time

	// LeaseCallback is called every time a lease is aquired, renewed or lost
	LeaseCallback LeaseCallback

	// Valid in states Discovering, Requesting, Rebinding
	broadcastConn BroadcastTransport

	// Valid in states Requesting
	offer *dhcpv4.DHCPv4

	// Valid in states Bound, Renewing
	unicastConn UnicastTransport

	// Valid in states Bound, Renewing, Rebinding
	lease              *dhcpv4.DHCPv4
	leaseDeadline      time.Time
	leaseBoundDeadline time.Time
	leaseRenewDeadline time.Time
}

// defaultBackoffOpts can be passed to NewExponentialBackOff and configures it
// to retry infinitely and use a DHCP-appropriate InitialInterval.
func defaultBackoffOpts(b *backoff.ExponentialBackOff) {
	b.MaxElapsedTime = 0 // No Timeout
	// Lots of servers wait 1s for existing users of an IP. Wait at least for
	// that and keep some slack for randomization, communication and processing
	// overhead.
	b.InitialInterval = 1400 * time.Millisecond
	b.MaxInterval = 30 * time.Second
	b.RandomizationFactor = 0.2
}

// NewClient instantiates (but doesn't start) a new DHCPv4 client.
// To have a working client it's required to set LeaseCallback to something
// that is capable of configuring the IP address on the given interface. Unless
// managed through external means like a routing protocol, setting the default
// route is also required. A simple example with the callback package thus
// looks like this:
//
//	c := dhcp4c.NewClient(yourInterface)
//	c.LeaseCallback = callback.Compose(callback.ManageIP(yourInterface), callback.ManageDefaultRoute(yourInterface))
//	c.Run(ctx)
func NewClient(iface *net.Interface) (*Client, error) {
	broadcastConn := transport.NewBroadcastTransport(iface)

	// broadcastConn needs to be open in stateDiscovering
	if err := broadcastConn.Open(); err != nil {
		return nil, fmt.Errorf("failed to create DHCP broadcast transport: %w", err)
	}

	discoverBackoff := backoff.NewExponentialBackOff(defaultBackoffOpts)

	acceptOfferBackoff := backoff.NewExponentialBackOff(defaultBackoffOpts,
		// Abort after 30s and go back to discovering
		backoff.WithMaxElapsedTime(30*time.Second))

	renewBackoff := backoff.NewExponentialBackOff(defaultBackoffOpts,
		// Increase maximum interval to reduce chatter when the server is down
		backoff.WithMaxInterval(5*time.Minute))

	rebindBackoff := backoff.NewExponentialBackOff(defaultBackoffOpts,
		// Increase maximum interval to reduce chatter when the server is down
		backoff.WithMaxInterval(5*time.Minute))

	// Check if the hardware address contains at least one non-zero value.
	// This exists to catch undefined/non-supplied hardware address values,
	// it does not check for L2 protocol-specific hardware address constraints.
	hasValidHWAddr := false
	for _, b := range iface.HardwareAddr {
		if b != 0x00 {
			hasValidHWAddr = true
			break
		}
	}
	if !hasValidHWAddr {
		return nil, fmt.Errorf("iface HardwareAddr is invalid (only zeroes or invalid length): %x", iface.HardwareAddr)
	}

	return &Client{
		state:              stateDiscovering,
		broadcastConn:      broadcastConn,
		unicastConn:        transport.NewUnicastTransport(iface),
		iface:              iface,
		RequestedOptions:   dhcpv4.OptionCodeList{},
		now:                time.Now,
		DiscoverBackoff:    discoverBackoff,
		AcceptOfferBackoff: acceptOfferBackoff,
		RenewBackoff:       renewBackoff,
		RebindBackoff:      rebindBackoff,
	}, nil
}

// acceptableLease checks if the given lease is valid enough to even be
// processed. This is intentionally not exposed to users because under certain
// cirumstances it can end up acquiring all available IP addresses from a
// server.
func (c *Client) acceptableLease(offer *dhcpv4.DHCPv4) bool {
	// RFC2131 Section 4.3.1 Table 3
	if offer.ServerIdentifier() == nil || offer.ServerIdentifier().To4() == nil {
		return false
	}
	// RFC2131 Section 4.3.1 Table 3
	// Minimum representable lease time is 1s (Section 1.1)
	if offer.IPAddressLeaseTime(0) < 1*time.Second {
		return false
	}

	// Ignore IPs that are in no way valid for an interface (multicast,
	// loopback, ...)
	if offer.YourIPAddr.To4() == nil || (!offer.YourIPAddr.IsGlobalUnicast() && !offer.YourIPAddr.IsLinkLocalUnicast()) {
		return false
	}

	// Technically the options Requested IP address, Parameter request list,
	// Client identifier and Maximum message size should be refused (MUST NOT),
	// but in the interest of interopatibilty let's simply remove them if they
	// are present.
	delete(offer.Options, dhcpv4.OptionRequestedIPAddress.Code())
	delete(offer.Options, dhcpv4.OptionParameterRequestList.Code())
	delete(offer.Options, dhcpv4.OptionClientIdentifier.Code())
	delete(offer.Options, dhcpv4.OptionMaximumDHCPMessageSize.Code())

	// Clamp rebindinding times longer than the lease time. Otherwise the state
	// machine might misbehave.
	if offer.IPAddressRebindingTime(0) > offer.IPAddressLeaseTime(0) {
		offer.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionRebindingTimeValue, dhcpv4.Duration(offer.IPAddressLeaseTime(0)).ToBytes()))
	}
	// Clamp renewal times longer than the rebinding time. Otherwise the state
	// machine might misbehave.
	if offer.IPAddressRenewalTime(0) > offer.IPAddressRebindingTime(0) {
		offer.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionRenewTimeValue, dhcpv4.Duration(offer.IPAddressRebindingTime(0)).ToBytes()))
	}

	// Normalize two options that can be represented either inline or as
	// options.
	if len(offer.ServerHostName) > 0 {
		offer.Options[uint8(dhcpv4.OptionTFTPServerName)] = []byte(offer.ServerHostName)
	}
	if len(offer.BootFileName) > 0 {
		offer.Options[uint8(dhcpv4.OptionBootfileName)] = []byte(offer.BootFileName)
	}

	// Normalize siaddr to option 150 (see RFC5859)
	if len(offer.GetOneOption(dhcpv4.OptionTFTPServerAddress)) == 0 {
		if offer.ServerIPAddr.To4() != nil && (offer.ServerIPAddr.IsGlobalUnicast() || offer.ServerIPAddr.IsLinkLocalUnicast()) {
			offer.Options[uint8(dhcpv4.OptionTFTPServerAddress)] = offer.ServerIPAddr.To4()
		}
	}

	return true
}

func earliestDeadline(dl1, dl2 time.Time) time.Time {
	if dl1.Before(dl2) {
		return dl1
	} else {
		return dl2
	}
}

// newXID generates a new transaction ID
func (c *Client) newXID() (dhcpv4.TransactionID, error) {
	var xid dhcpv4.TransactionID
	if _, err := io.ReadFull(rand.Reader, xid[:]); err != nil {
		return xid, fmt.Errorf("cannot read randomness for transaction ID: %w", err)
	}
	return xid, nil
}

// As most servers out there cannot do reassembly, let's just hope for the best
// and provide the local interface MTU. If the packet is too big it won't work
// anyways.  Also clamp to the biggest representable MTU in DHCPv4 (2 bytes
// unsigned int).
func (c *Client) maxMsgSize() uint16 {
	if c.iface.MTU < math.MaxUint16 {
		return uint16(c.iface.MTU)
	} else {
		return math.MaxUint16
	}
}

// newMsg creates a new DHCP message of a given type and adds common options.
func (c *Client) newMsg(t dhcpv4.MessageType) (*dhcpv4.DHCPv4, error) {
	xid, err := c.newXID()
	if err != nil {
		return nil, err
	}
	opts := make(dhcpv4.Options)
	opts.Update(dhcpv4.OptMessageType(t))
	if len(c.ClientIdentifier) > 0 {
		opts.Update(dhcpv4.OptClientIdentifier(c.ClientIdentifier))
	}
	if t == dhcpv4.MessageTypeDiscover || t == dhcpv4.MessageTypeRequest || t == dhcpv4.MessageTypeInform {
		opts.Update(dhcpv4.OptParameterRequestList(append(append(dhcpv4.OptionCodeList(nil), c.RequestedOptions...), internalOptions...)...))
		opts.Update(dhcpv4.OptMaxMessageSize(c.maxMsgSize()))
		if c.VendorClassIdentifier != "" {
			opts.Update(dhcpv4.OptClassIdentifier(c.VendorClassIdentifier))
		}
		for opt, val := range c.ExtraRequestOptions {
			opts[opt] = val
		}
	}
	return &dhcpv4.DHCPv4{
		OpCode:        dhcpv4.OpcodeBootRequest,
		HWType:        iana.HWTypeEthernet,
		ClientHWAddr:  c.iface.HardwareAddr,
		HopCount:      0,
		TransactionID: xid,
		NumSeconds:    0,
		Flags:         0,
		ClientIPAddr:  net.IPv4zero,
		YourIPAddr:    net.IPv4zero,
		ServerIPAddr:  net.IPv4zero,
		GatewayIPAddr: net.IPv4zero,
		Options:       opts,
	}, nil
}

// transactionStateSpec describes a state which is driven by a DHCP message
// transaction (sending a specific message and then transitioning into a
// different state depending on the received messages)
type transactionStateSpec struct {
	// ctx is a context for canceling the process
	ctx context.Context

	// transport is used to send and receive messages in this state
	transport Transport

	// stateDeadline is a fixed external deadline for how long the FSM can
	// remain in this state.
	// If it's exceeded the stateDeadlineExceeded callback is called and
	// responsible for transitioning out of this state. It can be left empty to
	// signal that there's no external deadline for the state.
	stateDeadline time.Time

	// backoff controls how long to wait for answers until handing control back
	// to the FSM.
	// Since the FSM hasn't advanced until then this means we just get called
	// again and retransmit.
	backoff backoff.BackOff

	// requestType is the type of DHCP request sent out in this state. This is
	// used to populate the default options for the message.
	requestType dhcpv4.MessageType

	// setExtraOptions can modify the request and set extra options before
	// transmitting. Returning an error here aborts the FSM an can be used to
	// terminate when no valid request can be constructed.
	setExtraOptions func(msg *dhcpv4.DHCPv4) error

	// handleMessage gets called for every parseable (not necessarily valid)
	// DHCP message received by the transport. It should return an error for
	// every message that doesn't advance the state machine and no error for
	// every one that does. It is responsible for advancing the FSM if the
	// required information is present.
	handleMessage func(msg *dhcpv4.DHCPv4, sentTime time.Time) error

	// stateDeadlineExceeded gets called if either the backoff returns
	// backoff.Stop or the stateDeadline runs out. It is responsible for
	// advancing the FSM into the next state.
	stateDeadlineExceeded func() error
}

func (c *Client) runTransactionState(s transactionStateSpec) error {
	sentTime := c.now()
	msg, err := c.newMsg(s.requestType)
	if err != nil {
		return fmt.Errorf("failed to get new DHCP message: %w", err)
	}
	if err := s.setExtraOptions(msg); err != nil {
		return fmt.Errorf("failed to create DHCP message: %w", err)
	}

	wait := s.backoff.NextBackOff()
	if wait == backoff.Stop {
		return s.stateDeadlineExceeded()
	}

	receiveDeadline := sentTime.Add(wait)
	if !s.stateDeadline.IsZero() {
		receiveDeadline = earliestDeadline(s.stateDeadline, receiveDeadline)

		// Jump out if deadline expires in less than 10ms. Minimum lease time is 1s
		// and if we have less than 10ms to wait for an answer before switching
		// state it makes no sense to send out another request. This nearly
		// eliminates the problem of sending two different requests back-to-back.
		if s.stateDeadline.Add(-10 * time.Millisecond).Before(sentTime) {
			return s.stateDeadlineExceeded()
		}
	}

	if err := s.transport.Send(msg); err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}

	if err := s.transport.SetReceiveDeadline(receiveDeadline); err != nil {
		return fmt.Errorf("failed to set deadline: %w", err)
	}

	for {
		offer, err := s.transport.Receive()
		select {
		case <-s.ctx.Done():
			c.cleanup()
			return s.ctx.Err()
		default:
		}
		if errors.Is(err, transport.ErrDeadlineExceeded) {
			return nil
		}
		var e transport.InvalidMessageError
		if errors.As(err, &e) {
			// Packet couldn't be read. Maybe log at some point in the future.
			continue
		}
		if err != nil {
			return fmt.Errorf("failed to receive packet: %w", err)
		}
		if offer.TransactionID != msg.TransactionID { // Not our transaction
			continue
		}
		err = s.handleMessage(offer, sentTime)
		if err == nil {
			return nil
		} else if !errors.Is(err, ErrInvalidMsg) {
			return err
		}
	}
}

var ErrInvalidMsg = errors.New("invalid message")

func (c *Client) runState(ctx context.Context) error {
	switch c.state {
	case stateDiscovering:
		return c.runTransactionState(transactionStateSpec{
			ctx:         ctx,
			transport:   c.broadcastConn,
			backoff:     c.DiscoverBackoff,
			requestType: dhcpv4.MessageTypeDiscover,
			setExtraOptions: func(msg *dhcpv4.DHCPv4) error {
				msg.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionRapidCommit, []byte{}))
				return nil
			},
			handleMessage: func(offer *dhcpv4.DHCPv4, sentTime time.Time) error {
				switch offer.MessageType() {
				case dhcpv4.MessageTypeOffer:
					if c.acceptableLease(offer) {
						c.offer = offer
						c.AcceptOfferBackoff.Reset()
						c.state = stateRequesting
						return nil
					}
				case dhcpv4.MessageTypeAck:
					if c.acceptableLease(offer) {
						return c.transitionToBound(offer, sentTime)
					}
				}
				return ErrInvalidMsg
			},
		})
	case stateRequesting:
		return c.runTransactionState(transactionStateSpec{
			ctx:         ctx,
			transport:   c.broadcastConn,
			backoff:     c.AcceptOfferBackoff,
			requestType: dhcpv4.MessageTypeRequest,
			setExtraOptions: func(msg *dhcpv4.DHCPv4) error {
				msg.UpdateOption(dhcpv4.OptServerIdentifier(c.offer.ServerIdentifier()))
				msg.TransactionID = c.offer.TransactionID
				msg.UpdateOption(dhcpv4.OptRequestedIPAddress(c.offer.YourIPAddr))
				return nil
			},
			handleMessage: func(msg *dhcpv4.DHCPv4, sentTime time.Time) error {
				switch msg.MessageType() {
				case dhcpv4.MessageTypeAck:
					if c.acceptableLease(msg) {
						return c.transitionToBound(msg, sentTime)
					}
				case dhcpv4.MessageTypeNak:
					c.requestingToDiscovering()
					return nil
				}
				return ErrInvalidMsg
			},
			stateDeadlineExceeded: func() error {
				c.requestingToDiscovering()
				return nil
			},
		})
	case stateBound:
		select {
		case <-time.After(c.leaseBoundDeadline.Sub(c.now())):
			c.state = stateRenewing
			c.RenewBackoff.Reset()
			return nil
		case <-ctx.Done():
			c.cleanup()
			return ctx.Err()
		}
	case stateRenewing:
		return c.runTransactionState(transactionStateSpec{
			ctx:           ctx,
			transport:     c.unicastConn,
			backoff:       c.RenewBackoff,
			requestType:   dhcpv4.MessageTypeRequest,
			stateDeadline: c.leaseRenewDeadline,
			setExtraOptions: func(msg *dhcpv4.DHCPv4) error {
				msg.ClientIPAddr = c.lease.YourIPAddr
				return nil
			},
			handleMessage: func(ack *dhcpv4.DHCPv4, sentTime time.Time) error {
				switch ack.MessageType() {
				case dhcpv4.MessageTypeAck:
					if c.acceptableLease(ack) {
						return c.transitionToBound(ack, sentTime)
					}
				case dhcpv4.MessageTypeNak:
					return c.leaseToDiscovering()
				}
				return ErrInvalidMsg
			},
			stateDeadlineExceeded: func() error {
				c.state = stateRebinding
				if err := c.switchToBroadcast(); err != nil {
					return fmt.Errorf("failed to switch to broadcast: %w", err)
				}
				c.RebindBackoff.Reset()
				return nil
			},
		})
	case stateRebinding:
		return c.runTransactionState(transactionStateSpec{
			ctx:           ctx,
			transport:     c.broadcastConn,
			backoff:       c.RebindBackoff,
			stateDeadline: c.leaseDeadline,
			requestType:   dhcpv4.MessageTypeRequest,
			setExtraOptions: func(msg *dhcpv4.DHCPv4) error {
				msg.ClientIPAddr = c.lease.YourIPAddr
				return nil
			},
			handleMessage: func(ack *dhcpv4.DHCPv4, sentTime time.Time) error {
				switch ack.MessageType() {
				case dhcpv4.MessageTypeAck:
					if c.acceptableLease(ack) {
						return c.transitionToBound(ack, sentTime)
					}
				case dhcpv4.MessageTypeNak:
					return c.leaseToDiscovering()
				}
				return ErrInvalidMsg
			},
			stateDeadlineExceeded: func() error {
				return c.leaseToDiscovering()
			},
		})
	}
	return errors.New("state machine in invalid state")
}

func (c *Client) Run(ctx context.Context) error {
	if c.LeaseCallback == nil {
		panic("LeaseCallback must be set before calling Run")
	}
	logger := supervisor.Logger(ctx)
	for {
		oldState := c.state
		if err := c.runState(ctx); err != nil {
			return err
		}
		if c.state != oldState {
			logger.Infof("%s => %s", oldState, c.state)
		}
	}
}

func (c *Client) cleanup() {
	c.unicastConn.Close()
	if c.lease != nil {
		c.LeaseCallback(nil)
	}
	c.broadcastConn.Close()
}

func (c *Client) requestingToDiscovering() {
	c.offer = nil
	c.DiscoverBackoff.Reset()
	c.state = stateDiscovering
}

func (c *Client) leaseToDiscovering() error {
	if c.state == stateRenewing {
		if err := c.switchToBroadcast(); err != nil {
			return err
		}
	}
	c.state = stateDiscovering
	c.lease = nil
	c.DiscoverBackoff.Reset()
	if err := c.LeaseCallback(nil); err != nil {
		return fmt.Errorf("lease callback failed: %w", err)
	}
	return nil
}

func leaseFromAck(ack *dhcpv4.DHCPv4, expiresAt time.Time) *Lease {
	if ack == nil {
		return nil
	}
	return &Lease{Options: ack.Options, AssignedIP: ack.YourIPAddr, ExpiresAt: expiresAt}
}

func (c *Client) transitionToBound(ack *dhcpv4.DHCPv4, sentTime time.Time) error {
	// Guaranteed to exist, leases without a lease time are filtered
	leaseTime := ack.IPAddressLeaseTime(0)
	c.leaseDeadline = sentTime.Add(leaseTime)
	c.leaseBoundDeadline = sentTime.Add(ack.IPAddressRenewalTime(time.Duration(float64(leaseTime) * 0.5)))
	c.leaseRenewDeadline = sentTime.Add(ack.IPAddressRebindingTime(time.Duration(float64(leaseTime) * 0.85)))

	if err := c.LeaseCallback(leaseFromAck(ack, c.leaseDeadline)); err != nil {
		return fmt.Errorf("lease callback failed: %w", err)
	}

	if c.state != stateRenewing {
		if err := c.switchToUnicast(ack.ServerIdentifier(), ack.YourIPAddr); err != nil {
			return fmt.Errorf("failed to switch transports: %w", err)
		}
	}
	c.state = stateBound
	c.lease = ack
	return nil
}

func (c *Client) switchToUnicast(serverIP, bindIP net.IP) error {
	if err := c.broadcastConn.Close(); err != nil {
		return fmt.Errorf("failed to close broadcast transport: %w", err)
	}
	if err := c.unicastConn.Open(serverIP, bindIP); err != nil {
		return fmt.Errorf("failed to open unicast transport: %w", err)
	}
	return nil
}

func (c *Client) switchToBroadcast() error {
	if err := c.unicastConn.Close(); err != nil {
		return fmt.Errorf("failed to close unicast transport: %w", err)
	}
	if err := c.broadcastConn.Open(); err != nil {
		return fmt.Errorf("failed to open broadcast transport: %w", err)
	}
	return nil
}
