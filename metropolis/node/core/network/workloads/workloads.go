// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package workloads

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"source.monogon.dev/metropolis/node/allocs"
	"source.monogon.dev/metropolis/node/core/network/ipam"
	wlapi "source.monogon.dev/metropolis/node/core/network/workloads/spec"
	"source.monogon.dev/osbase/event"
	"source.monogon.dev/osbase/supervisor"
)

var (
	firstHopV4 = net.IPv4(169, 254, 77, 1)
	firstHopV6 = net.ParseIP("fe80::1")
	// TODO: Replace prefix with Monogon OUI once we have it, right now
	// it's just a random locally-administered MAC.
	firstHopMAC = net.HardwareAddr{0x02, 0x9c, 0x52, 0xfe, 0x6d, 0x0a}
)

type Service struct {
	mux          sync.Mutex
	workloadNets []netip.Prefix
	attachments  map[netip.Addr]string
	// workloadToIntf maps workload name to short interface name.
	workloadToIntf map[string]string
	// intfUsed is the set of allocated short interface names.
	intfUsed map[string]struct{}

	k8sNodePrefix event.Value[*ipam.Prefixes]
}

func New(k8sNodePrefix event.Value[*ipam.Prefixes]) *Service {
	return &Service{
		workloadNets:   []netip.Prefix{},
		attachments:    make(map[netip.Addr]string),
		workloadToIntf: make(map[string]string),
		intfUsed:       make(map[string]struct{}),
		k8sNodePrefix:  k8sNodePrefix,
	}
}

func (s *Service) allocateIPs(workloadId string) ([]net.IP, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	// This is a really simple allocator as it won't stay for long. It just
	// walks the entire prefix and finds the first free IP. The size of the
	// map is bound to 2x256 (max pods per node) for its life so this is fine.
	var addrs []netip.Addr
	for _, wlNet := range s.workloadNets {
		candidateAddr := wlNet.Addr()
		// The second address is reserved by clusternet for the host loopback,
		// this will go away with the clusternet refactor.
		reservedForHost := wlNet.Addr().Next()
		for s.attachments[candidateAddr] != "" || candidateAddr == reservedForHost {
			candidateAddr = candidateAddr.Next()
		}
		// Allocator ran off the prefix
		if !wlNet.Contains(candidateAddr) {
			return nil, fmt.Errorf("no free IP addresses in prefix %v", wlNet)
		}
		addrs = append(addrs, candidateAddr)
	}
	var addrsOut []net.IP
	for _, addr := range addrs {
		s.attachments[addr] = workloadId
		addrsOut = append(addrsOut, net.IP(addr.AsSlice()))
	}
	return addrsOut, nil
}

func (s *Service) deallocateIPs(workloadId string) {
	s.mux.Lock()
	defer s.mux.Unlock()
	for ip, wlId := range s.attachments {
		if wlId == workloadId {
			delete(s.attachments, ip)
		}
	}
}

// allocateIntfName allocates a short interface name for the workload. This is
// needed because interface names are limited to 15 characters.
func (s *Service) allocateIntfName(workloadId string) (string, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if _, ok := s.workloadToIntf[workloadId]; ok {
		return "", fmt.Errorf("workload %q already has an interface", workloadId)
	}
	intfPrefix := "wk" + workloadId[:8]
	intf := intfPrefix
	for i := 0; ; i++ {
		if _, ok := s.intfUsed[intf]; !ok {
			break
		}
		if i > 0xffff {
			return "", fmt.Errorf("too many interface name collisions for workload %q", workloadId)
		}
		intf = fmt.Sprintf("%s-%04x", intfPrefix, i)
	}
	s.workloadToIntf[workloadId] = intf
	s.intfUsed[intf] = struct{}{}
	return intf, nil
}

func (s *Service) getIntfName(workloadId string) (string, bool) {
	s.mux.Lock()
	defer s.mux.Unlock()
	intf, ok := s.workloadToIntf[workloadId]
	return intf, ok
}

func (s *Service) deallocateIntfName(workloadId string) {
	s.mux.Lock()
	defer s.mux.Unlock()
	intf, ok := s.workloadToIntf[workloadId]
	if !ok {
		return
	}
	delete(s.workloadToIntf, workloadId)
	delete(s.intfUsed, intf)
}

func (s *Service) Run(ctx context.Context) error {
	l := supervisor.Logger(ctx)

	srv := grpc.NewServer()
	wlapi.RegisterWorkloadNetworkingServer(srv, s)
	os.Remove("/ephemeral/workloadnet.sock")
	lis, err := net.ListenUnix("unix", &net.UnixAddr{Net: "unix", Name: "/ephemeral/workloadnet.sock"})
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}
	supervisor.Run(ctx, "api", supervisor.GRPCServer(srv, lis, true))
	w := s.k8sNodePrefix.Watch()
	defer w.Close()

	lo, err := netlink.LinkByIndex(1)
	if err != nil {
		panic(err)
	}
	if err := netlink.AddrAdd(lo, &netlink.Addr{
		IPNet: &net.IPNet{IP: firstHopV4, Mask: net.CIDRMask(32, 32)},
		Label: "Router",
		Scope: unix.RT_SCOPE_LINK,
	}); err != nil && !errors.Is(err, unix.EEXIST) {
		l.Errorf("Unable to add router IP: %v", err)
	}

	supervisor.Signal(ctx, supervisor.SignalHealthy)
	// It's undefined what happens when the workloadNets actually change right
	// now with K8s IPAM. So just assign new workloads to the new prefixes for
	// now. With the Monogon IPAM implementation this will have defined
	// behavior.
	for {
		prefixes, err := w.Get(ctx)
		if err != nil {
			return err
		}
		if prefixes != nil {
			s.mux.Lock()
			s.workloadNets = *prefixes
			s.mux.Unlock()
		}
	}
}

func (s *Service) Attach(ctx context.Context, req *wlapi.AttachRequest) (*wlapi.AttachResponse, error) {
	intf, err := s.allocateIntfName(req.WorkloadId)
	if err != nil {
		return nil, status.Errorf(codes.AlreadyExists, "cannot add interface: %v", err)
	}
	workloadAddrs, err := s.allocateIPs(req.WorkloadId)
	if err != nil {
		return nil, status.Errorf(codes.ResourceExhausted, "cannot allocate IPs: %v", err)
	}

	linkAttrs := netlink.NewLinkAttrs()
	linkAttrs.Group = allocs.LinkGroupK8sPod
	linkAttrs.Name = intf
	linkAttrs.HardwareAddr = firstHopMAC

	netns, err := netns.GetFromPath(req.GetNetns().NetnsPath)
	if err != nil {
		return nil, fmt.Errorf("cannot open network namespace: %w", err)
	}
	defer netns.Close()

	nsHandle, err := netlink.NewHandleAt(netns, unix.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("unable to get ns handle: %w", err)
	}
	defer nsHandle.Close()

	hostIf := netlink.Veth{LinkAttrs: linkAttrs, PeerName: req.GetNetns().IfName, PeerNamespace: netlink.NsFd(netns)}
	if err := netlink.LinkAdd(&hostIf); err != nil {
		return nil, fmt.Errorf("unable to create veth pair: %w", err)
	}
	// Linux is currently unable to assign aliases on interface creation.
	if err := netlink.LinkSetAlias(&hostIf, "wk"+req.WorkloadId); err != nil {
		return nil, fmt.Errorf("failed to assign alias: %w", err)
	}
	if err := netlink.LinkSetUp(&hostIf); err != nil {
		return nil, fmt.Errorf("failed to set host up: %w", err)
	}

	// Loopback is always at index 1 by convention
	loIf, err := nsHandle.LinkByIndex(1)
	if err != nil {
		return nil, fmt.Errorf("unable to get loopback interface in namespace: %w", err)
	}
	if err := nsHandle.LinkSetUp(loIf); err != nil {
		return nil, fmt.Errorf("failed to set loopback up: %w", err)
	}

	workloadIf, err := nsHandle.LinkByName(req.GetNetns().IfName)
	if err != nil {
		return nil, fmt.Errorf("unable to get just-created peer interface in namespace: %w", err)
	}

	if err := nsHandle.LinkSetUp(workloadIf); err != nil {
		return nil, fmt.Errorf("failed to set peer up: %w", err)
	}
	var outAddrs [][]byte
	for _, workloadIP := range workloadAddrs {
		outAddrs = append(outAddrs, workloadIP)

		defaultMask := net.CIDRMask(0, 32) // /0
		zeroIP := net.IPv4zero
		hostMask := net.CIDRMask(32, 32) // /32
		firstHop := firstHopV4
		if workloadIP.To4() == nil {
			defaultMask = net.CIDRMask(0, 128) // /0
			zeroIP = net.IPv6zero
			hostMask = net.CIDRMask(128, 128) // /128
			firstHop = firstHopV6
		}

		if err := netlink.RouteAdd(&netlink.Route{
			Dst:       &net.IPNet{IP: workloadIP, Mask: hostMask},
			LinkIndex: hostIf.Index,
			Scope:     netlink.SCOPE_UNIVERSE,
			Protocol:  unix.RTPROT_STATIC,
		}); err != nil {
			return nil, fmt.Errorf("failed to add host to workload route: %w", err)
		}

		if err := nsHandle.AddrAdd(workloadIf, &netlink.Addr{
			IPNet: &net.IPNet{IP: workloadIP, Mask: hostMask},
		}); err != nil {
			return nil, fmt.Errorf("failed to add address: %w", err)
		}
		// Use dedicated on-link route instead of RTNH_F_ONLINK which gVisor
		// doesn't understand.
		if err := nsHandle.RouteAdd(&netlink.Route{
			Dst:       &net.IPNet{IP: firstHop, Mask: hostMask},
			Scope:     netlink.SCOPE_LINK,
			Protocol:  unix.RTPROT_STATIC,
			LinkIndex: workloadIf.Attrs().Index,
		}); err != nil {
			return nil, fmt.Errorf("failed to add peer route: %w", err)
		}
		if err := nsHandle.RouteAdd(&netlink.Route{
			Dst:       &net.IPNet{IP: zeroIP, Mask: defaultMask},
			Gw:        firstHop,
			Scope:     netlink.SCOPE_UNIVERSE,
			Protocol:  unix.RTPROT_STATIC,
			LinkIndex: workloadIf.Attrs().Index,
			Src:       workloadIP,
		}); err != nil {
			return nil, fmt.Errorf("failed to add default route: %w", err)
		}
	}
	return &wlapi.AttachResponse{Ip: outAddrs}, nil
}

func (s *Service) Detach(ctx context.Context, req *wlapi.DetachRequest) (*wlapi.DetachResponse, error) {
	defer s.deallocateIntfName(req.WorkloadId)
	defer s.deallocateIPs(req.WorkloadId)
	intf, ok := s.getIntfName(req.WorkloadId)
	if !ok {
		return &wlapi.DetachResponse{}, nil
	}

	hostIf, err := netlink.LinkByName(intf)
	if errors.As(err, &netlink.LinkNotFoundError{}) {
		// CNI requires that DEL calls return success if the interface in
		// question does not exist.
		return &wlapi.DetachResponse{}, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "error getting interface for deletion: %v", err)
	}
	if hostIf.Attrs().Group != allocs.LinkGroupK8sPod {
		return nil, status.Errorf(codes.InvalidArgument, "refusing to delete interface not belonging to workload, has group %d", hostIf.Attrs().Group)
	}
	// Routes and addresses do not need to be cleaned up as Linux already takes
	// care of that when the link is deleted.
	if err := netlink.LinkDel(hostIf); err != nil {
		return nil, status.Errorf(codes.Unavailable, "unable to delete veth interface: %v", err)
	}
	return &wlapi.DetachResponse{}, nil
}

func (s *Service) Status(ctx context.Context, req *wlapi.StatusRequest) (*wlapi.StatusResponse, error) {
	s.mux.Lock()
	defer s.mux.Unlock()

	if len(s.workloadNets) == 0 {
		return nil, status.Errorf(codes.Unavailable, "no prefixes available")
	}

	return &wlapi.StatusResponse{}, nil
}
