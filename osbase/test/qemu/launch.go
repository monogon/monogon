// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Package qemu implements test harnesses for running qemu VMs from tests.
package qemu

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"golang.org/x/sys/unix"

	"source.monogon.dev/osbase/freeport"
)

var (
	// HostInterfaceMAC is the MAC address the host SLIRP network interface has if it
	// is not disabled (see DisableHostNetworkInterface in MicroVMOptions)
	HostInterfaceMAC = net.HardwareAddr{0x02, 0x72, 0x82, 0xbf, 0xc3, 0x56}
)

type QemuValue map[string][]string

// ToOption encodes structured data into a QEMU option. Example: "test", {"key1":
// {"val1"}, "key2": {"val2", "val3"}} returns "test,key1=val1,key2=val2,key2=val3"
func (value QemuValue) ToOption(name string) string {
	var optionValues []string
	if name != "" {
		optionValues = append(optionValues, name)
	}
	for name, values := range value {
		if len(values) == 0 {
			optionValues = append(optionValues, name)
		}
		for _, val := range values {
			optionValues = append(optionValues, fmt.Sprintf("%v=%v", name, val))
		}
	}
	return strings.Join(optionValues, ",")
}

// PrettyPrintQemuArgs prints the given QEMU arguments to stderr.
func PrettyPrintQemuArgs(name string, args []string) {
	var argsFmt string
	for _, arg := range args {
		argsFmt += arg
		if !strings.HasPrefix(arg, "-") {
			argsFmt += "\n  "
		} else {
			argsFmt += " "
		}
	}
	fmt.Fprintf(os.Stderr, "Running %s:\n  %s\n", name, argsFmt)
}

// PortMap represents where VM ports are mapped to on the host. It maps from the VM
// port number to the host port number.
type PortMap map[uint16]uint16

// ToQemuForwards generates QEMU hostfwd values (https://qemu.weilnetz.de/doc/qemu-
// doc.html#:~:text=hostfwd=) for all mapped ports.
func (p PortMap) ToQemuForwards() []string {
	var hostfwdOptions []string
	for vmPort, hostPort := range p {
		hostfwdOptions = append(hostfwdOptions, fmt.Sprintf("tcp::%d-:%d", hostPort, vmPort))
	}
	return hostfwdOptions
}

// IdentityPortMap returns a port map where each given port is mapped onto itself
// on the host. This is mainly useful for development against Metropolis. The dbg
// command requires this mapping.
func IdentityPortMap(ports []uint16) PortMap {
	portMap := make(PortMap)
	for _, port := range ports {
		portMap[port] = port
	}
	return portMap
}

// ConflictFreePortMap returns a port map where each given port is mapped onto a
// random free port on the host. This is intended for automated testing where
// multiple instances of Metropolis nodes might be running. Please call this
// function for each Launch command separately and as close to it as possible since
// it cannot guarantee that the ports will remain free.
func ConflictFreePortMap(ports []uint16) (PortMap, error) {
	portMap := make(PortMap)
	for _, port := range ports {
		mappedPort, listenCloser, err := freeport.AllocateTCPPort()
		if err != nil {
			return portMap, fmt.Errorf("failed to get free host port: %w", err)
		}
		// Defer closing of the listening port until the function is done and all ports are
		// allocated
		defer listenCloser.Close()
		portMap[port] = mappedPort
	}
	return portMap, nil
}

// GuestServiceMap maps an IP/port combination inside the virtual guest network
// to a TCPAddr reachable by the host. If the guest connects to the virtual
// address/port, this connection gets forwarded to the host.
type GuestServiceMap map[*net.TCPAddr]net.TCPAddr

// ToQemuForwards generates QEMU guestfwd values (https://qemu.weilnetz.de/doc/qemu-
// doc.html#:~:text=guestfwd=) for all mapped addresses.
func (p GuestServiceMap) ToQemuForwards() []string {
	var guestfwdOptions []string
	for guestAddr, hostAddr := range p {
		guestfwdOptions = append(guestfwdOptions, fmt.Sprintf("tcp:%s-tcp:%s", guestAddr.String(), hostAddr.String()))
	}
	return guestfwdOptions
}

// NewSocketPair creates a new socket pair. By connecting both ends to different
// instances you can connect them with a virtual "network cable". The ends can be
// passed into the ConnectToSocket option.
func NewSocketPair() (*os.File, *os.File, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to call socketpair: %w", err)
	}

	fd1 := os.NewFile(uintptr(fds[0]), "network0")
	fd2 := os.NewFile(uintptr(fds[1]), "network1")
	return fd1, fd2, nil
}

var (
	// These are filled by bazel at linking time with the canonical path of
	// their corresponding file. Inside the init function we resolve it
	// with the rules_go runfiles package to the real path.
	xQEMUPath string
)

func init() {
	var err error
	for _, path := range []*string{
		&xQEMUPath,
	} {
		*path, err = runfiles.Rlocation(*path)
		if err != nil {
			panic(err)
		}
	}
}

// MicroVMOptions contains all options to start a MicroVM
type MicroVMOptions struct {
	// Name is a human-readable identifier to be used in debug output.
	Name string

	// Path to the ELF kernel binary
	KernelPath string

	// Path to the Initramfs
	InitramfsPath string

	// Cmdline contains additional kernel commandline options
	Cmdline string

	// SerialPort is a File(descriptor) over which you can communicate with the serial
	// port of the machine It can be set to an existing file descriptor (like
	// os.Stdout/os.Stderr) or you can use NewSocketPair() to get one end to talk to
	// from Go.
	SerialPort io.Writer

	// ExtraChardevs can be used similar to SerialPort, but can contain an arbitrary
	// number of additional serial ports
	ExtraChardevs []*os.File

	// ExtraNetworkInterfaces can contain an arbitrary number of file descriptors which
	// are mapped into the VM as virtio network interfaces. The first interface is
	// always a SLIRP-backed interface for communicating with the host.
	ExtraNetworkInterfaces []*os.File

	// PortMap contains ports that are mapped to the host through the built-in SLIRP
	// network interface.
	PortMap PortMap

	// GuestServiceMap contains TCP services made available in the guest virtual
	// network which are running on the host.
	GuestServiceMap GuestServiceMap

	// DisableHostNetworkInterface disables the SLIRP-backed host network interface
	// that is normally the first network interface. If this is set PortMap is ignored.
	// Mostly useful for speeding up QEMU's startup time for tests.
	DisableHostNetworkInterface bool

	// PcapDump can be used to dump all network traffic to a pcap file.
	// If unset, no dump is created.
	PcapDump string
}

// RunMicroVM launches a tiny VM mostly intended for testing. Very quick to boot
// (<40ms).
func RunMicroVM(ctx context.Context, opts *MicroVMOptions) error {
	// Generate options for all the file descriptors we'll be passing as virtio "serial
	// ports"
	var extraArgs []string
	for idx := range opts.ExtraChardevs {
		idxStr := strconv.Itoa(idx)
		id := "extra" + idxStr
		// That this works is pretty much a hack, but upstream QEMU doesn't have a
		// bidirectional chardev backend not based around files/sockets on the disk which
		// are a giant pain to work with. We're using QEMU's fdset functionality to make
		// FDs available as pseudo-files and then "ab"using the pipe backend's fallback
		// functionality to get a single bidirectional chardev backend backed by a passed-
		// down RDWR fd. Ref https://lists.gnu.org/archive/html/qemu-devel/2015-
		// 12/msg01256.html
		addFdConf := QemuValue{
			"set": {idxStr},
			"fd":  {strconv.Itoa(idx + 3)},
		}
		chardevConf := QemuValue{
			"id":   {id},
			"path": {"/dev/fdset/" + idxStr},
		}
		deviceConf := QemuValue{
			"chardev": {id},
		}
		extraArgs = append(extraArgs, "-add-fd", addFdConf.ToOption(""),
			"-chardev", chardevConf.ToOption("pipe"), "-device", deviceConf.ToOption("virtserialport"))
	}

	for idx := range opts.ExtraNetworkInterfaces {
		id := fmt.Sprintf("net%v", idx)
		netdevConf := QemuValue{
			"id": {id},
			"fd": {strconv.Itoa(idx + 3 + len(opts.ExtraChardevs))},
		}
		extraArgs = append(extraArgs, "-netdev", netdevConf.ToOption("socket"), "-device", "virtio-net-device,netdev="+id)
	}

	// This sets up a minimum viable environment for our Linux kernel. It clears all
	// standard QEMU configuration and sets up a MicroVM machine
	// (https://github.com/qemu/qemu/blob/master/docs/microvm.rst) with all legacy
	// emulation turned off. This means the only "hardware" the Linux kernel inside can
	// communicate with is a single virtio-mmio region. Over that MMIO interface we run
	// a paravirtualized RNG (since the kernel in there has nothing to gather that from
	// and it delays booting), a single paravirtualized console and an arbitrary number
	// of extra serial ports for talking to various things that might run inside. The
	// kernel, initramfs and command line are mapped into VM memory at boot time and
	// not loaded from any sort of disk. Booting and shutting off one of these VMs
	// takes <100ms.
	baseArgs := []string{
		"-nodefaults", "-no-user-config", "-nographic", "-no-reboot",
		"-accel", "kvm", "-cpu", "host",
		"-m", "1G",
		// Needed because QEMU does not boot without specifying the qboot bios
		// even tho the documentation clearly states that this is the default.
		"-bios", filepath.Join(filepath.Dir(xQEMUPath), "../share/qemu/qboot.rom"),
		"-M", "microvm,x-option-roms=off,pic=off,pit=off,rtc=off,isa-serial=off",
		"-kernel", opts.KernelPath,
		// We force using a triple-fault reboot strategy since otherwise the kernel first
		// tries others (like ACPI) which are not available in this very restricted
		// environment. Similarly we need to override the boot console since there's
		// nothing on the ISA bus that the kernel could talk to. We also force quiet for
		// performance reasons.
		"-append", "reboot=t console=hvc0 quiet " + opts.Cmdline,
		"-initrd", opts.InitramfsPath,
		"-device", "virtio-rng-device,max-bytes=1024,period=1000",
		"-device", "virtio-serial-device,max_ports=16",
		"-chardev", "stdio,id=con0", "-device", "virtconsole,chardev=con0",
	}

	if !opts.DisableHostNetworkInterface {
		qemuNetType := "user"
		qemuNetConfig := QemuValue{
			"id":        {"usernet0"},
			"net":       {"10.42.0.0/24"},
			"dhcpstart": {"10.42.0.10"},
		}
		if opts.PortMap != nil {
			qemuNetConfig["hostfwd"] = opts.PortMap.ToQemuForwards()
		}
		if opts.GuestServiceMap != nil {
			qemuNetConfig["guestfwd"] = opts.GuestServiceMap.ToQemuForwards()
		}

		baseArgs = append(baseArgs, "-netdev", qemuNetConfig.ToOption(qemuNetType),
			"-device", "virtio-net-device,netdev=usernet0,mac="+HostInterfaceMAC.String())
	}

	if !opts.DisableHostNetworkInterface && opts.PcapDump != "" {
		qemuNetDump := QemuValue{
			"id":     {"usernet0"},
			"netdev": {"usernet0"},
			"file":   {opts.PcapDump},
		}
		extraArgs = append(extraArgs, "-object", qemuNetDump.ToOption("filter-dump"))
	}

	var stdErrBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, xQEMUPath, append(baseArgs, extraArgs...)...)
	cmd.Stdout = opts.SerialPort
	cmd.Stderr = &stdErrBuf

	cmd.ExtraFiles = append(cmd.ExtraFiles, opts.ExtraChardevs...)
	cmd.ExtraFiles = append(cmd.ExtraFiles, opts.ExtraNetworkInterfaces...)

	PrettyPrintQemuArgs(opts.Name, cmd.Args)

	err := cmd.Run()
	// If it's a context error, just quit. There's no way to tell a
	// killed-due-to-context vs killed-due-to-external-reason error returned by Run,
	// so we approximate by looking at the context's status.
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}

	var exerr *exec.ExitError
	if err != nil && errors.As(err, &exerr) {
		exerr.Stderr = stdErrBuf.Bytes()
		newErr := QEMUError(*exerr)
		return &newErr
	}
	return err
}

// QEMUError is a special type of ExitError used when QEMU fails. In addition to
// normal ExitError features it prints stderr for debugging.
type QEMUError exec.ExitError

func (e *QEMUError) Error() string {
	return fmt.Sprintf("%v: %v", e.String(), string(e.Stderr))
}
