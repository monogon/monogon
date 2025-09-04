// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package dhcp4c

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/stretchr/testify/assert"

	"source.monogon.dev/osbase/net/dhcp4c/transport"
)

type fakeTime struct {
	time time.Time
}

func newFakeTime(t time.Time) *fakeTime {
	return &fakeTime{
		time: t,
	}
}

func (ft *fakeTime) Now() time.Time {
	return ft.time
}

func (ft *fakeTime) Advance(d time.Duration) {
	ft.time = ft.time.Add(d)
}

type mockTransport struct {
	sentPacket     *dhcpv4.DHCPv4
	sendError      error
	setDeadline    time.Time
	receivePackets []*dhcpv4.DHCPv4
	receiveError   error
	receiveIdx     int
	closed         bool
}

func (mt *mockTransport) sendPackets(pkts ...*dhcpv4.DHCPv4) {
	mt.receiveIdx = 0
	mt.receivePackets = pkts
}

func (mt *mockTransport) Open() error {
	mt.closed = false
	return nil
}

func (mt *mockTransport) Send(payload *dhcpv4.DHCPv4) error {
	mt.sentPacket = payload
	return mt.sendError
}

func (mt *mockTransport) Receive() (*dhcpv4.DHCPv4, error) {
	if mt.receiveError != nil {
		return nil, mt.receiveError
	}
	if len(mt.receivePackets) > mt.receiveIdx {
		packet := mt.receivePackets[mt.receiveIdx]
		packet, err := dhcpv4.FromBytes(packet.ToBytes()) // Clone packet
		if err != nil {
			panic("ToBytes => FromBytes failed")
		}
		packet.TransactionID = mt.sentPacket.TransactionID
		mt.receiveIdx++
		return packet, nil
	}
	return nil, transport.ErrDeadlineExceeded
}

func (mt *mockTransport) SetReceiveDeadline(t time.Time) error {
	mt.setDeadline = t
	return nil
}

func (mt *mockTransport) Close() error {
	mt.closed = true
	return nil
}

type unicastMockTransport struct {
	mockTransport
	serverIP net.IP
	bindIP   net.IP
}

func (umt *unicastMockTransport) Open(serverIP, bindIP net.IP) error {
	if umt.serverIP != nil {
		panic("double-open of unicast transport")
	}
	umt.serverIP = serverIP
	umt.bindIP = bindIP
	return nil
}

func (umt *unicastMockTransport) Close() error {
	umt.serverIP = nil
	umt.bindIP = nil
	return umt.mockTransport.Close()
}

type mockBackoff struct {
	indefinite bool
	values     []time.Duration
	idx        int
}

func newMockBackoff(vals []time.Duration, indefinite bool) *mockBackoff {
	return &mockBackoff{values: vals, indefinite: indefinite}
}

func (mb *mockBackoff) NextBackOff() time.Duration {
	if mb.idx < len(mb.values) || mb.indefinite {
		val := mb.values[mb.idx%len(mb.values)]
		mb.idx++
		return val
	}
	return backoff.Stop
}

func (mb *mockBackoff) Reset() {
	mb.idx = 0
}

func TestClient_runTransactionState(t *testing.T) {
	ft := newFakeTime(time.Date(2020, 10, 28, 15, 02, 32, 352, time.UTC))
	c := Client{
		now:   ft.Now,
		iface: &net.Interface{MTU: 9324, HardwareAddr: net.HardwareAddr{0x12, 0x23, 0x34, 0x45, 0x56, 0x67}},
	}
	mt := &mockTransport{}
	err := c.runTransactionState(transactionStateSpec{
		ctx:         context.Background(),
		transport:   mt,
		backoff:     newMockBackoff([]time.Duration{1 * time.Second}, true),
		requestType: dhcpv4.MessageTypeDiscover,
		setExtraOptions: func(msg *dhcpv4.DHCPv4) error {
			msg.UpdateOption(dhcpv4.OptDomainName("just.testing.invalid"))
			return nil
		},
		handleMessage: func(msg *dhcpv4.DHCPv4, sentTime time.Time) error {
			return nil
		},
		stateDeadlineExceeded: func() error {
			panic("shouldn't be called")
		},
	})
	assert.NoError(t, err)
	assert.Equal(t, "just.testing.invalid", mt.sentPacket.DomainName())
	assert.Equal(t, dhcpv4.MessageTypeDiscover, mt.sentPacket.MessageType())
}

// TestAcceptableLease tests if a minimal valid lease is accepted by
// acceptableLease
func TestAcceptableLease(t *testing.T) {
	var c Client
	offer := &dhcpv4.DHCPv4{
		OpCode: dhcpv4.OpcodeBootReply,
	}
	offer.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeOffer))
	offer.UpdateOption(dhcpv4.OptServerIdentifier(net.IP{192, 0, 2, 1}))
	offer.UpdateOption(dhcpv4.OptIPAddressLeaseTime(10 * time.Second))
	offer.YourIPAddr = net.IP{192, 0, 2, 2}
	assert.True(t, c.acceptableLease(offer), "Valid lease is not acceptable")
}

type dhcpClientPuppet struct {
	ft  *fakeTime
	bmt *mockTransport
	umt *unicastMockTransport
	c   *Client
}

func newPuppetClient(initState state) *dhcpClientPuppet {
	ft := newFakeTime(time.Date(2020, 10, 28, 15, 02, 32, 352, time.UTC))
	bmt := &mockTransport{}
	umt := &unicastMockTransport{}
	c := &Client{
		state:              initState,
		now:                ft.Now,
		iface:              &net.Interface{MTU: 9324, HardwareAddr: net.HardwareAddr{0x12, 0x23, 0x34, 0x45, 0x56, 0x67}},
		broadcastConn:      bmt,
		unicastConn:        umt,
		DiscoverBackoff:    newMockBackoff([]time.Duration{1 * time.Second}, true),
		AcceptOfferBackoff: newMockBackoff([]time.Duration{1 * time.Second, 2 * time.Second}, false),
		RenewBackoff:       newMockBackoff([]time.Duration{1 * time.Second}, true),
		RebindBackoff:      newMockBackoff([]time.Duration{1 * time.Second}, true),
	}
	return &dhcpClientPuppet{
		ft:  ft,
		bmt: bmt,
		umt: umt,
		c:   c,
	}
}

func newResponse(m dhcpv4.MessageType) *dhcpv4.DHCPv4 {
	o := &dhcpv4.DHCPv4{
		OpCode: dhcpv4.OpcodeBootReply,
	}
	o.UpdateOption(dhcpv4.OptMessageType(m))
	return o
}

// TestDiscoverOffer tests if the DHCP state machine in discovering state
// properly selects the first valid lease and transitions to requesting state
func TestDiscoverRequesting(t *testing.T) {
	p := newPuppetClient(stateDiscovering)

	// A minimal valid lease
	offer := newResponse(dhcpv4.MessageTypeOffer)
	testIP := net.IP{192, 0, 2, 2}
	offer.UpdateOption(dhcpv4.OptServerIdentifier(net.IP{192, 0, 2, 1}))
	offer.UpdateOption(dhcpv4.OptIPAddressLeaseTime(10 * time.Second))
	offer.YourIPAddr = testIP

	// Intentionally bad offer with no lease time.
	terribleOffer := newResponse(dhcpv4.MessageTypeOffer)
	terribleOffer.UpdateOption(dhcpv4.OptServerIdentifier(net.IP{192, 0, 2, 2}))
	terribleOffer.YourIPAddr = net.IPv4(192, 0, 2, 2)

	// Send the bad offer first, then the valid offer
	p.bmt.sendPackets(terribleOffer, offer)

	if err := p.c.runState(context.Background()); err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, stateRequesting, p.c.state, "DHCP client didn't process offer")
	assert.Equal(t, testIP, p.c.offer.YourIPAddr, "DHCP client requested invalid offer")
}

// TestOfferBound tests if the DHCP state machine in requesting state processes
// a valid DHCPACK and transitions to bound state.
func TestRequestingBound(t *testing.T) {
	p := newPuppetClient(stateRequesting)

	offer := newResponse(dhcpv4.MessageTypeAck)
	testIP := net.IP{192, 0, 2, 2}
	offer.UpdateOption(dhcpv4.OptServerIdentifier(net.IP{192, 0, 2, 1}))
	offer.UpdateOption(dhcpv4.OptIPAddressLeaseTime(10 * time.Second))
	offer.YourIPAddr = testIP

	p.bmt.sendPackets(offer)
	p.c.offer = offer
	p.c.LeaseCallback = func(lease *Lease) error {
		assert.Equal(t, testIP, lease.AssignedIP, "new lease has wrong IP")
		return nil
	}

	if err := p.c.runState(context.Background()); err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, stateBound, p.c.state, "DHCP client didn't process offer")
	assert.Equal(t, testIP, p.c.lease.YourIPAddr, "DHCP client requested invalid offer")
}

// TestRequestingDiscover tests if the DHCP state machine in requesting state
// transitions back to discovering if it takes too long to get a valid DHCPACK.
func TestRequestingDiscover(t *testing.T) {
	p := newPuppetClient(stateRequesting)

	offer := newResponse(dhcpv4.MessageTypeOffer)
	testIP := net.IP{192, 0, 2, 2}
	offer.UpdateOption(dhcpv4.OptServerIdentifier(net.IP{192, 0, 2, 1}))
	offer.UpdateOption(dhcpv4.OptIPAddressLeaseTime(10 * time.Second))
	offer.YourIPAddr = testIP
	p.c.offer = offer

	for i := 0; i < 10; i++ {
		p.bmt.sendPackets()
		if err := p.c.runState(context.Background()); err != nil {
			t.Fatal(err)
		}
		assert.Equal(t, dhcpv4.MessageTypeRequest, p.bmt.sentPacket.MessageType(), "Invalid message type for requesting")
		if p.c.state == stateDiscovering {
			break
		}
		p.ft.time = p.bmt.setDeadline

		if i == 9 {
			t.Fatal("Too many tries while requesting, backoff likely wrong")
		}
	}
	assert.Equal(t, stateDiscovering, p.c.state, "DHCP client didn't switch back to offer after requesting expired")
}

// TestDiscoverRapidCommit tests if the DHCP state machine in discovering state
// transitions directly to bound if a rapid commit response (DHCPACK) is
// received.
func TestDiscoverRapidCommit(t *testing.T) {
	testIP := net.IP{192, 0, 2, 2}
	offer := newResponse(dhcpv4.MessageTypeAck)
	offer.UpdateOption(dhcpv4.OptServerIdentifier(net.IP{192, 0, 2, 1}))
	leaseTime := 10 * time.Second
	offer.UpdateOption(dhcpv4.OptIPAddressLeaseTime(leaseTime))
	offer.YourIPAddr = testIP

	p := newPuppetClient(stateDiscovering)
	p.c.LeaseCallback = func(lease *Lease) error {
		assert.Equal(t, testIP, lease.AssignedIP, "callback called with wrong IP")
		assert.Equal(t, p.ft.Now().Add(leaseTime), lease.ExpiresAt, "invalid ExpiresAt")
		return nil
	}
	p.bmt.sendPackets(offer)
	if err := p.c.runState(context.Background()); err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, stateBound, p.c.state, "DHCP client didn't process offer")
	assert.Equal(t, testIP, p.c.lease.YourIPAddr, "DHCP client requested invalid offer")
	assert.Equal(t, 5*time.Second, p.c.leaseBoundDeadline.Sub(p.ft.Now()), "Renewal time was incorrectly defaulted")
}

type TestOption uint8

func (o TestOption) Code() uint8 {
	return uint8(o) + 224 // Private options
}
func (o TestOption) String() string {
	return fmt.Sprintf("Test Option %d", uint8(o))
}

// TestBoundRenewingBound tests if the DHCP state machine in bound correctly
// transitions to renewing after leaseBoundDeadline expires, sends a
// DHCPREQUEST and after it gets a DHCPACK response calls LeaseCallback and
// transitions back to bound with correct new deadlines.
func TestBoundRenewingBound(t *testing.T) {
	offer := newResponse(dhcpv4.MessageTypeAck)
	testIP := net.IP{192, 0, 2, 2}
	serverIP := net.IP{192, 0, 2, 1}
	offer.UpdateOption(dhcpv4.OptServerIdentifier(serverIP))
	leaseTime := 10 * time.Second
	offer.UpdateOption(dhcpv4.OptIPAddressLeaseTime(leaseTime))
	offer.YourIPAddr = testIP

	p := newPuppetClient(stateBound)
	p.umt.Open(serverIP, testIP)
	p.c.lease, _ = dhcpv4.FromBytes(offer.ToBytes())
	// Other deadlines are intentionally empty to make sure they aren't used
	p.c.leaseRenewDeadline = p.ft.Now().Add(8500 * time.Millisecond)
	p.c.leaseBoundDeadline = p.ft.Now().Add(5000 * time.Millisecond)

	p.ft.Advance(5*time.Second - 5*time.Millisecond)
	if err := p.c.runState(context.Background()); err != nil {
		t.Fatal(err)
	}
	// We cannot intercept time.After so we just advance the clock by the time slept
	p.ft.Advance(5 * time.Millisecond)
	assert.Equal(t, stateRenewing, p.c.state, "DHCP client not renewing")
	offer.UpdateOption(dhcpv4.OptGeneric(TestOption(1), []byte{0x12}))
	p.umt.sendPackets(offer)
	p.c.LeaseCallback = func(lease *Lease) error {
		assert.Equal(t, testIP, lease.AssignedIP, "callback called with wrong IP")
		assert.Equal(t, p.ft.Now().Add(leaseTime), lease.ExpiresAt, "invalid ExpiresAt")
		assert.Equal(t, []byte{0x12}, lease.Options.Get(TestOption(1)), "renewal didn't add new option")
		return nil
	}
	if err := p.c.runState(context.Background()); err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, stateBound, p.c.state, "DHCP client didn't renew")
	assert.Equal(t, p.ft.Now().Add(leaseTime), p.c.leaseDeadline, "lease deadline not updated")
	assert.Equal(t, dhcpv4.MessageTypeRequest, p.umt.sentPacket.MessageType(), "Invalid message type for renewal")
}

// TestRenewingRebinding tests if the DHCP state machine in renewing state
// correctly sends DHCPREQUESTs and transitions to the rebinding state when it
// hasn't received a valid response until the deadline expires.
func TestRenewingRebinding(t *testing.T) {
	offer := newResponse(dhcpv4.MessageTypeAck)
	testIP := net.IP{192, 0, 2, 2}
	serverIP := net.IP{192, 0, 2, 1}
	offer.UpdateOption(dhcpv4.OptServerIdentifier(serverIP))
	leaseTime := 10 * time.Second
	offer.UpdateOption(dhcpv4.OptIPAddressLeaseTime(leaseTime))
	offer.YourIPAddr = testIP

	p := newPuppetClient(stateRenewing)
	p.umt.Open(serverIP, testIP)
	p.c.lease, _ = dhcpv4.FromBytes(offer.ToBytes())
	// Other deadlines are intentionally empty to make sure they aren't used
	p.c.leaseRenewDeadline = p.ft.Now().Add(8500 * time.Millisecond)
	p.c.leaseDeadline = p.ft.Now().Add(10000 * time.Millisecond)

	startTime := p.ft.Now()
	p.ft.Advance(5 * time.Second)

	p.c.LeaseCallback = func(*Lease) error {
		t.Fatal("Lease callback called without valid offer")
		return nil
	}

	for i := 0; i < 10; i++ {
		p.umt.sendPackets()
		if err := p.c.runState(context.Background()); err != nil {
			t.Fatal(err)
		}
		assert.Equal(t, dhcpv4.MessageTypeRequest, p.umt.sentPacket.MessageType(), "Invalid message type for renewal")
		p.ft.time = p.umt.setDeadline

		if p.c.state == stateRebinding {
			break
		}
		if i == 9 {
			t.Fatal("Too many tries while renewing, backoff likely wrong")
		}
	}
	assert.Equal(t, startTime.Add(8500*time.Millisecond), p.umt.setDeadline, "wrong listen deadline when renewing")
	assert.Equal(t, stateRebinding, p.c.state, "DHCP client not renewing")
	assert.False(t, p.bmt.closed)
	assert.True(t, p.umt.closed)
}

// TestRebindingBound tests if the DHCP state machine in rebinding state sends
// DHCPREQUESTs to the network and if it receives a valid DHCPACK correctly
// transitions back to bound state.
func TestRebindingBound(t *testing.T) {
	offer := newResponse(dhcpv4.MessageTypeAck)
	testIP := net.IP{192, 0, 2, 2}
	serverIP := net.IP{192, 0, 2, 1}
	offer.UpdateOption(dhcpv4.OptServerIdentifier(serverIP))
	leaseTime := 10 * time.Second
	offer.UpdateOption(dhcpv4.OptIPAddressLeaseTime(leaseTime))
	offer.YourIPAddr = testIP

	p := newPuppetClient(stateRebinding)
	p.c.lease, _ = dhcpv4.FromBytes(offer.ToBytes())
	// Other deadlines are intentionally empty to make sure they aren't used
	p.c.leaseDeadline = p.ft.Now().Add(10000 * time.Millisecond)

	p.ft.Advance(9 * time.Second)
	if err := p.c.runState(context.Background()); err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, dhcpv4.MessageTypeRequest, p.bmt.sentPacket.MessageType(), "DHCP rebind sent invalid message type")
	assert.Equal(t, stateRebinding, p.c.state, "DHCP client transferred out of rebinding state without trigger")
	offer.UpdateOption(dhcpv4.OptGeneric(TestOption(1), []byte{0x12})) // Mark answer
	p.bmt.sendPackets(offer)
	p.bmt.sentPacket = nil
	p.c.LeaseCallback = func(lease *Lease) error {
		assert.Equal(t, testIP, lease.AssignedIP, "callback called with wrong IP")
		assert.Equal(t, p.ft.Now().Add(leaseTime), lease.ExpiresAt, "invalid ExpiresAt")
		assert.Equal(t, []byte{0x12}, lease.Options.Get(TestOption(1)), "renewal didn't add new option")
		return nil
	}
	if err := p.c.runState(context.Background()); err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, dhcpv4.MessageTypeRequest, p.bmt.sentPacket.MessageType())
	assert.Equal(t, stateBound, p.c.state, "DHCP client didn't go back to bound")
}

// TestRebindingBound tests if the DHCP state machine in rebinding state
// transitions to discovering state if leaseDeadline expires and calls
// LeaseCallback with an empty new lease.
func TestRebindingDiscovering(t *testing.T) {
	offer := newResponse(dhcpv4.MessageTypeAck)
	testIP := net.IP{192, 0, 2, 2}
	serverIP := net.IP{192, 0, 2, 1}
	offer.UpdateOption(dhcpv4.OptServerIdentifier(serverIP))
	leaseTime := 10 * time.Second
	offer.UpdateOption(dhcpv4.OptIPAddressLeaseTime(leaseTime))
	offer.YourIPAddr = testIP

	p := newPuppetClient(stateRebinding)
	p.c.lease, _ = dhcpv4.FromBytes(offer.ToBytes())
	// Other deadlines are intentionally empty to make sure they aren't used
	p.c.leaseDeadline = p.ft.Now().Add(10000 * time.Millisecond)

	p.ft.Advance(9 * time.Second)
	p.c.LeaseCallback = func(lease *Lease) error {
		assert.Nil(t, lease, "transition to discovering didn't clear new lease on callback")
		return nil
	}
	for i := 0; i < 10; i++ {
		p.bmt.sendPackets()
		p.bmt.sentPacket = nil
		if err := p.c.runState(context.Background()); err != nil {
			t.Fatal(err)
		}
		if p.c.state == stateDiscovering {
			assert.Nil(t, p.bmt.sentPacket)
			break
		}
		assert.Equal(t, dhcpv4.MessageTypeRequest, p.bmt.sentPacket.MessageType(), "Invalid message type for rebind")
		p.ft.time = p.bmt.setDeadline
		if i == 9 {
			t.Fatal("Too many tries while rebinding, backoff likely wrong")
		}
	}
	assert.Nil(t, p.c.lease, "Lease not zeroed on transition to discovering")
	assert.Equal(t, stateDiscovering, p.c.state, "DHCP client didn't transition to discovering after loosing lease")
}
