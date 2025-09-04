// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package network

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"source.monogon.dev/go/algorithm/toposort"
	"source.monogon.dev/go/logging"
	"source.monogon.dev/metropolis/node"
	"source.monogon.dev/osbase/net/dhcp4c"
	dhcpcb "source.monogon.dev/osbase/net/dhcp4c/callback"
	"source.monogon.dev/osbase/supervisor"
	"source.monogon.dev/osbase/sysctl"

	netpb "source.monogon.dev/osbase/net/proto"
)

var vlanProtoMap = map[netpb.VLAN_Protocol]netlink.VlanProtocol{
	netpb.VLAN_PROTOCOL_CVLAN: netlink.VLAN_PROTOCOL_8021Q,
	netpb.VLAN_PROTOCOL_SVLAN: netlink.VLAN_PROTOCOL_8021AD,
}

func (s *Service) runStaticConfig(ctx context.Context) error {
	l := supervisor.Logger(ctx)
	var success bool
	sortedInterfaces, err := getSortedIfaces(s)
	if err != nil {
		return err
	}

	hostDevices, err := listHostDeviceIfaces()
	if err != nil {
		return err
	}

	var hasIPv4Autoconfig bool

	nameLinkMap := make(map[string]netlink.Link)

	// interface name -> parent interface name
	nameParentMap := make(map[string]string)

	for _, i := range sortedInterfaces {
		var newLink netlink.Link
		var err error
		switch it := i.Type.(type) {
		case *netpb.Interface_Device:
			newLink, err = deviceIfaceFromSpec(it, hostDevices, l)
		case *netpb.Interface_Bond:
			for _, m := range it.Bond.MemberInterface {
				nameParentMap[m] = i.Name
			}
			newLink, err = bondIfaceFromSpec(it, i)
		case *netpb.Interface_Vlan:
			newLink = &netlink.Vlan{
				VlanId:       int(it.Vlan.Id),
				VlanProtocol: vlanProtoMap[it.Vlan.Protocol],
				LinkAttrs:    netlink.NewLinkAttrs(),
			}
			newLink.Attrs().ParentIndex = nameLinkMap[it.Vlan.Parent].Attrs().Index
		}
		if err != nil {
			return fmt.Errorf("interface %q: %w", i.Name, err)
		}
		newLink.Attrs().Name = i.Name
		if i.Mtu != 0 {
			newLink.Attrs().MTU = int(i.Mtu)
		}
		if nameParentMap[i.Name] != "" {
			newLink.Attrs().MasterIndex = nameLinkMap[nameParentMap[i.Name]].Attrs().Index
		} else {
			// Set link administratively up if no MasterIndex has been set.
			newLink.Attrs().Flags |= net.FlagUp
		}
		if newLink.Attrs().Index <= 0 {
			if err := netlink.LinkAdd(newLink); err != nil {
				return fmt.Errorf("failed to add link %q: %w", i.Name, err)
			}
			defer func() {
				if !success {
					if err := netlink.LinkDel(newLink); err != nil {
						l.Errorf("Failed to delete link on teardown: %v", err)
					}
				}
			}()
		} else {
			if err := netlink.LinkModify(newLink); err != nil {
				return fmt.Errorf("failed to modify link %q: %w", i.Name, err)
			}
			defer func() {
				if !success {
					if err := netlink.LinkSetDown(newLink); err != nil {
						l.Errorf("Failed to set link down: %v", err)
					}
				}
			}()
		}
		nameLinkMap[i.Name] = newLink
		// Get the link from the kernel again to get all defaulted and
		// hardware-derived attributes.
		newLinkWithAttrs, err := netlink.LinkByIndex(newLink.Attrs().Index)
		if err != nil {
			return fmt.Errorf("while getting just-configured link: %w", err)
		}
		if i.Ipv4Autoconfig != nil {
			if err := s.runDHCPv4(ctx, newLinkWithAttrs); err != nil {
				return fmt.Errorf("error enabling DHCPv4 on %q: %w", newLink.Attrs().Name, err)
			}
			hasIPv4Autoconfig = true
		}
		if i.Ipv6Autoconfig != nil {
			opts := sysctl.Options{
				"net.ipv6.conf." + newLinkWithAttrs.Attrs().Name + ".accept_ra": "1",
			}
			if err := opts.Apply(); err != nil {
				return fmt.Errorf("failed enabling accept_ra for interface %q: %w", newLink.Attrs().Name, err)
			}
			// TODO(lorenz): Actually implement DHCPv6/Managed flag
		}
		for _, a := range i.Address {
			if err := addAddrFromSpec(a, newLink); err != nil {
				return fmt.Errorf("failed adding address %q to link: %w", a, err)
			}
		}
		for _, r := range i.Route {
			if err := routeFromSpec(r, newLink); err != nil {
				return fmt.Errorf("failed creating route on interface %q: %w", i.Name, err)
			}
		}
		l.Infof("Configured interface %q", i.Name)
	}
	var nsAddrList []string
	for _, ns := range s.StaticConfig.Nameserver {
		nsIP := net.ParseIP(ns.Ip)
		if nsIP == nil {
			l.Warningf("failed to parse %q as nameserver IP", ns.Ip)
			continue
		}
		nsAddr := net.JoinHostPort(nsIP.String(), "53")
		nsAddrList = append(nsAddrList, nsAddr)
	}
	if len(nsAddrList) > 0 {
		s.dnsForward.DNSServers.Set(nsAddrList)
	}

	if !hasIPv4Autoconfig {
		var selectedAddr net.IP
	ifLoop:
		for _, i := range s.StaticConfig.Interface {
			if i.Ipv4Autoconfig != nil {
				continue
			}
			for _, a := range i.Address {
				ipNet, err := addressOrPrefix(a)
				if err != nil {
					l.Warningf("failed to parse %q as IPNet", a)
					continue
				}
				if ipNet.IP.To4() != nil {
					selectedAddr = ipNet.IP.To4()
					break ifLoop
				}
			}
		}
		s.Status.Set(&node.NetStatus{
			ExternalAddress: selectedAddr,
		})
	}

	supervisor.Signal(ctx, supervisor.SignalHealthy)
	success = true
	supervisor.Signal(ctx, supervisor.SignalDone)
	return nil
}

func (s *Service) runDHCPv4(ctx context.Context, lnk netlink.Link) error {
	c, err := dhcp4c.NewClient(netlinkLinkToNetInterface(lnk))
	if err != nil {
		return fmt.Errorf("failed creating DHCPv4 client: %w", err)
	}
	c.RequestedOptions = []dhcpv4.OptionCode{dhcpv4.OptionRouter, dhcpv4.OptionDomainNameServer, dhcpv4.OptionClasslessStaticRoute}
	c.LeaseCallback = dhcpcb.Compose(dhcpcb.ManageIP(lnk), arpAnnounceCB(lnk), dhcpcb.ManageRoutes(lnk), s.statusCallback(ctx))
	return supervisor.Run(ctx, "dhcp-"+lnk.Attrs().Name, c.Run)
}

// getSortedIfaces returns a list of all interfaces to be configured in
// an order which is valid to configure them in, ie. parent interfaces get
// configured before child interfaces. It also validates that all interfaces
// referenced do in fact exist in the configuration.
func getSortedIfaces(s *Service) ([]*netpb.Interface, error) {
	var depGraph toposort.Graph[string]
	ifMap := make(map[string]*netpb.Interface)
	for _, iface := range s.StaticConfig.Interface {
		if err := isValidDevName(iface.Name); err != nil {
			return nil, fmt.Errorf("invalid interface name %q: %w", iface.Name, err)
		}
		ifMap[iface.Name] = iface
		depGraph.AddNode(iface.Name)
		switch it := iface.Type.(type) {
		case *netpb.Interface_Bond:
			for _, depIf := range it.Bond.MemberInterface {
				// Bond interfaces are set up with no children, their children
				// are then added when they are configured. Thus this needs a
				// reverse dependency.
				depGraph.AddEdge(depIf, iface.Name)
			}
		case *netpb.Interface_Vlan:
			depGraph.AddEdge(iface.Name, it.Vlan.Parent)
		}
	}
	badRefs := depGraph.ImplicitNodeReferences()
	if len(badRefs) > 0 {
		var errMsgs []string
		for n, refs := range badRefs {
			var strRefs []string
			for ref := range refs {
				strRefs = append(strRefs, ref)
			}
			errMsgs = append(errMsgs, fmt.Sprintf("reference to undefined interface %q from interfaces %s", n, strings.Join(strRefs, ", ")))
		}
		return nil, errors.New(strings.Join(errMsgs, "; "))
	}
	interfaceOrder, err := depGraph.DetTopologicalOrder(strings.Compare)
	if err != nil {
		return nil, fmt.Errorf("unable to calculate interface setup order: %w", err)
	}
	var sortedInterfaces []*netpb.Interface
	for _, ifname := range interfaceOrder {
		sortedInterfaces = append(sortedInterfaces, ifMap[ifname])
	}
	return sortedInterfaces, nil
}

type deviceIfData struct {
	dev    *netlink.Device
	driver string
}

func listHostDeviceIfaces() ([]deviceIfData, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("failed to list network links: %w", err)
	}

	var hostDevices []deviceIfData

	for _, link := range links {
		d, ok := link.(*netlink.Device)
		if !ok {
			continue
		}
		var driver string
		driverPath, err := os.Readlink("/sys/class/net/" + d.Name + "/device/driver")
		if err == nil {
			driver = filepath.Base(driverPath)
		}
		hostDevices = append(hostDevices, deviceIfData{
			dev:    d,
			driver: driver,
		})
	}
	return hostDevices, nil
}

func deviceIfaceFromSpec(it *netpb.Interface_Device, hostDevices []deviceIfData, l logging.Leveled) (*netlink.Device, error) {
	var matchedDevices []*netlink.Device
	var err error
	var parsedHWAddr net.HardwareAddr
	if it.Device.HardwareAddress != "" {
		parsedHWAddr, err = net.ParseMAC(it.Device.HardwareAddress)
		if err != nil {
			return nil, fmt.Errorf("unable to parse hardware address %q: %w", it.Device.HardwareAddress, err)
		}
	}

	// This is O(N^2), but is bounded by the amount of physical NICs in the
	// system. At this point we can reasonably assume N < 100.
	for _, d := range hostDevices {
		if len(parsedHWAddr) != 0 {
			// If device has a permanent hardware address, it must match,
			// otherwise the standard hardware address must match
			if len(d.dev.PermHWAddr) > 0 {
				if !bytes.Equal(d.dev.PermHWAddr, parsedHWAddr) {
					l.V(1).Infof("mismatched perm hw addr %q: %s %s\n", d.dev.Name, d.dev.PermHWAddr, parsedHWAddr)
					continue
				}
			} else if !bytes.Equal(d.dev.HardwareAddr, parsedHWAddr) {
				l.V(1).Infof("mismatched fallback hw addr %q: %s %s\n", d.dev.Name, d.dev.HardwareAddr, parsedHWAddr)
				continue
			}
		}
		if it.Device.Driver != "" {
			if it.Device.Driver != d.driver {
				l.V(1).Infof("mismatched driver %q: %s %s\n", d.dev.Name, it.Device.Driver, d.driver)
				continue
			}
		}
		matchedDevices = append(matchedDevices, d.dev)
	}
	if len(matchedDevices) == 0 {
		var available []string
		for _, d := range hostDevices {
			hwAddr := d.dev.HardwareAddr
			if len(d.dev.PermHWAddr) > 0 {
				hwAddr = d.dev.PermHWAddr
			}
			available = append(available, fmt.Sprintf("(%v,%v)", d.driver, hwAddr))
		}
		if len(available) == 0 {
			available = append(available, "none")
		}
		return nil, fmt.Errorf("no host devices match (driver=%v,hwaddr=%v), have %s", it.Device.Driver, it.Device.HardwareAddress, strings.Join(available, ", "))
	}
	if len(matchedDevices) <= int(it.Device.Index) || it.Device.Index < 0 {
		return nil, fmt.Errorf("there are %d matching host devices but requested device index is %d", len(matchedDevices), it.Device.Index)
	}
	// Return new Device stuct with empty LinkAttrs other than Index, which is
	// used to identify the device. Add/Modify operations will otherwise try to
	// set attributes returned by the kernel, some of which cause an ENOTSUPP
	// error.
	dev := &netlink.Device{
		LinkAttrs: netlink.NewLinkAttrs(),
	}
	dev.Index = matchedDevices[it.Device.Index].Index
	return dev, nil
}

var lacpRateMap = map[netpb.Bond_LACP_Rate]netlink.BondLacpRate{
	netpb.Bond_LACP_RATE_SLOW: netlink.BOND_LACP_RATE_SLOW,
	netpb.Bond_LACP_RATE_FAST: netlink.BOND_LACP_RATE_FAST,
}

var lacpAdSelectMap = map[netpb.Bond_LACP_SelectionLogic]netlink.BondAdSelect{
	netpb.Bond_LACP_SELECTION_LOGIC_STABLE:    netlink.BOND_AD_SELECT_STABLE,
	netpb.Bond_LACP_SELECTION_LOGIC_BANDWIDTH: netlink.BOND_AD_SELECT_BANDWIDTH,
	netpb.Bond_LACP_SELECTION_LOGIC_COUNT:     netlink.BOND_AD_SELECT_COUNT,
}

var xmitHashPolicyMap = map[netpb.Bond_TransmitHashPolicy]netlink.BondXmitHashPolicy{
	netpb.Bond_TRANSMIT_HASH_POLICY_LAYER2:         netlink.BOND_XMIT_HASH_POLICY_LAYER2,
	netpb.Bond_TRANSMIT_HASH_POLICY_LAYER2_3:       netlink.BOND_XMIT_HASH_POLICY_LAYER2_3,
	netpb.Bond_TRANSMIT_HASH_POLICY_LAYER3_4:       netlink.BOND_XMIT_HASH_POLICY_LAYER3_4,
	netpb.Bond_TRANSMIT_HASH_POLICY_ENCAP_LAYER2_3: netlink.BOND_XMIT_HASH_POLICY_ENCAP2_3,
	netpb.Bond_TRANSMIT_HASH_POLICY_ENCAP_LAYER3_4: netlink.BOND_XMIT_HASH_POLICY_ENCAP3_4,
	netpb.Bond_TRANSMIT_HASH_POLICY_VLAN_SRCMAC:    netlink.BOND_XMIT_HASH_POLICY_VLAN_SRCMAC,
}

func bondIfaceFromSpec(it *netpb.Interface_Bond, i *netpb.Interface) (*netlink.Bond, error) {
	newBond := netlink.NewLinkBond(netlink.NewLinkAttrs())
	newBond.MinLinks = int(it.Bond.MinLinks)
	newBond.XmitHashPolicy = xmitHashPolicyMap[it.Bond.TransmitHashPolicy]
	switch bt := it.Bond.Mode.(type) {
	case *netpb.Bond_Lacp:
		newBond.Mode = netlink.BOND_MODE_802_3AD
		newBond.LacpRate = lacpRateMap[bt.Lacp.Rate]
		newBond.AdSelect = lacpAdSelectMap[bt.Lacp.SelectionLogic]
		if bt.Lacp.ActorSystemMac != "" {
			mac, err := net.ParseMAC(bt.Lacp.ActorSystemMac)
			if err != nil {
				return nil, fmt.Errorf("malformed LACP actor_system_mac for bond %q: %w", i.Name, err)
			}
			newBond.AdActorSystem = mac
		}
		if bt.Lacp.ActorSystemPriority != 0 {
			newBond.AdActorSysPrio = int(bt.Lacp.ActorSystemPriority)
		}
		if bt.Lacp.UserPortKey != 0 {
			newBond.AdUserPortKey = int(bt.Lacp.UserPortKey)
		}
	case *netpb.Bond_ActiveBackup_:
		newBond.Mode = netlink.BOND_MODE_ACTIVE_BACKUP
	default:
		return nil, fmt.Errorf("unknown bond type %T", bt)
	}
	return newBond, nil
}

func addAddrFromSpec(a string, link netlink.Link) error {
	var addr netlink.Addr
	ipNet, err := addressOrPrefix(a)
	if err != nil {
		// Error already contains original string and enough wrapping
		// is already done, so pass through directly.
		return err
	}
	addr.IPNet = ipNet
	if ones, size := addr.Mask.Size(); ones == size {
		// If this is a single host IP, do not add a prefix route as it is
		// not routable without a separate inteface route.
		addr.Flags |= unix.IFA_F_NOPREFIXROUTE
	}
	addr.Flags |= unix.IFA_F_PERMANENT
	// Kernel will add the on-link prefix for us in the routing
	// table if required.
	if err := netlink.AddrAdd(link, &addr); err != nil {
		return fmt.Errorf("failed to add to kernel interface: %w", err)
	}
	return nil
}

func routeFromSpec(r *netpb.Interface_Route, link netlink.Link) error {
	var route netlink.Route
	dst, err := addressOrPrefix(r.Destination)
	if err != nil {
		return fmt.Errorf("destination invalid: %w", err)
	}
	if !dst.IP.Mask(dst.Mask).Equal(dst.IP) {
		return fmt.Errorf("destination %v has bits in the mask set", r.Destination)
	}
	route.Dst = dst
	route.Protocol = unix.RTPROT_STATIC
	route.LinkIndex = link.Attrs().Index
	route.Priority = int(r.Metric)
	if r.SourceIp != "" {
		srcIP := net.ParseIP(r.SourceIp)
		if srcIP == nil {
			return fmt.Errorf("failed parsing %q as IP", r.SourceIp)
		}
		route.Src = srcIP
	}
	if r.GatewayIp != "" {
		gwIP := net.ParseIP(r.GatewayIp)
		if gwIP == nil {
			return fmt.Errorf("failed parsing %q as IP", r.GatewayIp)
		}
		route.Gw = gwIP

		// These are all interface routes, if a gateway is present
		// it is always treated as on-link.
		route.Flags |= int(netlink.FLAG_ONLINK)
	}
	if err := netlink.RouteAdd(&route); err != nil {
		return fmt.Errorf("failed creating kernel route %q: %w", r.Destination, err)
	}
	return nil
}

func addressOrPrefix(s string) (*net.IPNet, error) {
	if strings.ContainsRune(s, '/') {
		ip, prefix, err := net.ParseCIDR(s)
		if err != nil {
			// err already contains original string, no need to wrap
			return nil, err
		}
		return &net.IPNet{IP: ip, Mask: prefix.Mask}, nil
	} else {
		ip := net.ParseIP(s)
		if ip == nil {
			return nil, fmt.Errorf("invalid IP: %v", ip)
		}
		var mask net.IPMask
		if ip.To4() == nil {
			mask = net.CIDRMask(128, 128) // IPv6 /128
		} else {
			mask = net.CIDRMask(32, 32) // IPv4 /32
		}
		return &net.IPNet{IP: ip, Mask: mask}, nil
	}
}

func netlinkLinkToNetInterface(lnk netlink.Link) *net.Interface {
	attrs := lnk.Attrs()
	return &net.Interface{
		Index:        attrs.Index,
		MTU:          attrs.MTU,
		Name:         attrs.Name,
		HardwareAddr: attrs.HardwareAddr,
		Flags:        attrs.Flags,
	}
}

var validDevNameRegexp = regexp.MustCompile("^[^/:[:space:]]{1,15}$")

func isValidDevName(name string) error {
	if name == "." || name == ".." {
		return errors.New("cannot be \".\" or \"..\"")
	}
	if strings.ContainsRune(name, '%') {
		return errors.New("contains \"%\" sign which dynamically allocate names, this is disallowed")
	}
	if !validDevNameRegexp.MatchString(name) {
		return errors.New("too short, too long or contains forward slashes, colons or spaces")
	}
	return nil
}
