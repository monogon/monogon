// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package network

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/vishvananda/netlink"

	"source.monogon.dev/metropolis/node"
	"source.monogon.dev/metropolis/node/core/clusternet"
	"source.monogon.dev/metropolis/node/core/network/dhcp4c"
	dhcpcb "source.monogon.dev/metropolis/node/core/network/dhcp4c/callback"
	"source.monogon.dev/metropolis/node/core/network/workloads"
	"source.monogon.dev/osbase/event"
	"source.monogon.dev/osbase/event/memory"
	"source.monogon.dev/osbase/net/dns"
	"source.monogon.dev/osbase/net/dns/forward"
	"source.monogon.dev/osbase/supervisor"
	"source.monogon.dev/osbase/sysctl"

	netpb "source.monogon.dev/osbase/net/proto"
)

// Service is the network service for this node. It maintains all
// networking-related functionality, but is generally not aware of the inner
// workings of Metropolis, instead functioning in a generic manner. Once created
// via New, it can be started and restarted arbitrarily, but the service object
// itself must be long-lived.
type Service struct {
	// If set, use the given static network configuration instead of relying on
	// autoconfiguration.
	StaticConfig *netpb.Net

	// Vendor Class identifier of the system
	DHCPVendorClassID string

	DNS        *dns.Service
	dnsForward *forward.Forward

	// dhcp client for the 'main' interface of the node.
	dhcp *dhcp4c.Client
	// dhcpAddress is the current address obtained from DHCP.
	dhcpAddress net.IP
	// dnsServers are the current DNS servers obtained from DHCP.
	dnsServers dhcp4c.DNSServers

	// nftConn is a shared file descriptor handle to nftables, automatically
	// initialized on first use.
	nftConn             nftables.Conn
	natTable            *nftables.Table
	natPostroutingChain *nftables.Chain

	// Status is the current status of the network as seen by the service.
	Status memory.Value[*node.NetStatus]

	workloadSvc *workloads.Service
}

// New instantiates a new network service. If autoconfiguration is desired,
// staticConfig must be set to nil. If staticConfig is set to a non-nil value,
// it will be used instead of autoconfiguration.
// If dnsHandlerNames is non-nil, DNS handlers with these names must be set
// on the DNS service with s.DNS.SetHandler. When serving DNS queries, they
// will be tried in the order they appear here before forwarding.
func New(staticConfig *netpb.Net, dnsHandlerNames []string, ipamPrefixSrc event.Value[*clusternet.Prefixes]) *Service {
	dnsSvc := dns.New(slices.Concat(dnsHandlerNames, []string{"forward"}))
	dnsForward := forward.New()
	dnsSvc.SetHandler("forward", dnsForward)

	var wlSvc *workloads.Service
	if ipamPrefixSrc != nil {
		wlSvc = workloads.New(ipamPrefixSrc)
	}

	return &Service{
		DNS:          dnsSvc,
		dnsForward:   dnsForward,
		StaticConfig: staticConfig,
		workloadSvc:  wlSvc,
	}
}

func singleIPtoNetlinkAddr(ip net.IP, label string) *netlink.Addr {
	var mask net.IPMask
	if ip.To4() == nil {
		mask = net.CIDRMask(128, 128) // IPv6 /128
	} else {
		mask = net.CIDRMask(32, 32) // IPv4 /32
	}
	scope := netlink.SCOPE_UNIVERSE
	if ip.IsLinkLocalUnicast() {
		scope = netlink.SCOPE_LINK
	}
	if ip.IsLoopback() {
		scope = netlink.SCOPE_HOST
	}
	return &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   ip,
			Mask: mask,
		},
		Label: label,
		Scope: int(scope),
	}
}

// AddLoopbackIP adds the given IP to a loopback interface which can then be
// used to bind listeners to. Once this function returns, the IP is assigned and
// is ready to use. It's recommended to use defer to call ReleaseLoopbackIP to
// make sure IPs are released when the goroutine using it exits.
func (s *Service) AddLoopbackIP(ip net.IP) error {
	loopbackIf, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("no loopback interface: %w", err)
	}
	if err := netlink.AddrAdd(loopbackIf, singleIPtoNetlinkAddr(ip, "localsvc")); err != nil {
		return fmt.Errorf("failed to add IP: %w", err)
	}
	return nil
}

// ReleaseLoopbackIP releases an IP allocated by AddLoopbackIP.
// Calling it multiple times for the same IP is an error.
func (s *Service) ReleaseLoopbackIP(ip net.IP) error {
	loopbackIf, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("no loopback interface: %w", err)
	}
	if err := netlink.AddrDel(loopbackIf, singleIPtoNetlinkAddr(ip, "localsvc")); err != nil {
		return fmt.Errorf("failed to delete IP: %w", err)
	}
	return nil
}

// nfifname converts an interface name into 16 bytes padded with zeroes (for
// nftables)
func nfifname(n string) []byte {
	b := make([]byte, 16)
	copy(b, n+"\x00")
	return b
}

// statusCallback is the main DHCP client callback connecting updates to the
// current lease to the rest of Metropolis. It updates the DNS service's
// configuration to use the received upstream servers, and notifies the rest of
// Metropolis via an event value that the network configuration has changed.
func (s *Service) statusCallback(ctx context.Context) dhcp4c.LeaseCallback {
	return func(lease *dhcp4c.Lease) error {
		// Reconfigure DNS if needed.
		newServers := lease.DNSServers()
		if !newServers.Equal(s.dnsServers) {
			s.dnsServers = newServers
			newAddrs := make([]string, len(newServers))
			for i, ip := range newServers {
				newAddrs[i] = net.JoinHostPort(ip.String(), "53")
			}
			s.dnsForward.DNSServers.Set(newAddrs)
		}

		var newAddress net.IP
		if lease != nil {
			newAddress = lease.AssignedIP
		}
		if !newAddress.Equal(s.dhcpAddress) {
			s.dhcpAddress = newAddress
			// Notify status waiters.
			s.Status.Set(&node.NetStatus{
				ExternalAddress: newAddress,
			})
			if newAddress != nil {
				supervisor.Logger(ctx).Infof("New DHCP address: %s", newAddress)
			} else {
				supervisor.Logger(ctx).Warning("Lost DHCP address")
			}
		}
		return nil
	}
}

func (s *Service) useInterface(ctx context.Context, iface netlink.Link) error {
	var err error
	s.dhcp, err = dhcp4c.NewClient(netlinkLinkToNetInterface(iface))
	if err != nil {
		return fmt.Errorf("failed to create DHCP client on interface %v: %w", iface.Attrs().Name, err)
	}
	s.dhcp.VendorClassIdentifier = s.DHCPVendorClassID
	s.dhcp.RequestedOptions = []dhcpv4.OptionCode{dhcpv4.OptionRouter, dhcpv4.OptionDomainNameServer, dhcpv4.OptionClasslessStaticRoute}
	s.dhcp.LeaseCallback = dhcpcb.Compose(dhcpcb.ManageIP(iface), arpAnnounceCB(iface), dhcpcb.ManageRoutes(iface), s.statusCallback(ctx))
	err = supervisor.Run(ctx, "dhcp", s.dhcp.Run)
	if err != nil {
		return err
	}

	return nil
}

// RFC2474 Section 4.2.2.1 with reference to RFC791 Section 3.1 (Network
// Control Precedence)
const dscpCS7 = 0x7 << 3

func (s *Service) Run(ctx context.Context) error {
	logger := supervisor.Logger(ctx)

	earlySysctlOpts := sysctl.Options{
		// Enable strict reverse path filtering on all interfaces (important
		// for spoofing prevention from Pods with CAP_NET_ADMIN)
		"net.ipv4.conf.all.rp_filter": "1",
		// Disable source routing
		"net.ipv4.conf.all.accept_source_route": "0",
		// By default no interfaces should accept router advertisements.
		// This will be selectively enabled on the appropriate interfaces.
		"net.ipv6.conf.all.accept_ra": "0",
		// Make static IPs stick around, otherwise we have to configure them
		// again after carrier loss events.
		"net.ipv6.conf.all.keep_addr_on_down": "1",
		// Make neighbor discovery use DSCP CS7 without ECN
		"net.ipv6.conf.all.ndisc_tclass": strconv.Itoa(dscpCS7 << 2),
	}
	if err := earlySysctlOpts.Apply(); err != nil {
		logger.Fatalf("Error configuring early sysctl options: %v", err)
	}

	if err := applyQuirks(logger); err != nil {
		logger.Errorf("Applying quirks failed, continuing without: %v", err)
	}

	loopbackIf, err := netlink.LinkByName("lo")
	if err != nil {
		logger.Fatalf("No loopback interface: %v", err)
	}
	if err := netlink.LinkSetUp(loopbackIf); err != nil {
		logger.Errorf("Failed to bring up loopback interface: %v", err)
	}

	supervisor.Run(ctx, "linkstate", s.runLinkState)

	// Choose between autoconfig and static config runnables
	if s.StaticConfig == nil {
		supervisor.Run(ctx, "dynamic", s.runDynamicConfig)
	} else {
		supervisor.Run(ctx, "static", s.runStaticConfig)
	}

	supervisor.Run(ctx, "dns", s.DNS.Run)
	supervisor.Run(ctx, "dns-forward", s.dnsForward.Run)

	if s.workloadSvc != nil {
		supervisor.Run(ctx, "workloads", s.workloadSvc.Run)
	}

	s.natTable = s.nftConn.AddTable(&nftables.Table{
		Family: nftables.TableFamilyIPv4,
		Name:   "nat",
	})

	s.natPostroutingChain = s.nftConn.AddChain(&nftables.Chain{
		Name:     "postrouting",
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityNATSource,
		Table:    s.natTable,
		Type:     nftables.ChainTypeNAT,
	})
	// SNAT/Masquerade all traffic coming from pod interface (identified by
	// group) not going to another pod, either local or over clusternet.
	// Will need to be changed when we support L3 attachments (BGP, ...).
	s.nftConn.AddRule(&nftables.Rule{
		Table: s.natTable,
		Chain: s.natPostroutingChain,
		Exprs: []expr.Any{
			&expr.Meta{
				Key:      expr.MetaKeyIIFGROUP,
				Register: 8,
			},
			// Check if incoming interface is a K8s pod
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 8,
				Data:     binaryutil.NativeEndian.PutUint32(node.LinkGroupK8sPod),
			},
			&expr.Meta{
				Key:      expr.MetaKeyOIFGROUP,
				Register: 8,
			},
			// Check if outgoing interface is not a K8s pod
			&expr.Cmp{
				Op:       expr.CmpOpNeq,
				Register: 8,
				Data:     binaryutil.NativeEndian.PutUint32(node.LinkGroupK8sPod),
			},
			// Check if outgoing interface isn't clusternet
			&expr.Cmp{
				Op:       expr.CmpOpNeq,
				Register: 8,
				Data:     binaryutil.NativeEndian.PutUint32(node.LinkGroupClusternet),
			},
			&expr.Masq{},
		},
	})

	if err := s.nftConn.Flush(); err != nil {
		logger.Fatalf("Failed to set up nftables nat chain: %v", err)
	}

	sysctlOpts := sysctl.Options{
		// Enable IP forwarding for our pods
		"net.ipv4.ip_forward": "1",

		// Increase Linux socket kernel buffer sizes to 16MiB (needed for fast
		// datacenter networks)
		"net.core.rmem_max": "16777216",
		"net.core.wmem_max": "16777216",
		"net.ipv4.tcp_rmem": "4096 87380 16777216",
		"net.ipv4.tcp_wmem": "4096 87380 16777216",
	}
	if err := sysctlOpts.Apply(); err != nil {
		logger.Fatalf("Failed to set up kernel network config: %v", err)
	}

	supervisor.Signal(ctx, supervisor.SignalHealthy)
	supervisor.Signal(ctx, supervisor.SignalDone)
	return nil
}

func (s *Service) runDynamicConfig(ctx context.Context) error {
	logger := supervisor.Logger(ctx)
	logger.Info("Starting network interface management")

	links, err := netlink.LinkList()
	if err != nil {
		logger.Fatalf("Failed to list network links: %s", err)
	}

	var ethernetLinks []netlink.Link
	for _, link := range links {
		attrs := link.Attrs()
		if link.Type() == "device" && len(attrs.HardwareAddr) > 0 {
			if len(attrs.HardwareAddr) == 6 { // Ethernet
				if attrs.Flags&net.FlagUp != net.FlagUp {
					netlink.LinkSetUp(link) // Attempt to take up all ethernet links
				}
				ethernetLinks = append(ethernetLinks, link)
			} else {
				logger.Infof("Ignoring non-Ethernet interface %s", attrs.Name)
			}
		}
	}
	if len(ethernetLinks) != 1 {
		logger.Warningf("Network service needs exactly one link, bailing")
	} else {
		link := ethernetLinks[0]
		if err := s.useInterface(ctx, link); err != nil {
			return fmt.Errorf("failed to bring up link %s: %w", link.Attrs().Name, err)
		}
		logger.Infof("Network service using interface %s", link.Attrs().HardwareAddr.String())
	}

	supervisor.Signal(ctx, supervisor.SignalHealthy)
	supervisor.Signal(ctx, supervisor.SignalDone)
	return nil
}
