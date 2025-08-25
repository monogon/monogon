// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// cluster builds on the launch package and implements launching Metropolis
// nodes and clusters in a virtualized environment using qemu. It's kept in a
// separate package as it depends on a Metropolis node image, which might not be
// required for some use of the launch library.
package launch

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cenkalti/backoff/v4"
	"go.uber.org/multierr"
	"golang.org/x/net/proxy"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"

	"source.monogon.dev/metropolis/node/allocs"
	ipb "source.monogon.dev/metropolis/node/core/curator/proto/api"
	apb "source.monogon.dev/metropolis/proto/api"
	cpb "source.monogon.dev/metropolis/proto/common"

	"source.monogon.dev/go/logging"
	metroctl "source.monogon.dev/metropolis/cli/metroctl/core"
	"source.monogon.dev/metropolis/installer/install"
	"source.monogon.dev/metropolis/node"
	"source.monogon.dev/metropolis/node/core/rpc"
	"source.monogon.dev/metropolis/node/core/rpc/resolver"
	"source.monogon.dev/osbase/blockdev"
	"source.monogon.dev/osbase/oci"
	"source.monogon.dev/osbase/oci/osimage"
	"source.monogon.dev/osbase/oci/registry"
	"source.monogon.dev/osbase/structfs"
	"source.monogon.dev/osbase/test/qemu"
)

const (
	// NodeNumberKey is the key of the node label used to carry a node's numerical
	// index in the test system.
	NodeNumberKey string = "test-node-number"
)

// NodeOptions contains all options that can be passed to Launch()
type NodeOptions struct {
	// Name is a human-readable identifier to be used in debug output.
	Name string

	// CPUs is the number of virtual CPUs of the VM.
	CPUs int

	// ThreadsPerCPU is the number of threads per CPU. This is multiplied by
	// CPUs to get the total number of threads.
	ThreadsPerCPU int

	// MemoryMiB is the RAM size in MiB of the VM.
	MemoryMiB int

	// DiskBytes contains the size of the root disk in bytes or zero if the
	// unmodified image size is used.
	DiskBytes uint64

	// Ports contains the port mapping where to expose the internal ports of the VM to
	// the host. See IdentityPortMap() and ConflictFreePortMap(). Ignored when
	// ConnectToSocket is set.
	Ports qemu.PortMap

	// If set to true, reboots are honored. Otherwise, all reboots exit the Launch()
	// command. Metropolis nodes generally restart on almost all errors, so unless you
	// want to test reboot behavior this should be false.
	AllowReboot bool

	// By default, the VM is connected to the Host via SLIRP. If ConnectToSocket is
	// set, it is instead connected to the given file descriptor/socket. If this is
	// set, all port maps from the Ports option are ignored. Intended for networking
	// this instance together with others for running more complex network
	// configurations.
	ConnectToSocket *os.File

	// When PcapDump is set, all traffic is dumped to a pcap file in the
	// runtime directory (e.g. "net0.pcap" for the first interface).
	PcapDump bool

	// SerialPort is an io.ReadWriter over which you can communicate with the serial
	// port of the machine. It can be set to an existing file descriptor (like
	// os.Stdout/os.Stderr) or any Go structure implementing this interface.
	SerialPort io.ReadWriter

	// NodeParameters is passed into the VM and subsequently used for bootstrapping or
	// registering into a cluster.
	NodeParameters *apb.NodeParameters

	// Mac is the node's MAC address.
	Mac *net.HardwareAddr

	// Runtime keeps the node's QEMU runtime state.
	Runtime *NodeRuntime

	// RunVNC starts a VNC socket for troubleshooting/testing console code. Note:
	// this will not work in tests, as those use a built-in qemu which does not
	// implement a VGA device.
	RunVNC bool
}

// NodeRuntime keeps the node's QEMU runtime options.
type NodeRuntime struct {
	// ld points at the node's launch directory storing data such as storage
	// images, firmware variables or the TPM state.
	ld string
	// sd points at the node's socket directory.
	sd string

	// ctxT is the context QEMU will execute in.
	ctxT context.Context
	// CtxC is the QEMU context's cancellation function.
	CtxC context.CancelFunc
}

// NodePorts is the list of ports a fully operational Metropolis node listens on
var NodePorts = []allocs.Port{
	allocs.PortConsensus,

	allocs.PortCuratorService,
	allocs.PortDebugService,

	allocs.PortKubernetesAPI,
	allocs.PortKubernetesAPIWrapped,
	allocs.PortCuratorService,
	allocs.PortDebugger,
	allocs.PortMetrics,
}

// setupRuntime creates the node's QEMU runtime directory, together with all
// files required to preserve its state, a level below the chosen path ld. The
// node's socket directory is similarily created a level below sd. It may
// return an I/O error.
func setupRuntime(ld, sd string, diskBytes uint64) (*NodeRuntime, error) {
	// Create a temporary directory to keep all the runtime files.
	stdp, err := os.MkdirTemp(ld, "node_state*")
	if err != nil {
		return nil, fmt.Errorf("failed to create the state directory: %w", err)
	}

	// Initialize the node's storage.
	ociImage, err := oci.AsImage(oci.ReadLayout(xNodeImagePath))
	if err != nil {
		return nil, fmt.Errorf("failed to read OS image: %w", err)
	}
	osImage, err := osimage.Read(ociImage)
	if err != nil {
		return nil, fmt.Errorf("failed to read OS image: %w", err)
	}

	abloader, err := structfs.OSPathBlob(xAbloaderPath)
	if err != nil {
		return nil, fmt.Errorf("cannot open abloader: %w", err)
	}

	di := filepath.Join(stdp, "image.img")
	logf("Cluster: generating node image: %s -> %s", xNodeImagePath, di)

	df, err := blockdev.CreateFile(di, 512, int64(diskBytes/512))
	if err != nil {
		return nil, fmt.Errorf("while opening image for writing: %w", err)
	}
	defer df.Close()

	installParams := &install.Params{
		PartitionSize: install.PartitionSizeInfo{
			ESP:    128,
			System: 1024,
			Data:   128,
		},
		OSImage:            osImage,
		UnverifiedPayloads: true,
		ABLoader:           abloader,
		Output:             df,
	}

	if _, err := install.Write(installParams); err != nil {
		return nil, fmt.Errorf("while creating node image: %w", err)
	}

	// Initialize the OVMF firmware variables file.
	dv := filepath.Join(stdp, filepath.Base(xOvmfVarsPath))
	if err := copyFile(xOvmfVarsPath, dv); err != nil {
		return nil, fmt.Errorf("while copying firmware variables: %w", err)
	}

	// Create the socket directory.
	sotdp, err := os.MkdirTemp(sd, "node_sock*")
	if err != nil {
		return nil, fmt.Errorf("failed to create the socket directory: %w", err)
	}

	return &NodeRuntime{
		ld: stdp,
		sd: sotdp,
	}, nil
}

// CuratorClient returns an authenticated owner connection to a Curator
// instance within Cluster c, or nil together with an error.
func (c *Cluster) CuratorClient() (*grpc.ClientConn, error) {
	if c.authClient == nil {
		authCreds := rpc.NewAuthenticatedCredentials(c.Owner, rpc.WantInsecure())
		r := resolver.New(c.ctxT, resolver.WithLogger(logging.NewFunctionBackend(func(severity logging.Severity, msg string) {
			logf("Cluster: client resolver: %s: %s", severity, msg)
		})))
		for _, n := range c.Nodes {
			r.AddEndpoint(resolver.NodeAtAddressWithDefaultPort(n.ManagementAddress))
		}
		authClient, err := grpc.NewClient(resolver.MetropolisControlAddress,
			grpc.WithTransportCredentials(authCreds),
			grpc.WithResolvers(r),
			grpc.WithContextDialer(c.DialNode),
		)
		if err != nil {
			return nil, fmt.Errorf("creating client with owner credentials failed: %w", err)
		}
		c.authClient = authClient
	}
	return c.authClient, nil
}

// NodeClient returns an authenticated owner connection to the specified
// node within Cluster c, or nil together with an error.
func (c *Cluster) NodeClient(n *NodeInCluster) (*grpc.ClientConn, error) {
	authCreds := rpc.NewAuthenticatedCredentials(c.Owner, rpc.WantRemoteCluster(c.CACertificate), rpc.WantRemoteNode(n.ID))
	endpoint := net.JoinHostPort(n.ManagementAddress, allocs.PortNodeManagement.PortString())
	authClient, err := grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(authCreds),
		grpc.WithContextDialer(c.DialNode),
	)
	if err != nil {
		return nil, fmt.Errorf("creating client with owner credentials failed: %w", err)
	}
	return authClient, nil
}

// LaunchNode launches a single Metropolis node instance with the given options.
// The instance runs mostly paravirtualized but with some emulated hardware
// similar to how a cloud provider might set up its VMs. The disk is fully
// writable, and the changes are kept across reboots and shutdowns. ld and sd
// point to the launch directory and the socket directory, holding the nodes'
// state files (storage, tpm state, firmware state), and UNIX socket files
// (swtpm <-> QEMU interplay) respectively. The directories must exist before
// LaunchNode is called. LaunchNode will update options.Runtime and options.Mac
// if either are not initialized.
func LaunchNode(ctx context.Context, ld, sd string, tpmFactory *TPMFactory, options *NodeOptions, doneC chan error) error {
	// TODO(mateusz@monogon.tech) try using QEMU's abstract socket namespace instead
	// of /tmp (requires QEMU version >5.0).
	// https://github.com/qemu/qemu/commit/776b97d3605ed0fc94443048fdf988c7725e38a9).
	// swtpm accepts already-open FDs so we can pass in an abstract socket namespace FD
	// that we open and pass the name of it to QEMU. Not pinning this crashes both
	// swtpm and qemu because we run into UNIX socket length limitations (for legacy
	// reasons 108 chars).

	if options.CPUs == 0 {
		options.CPUs = 1
	}
	if options.ThreadsPerCPU == 0 {
		options.ThreadsPerCPU = 1
	}
	if options.MemoryMiB == 0 {
		options.MemoryMiB = 2048
	}
	if options.DiskBytes == 0 {
		options.DiskBytes = 5 * 1024 * 1024 * 1024
	}

	// If it's the node's first start, set up its runtime directories.
	if options.Runtime == nil {
		r, err := setupRuntime(ld, sd, options.DiskBytes)
		if err != nil {
			return fmt.Errorf("while setting up node runtime: %w", err)
		}
		options.Runtime = r
	}

	// Replace the node's context with a new one.
	r := options.Runtime
	if r.CtxC != nil {
		r.CtxC()
	}
	r.ctxT, r.CtxC = context.WithCancel(ctx)

	var qemuNetType string
	var qemuNetConfig qemu.QemuValue
	if options.ConnectToSocket != nil {
		qemuNetType = "socket"
		qemuNetConfig = qemu.QemuValue{
			"id": {"net0"},
			"fd": {"3"},
		}
	} else {
		qemuNetType = "user"
		qemuNetConfig = qemu.QemuValue{
			"id":        {"net0"},
			"net":       {"10.42.0.0/24"},
			"dhcpstart": {"10.42.0.10"},
			"hostfwd":   options.Ports.ToQemuForwards(),
		}
	}

	// Generate the node's MAC address if it isn't already set in NodeOptions.
	if options.Mac == nil {
		mac, err := generateRandomEthernetMAC()
		if err != nil {
			return err
		}
		options.Mac = mac
	}

	tpmSocketPath := filepath.Join(r.sd, "tpm-socket")
	fwVarPath := filepath.Join(r.ld, "VARS.fd")
	storagePath := filepath.Join(r.ld, "image.img")
	qemuArgs := []string{
		"-machine", "q35",
		"-accel", "kvm",
		"-display", "none",
		"-nodefaults",
		"-cpu", "host",
		"-m", fmt.Sprintf("%dM", options.MemoryMiB),
		"-smp", fmt.Sprintf("cores=%d,threads=%d", options.CPUs, options.ThreadsPerCPU),
		"-drive", "if=pflash,format=raw,readonly=on,file=" + xOvmfCodePath,
		"-drive", "if=pflash,format=raw,file=" + fwVarPath,
		"-drive", "if=virtio,format=raw,cache=unsafe,file=" + storagePath,
		"-netdev", qemuNetConfig.ToOption(qemuNetType),
		"-device", "virtio-net-pci,netdev=net0,mac=" + options.Mac.String(),
		"-chardev", "socket,id=chrtpm,path=" + tpmSocketPath,
		"-tpmdev", "emulator,id=tpm0,chardev=chrtpm",
		"-device", "tpm-tis,tpmdev=tpm0",
		"-device", "virtio-rng-pci",
		"-serial", "stdio",
	}
	if options.RunVNC {
		vncSocketPath := filepath.Join(r.sd, "vnc-socket")
		qemuArgs = append(qemuArgs,
			"-vnc", "unix:"+vncSocketPath,
			"-device", "virtio-vga",
		)
	}

	if !options.AllowReboot {
		qemuArgs = append(qemuArgs, "-no-reboot")
	}

	if options.NodeParameters != nil {
		parametersPath := filepath.Join(r.ld, "parameters.pb")
		parametersRaw, err := proto.Marshal(options.NodeParameters)
		if err != nil {
			return fmt.Errorf("failed to encode node paraeters: %w", err)
		}
		if err := os.WriteFile(parametersPath, parametersRaw, 0o644); err != nil {
			return fmt.Errorf("failed to write node parameters: %w", err)
		}
		qemuArgs = append(qemuArgs, "-fw_cfg", "name=dev.monogon.metropolis/parameters.pb,file="+parametersPath)
	}

	if options.PcapDump {
		qemuNetDump := qemu.QemuValue{
			"id":     {"net0"},
			"netdev": {"net0"},
			"file":   {filepath.Join(r.ld, "net0.pcap")},
		}
		qemuArgs = append(qemuArgs, "-object", qemuNetDump.ToOption("filter-dump"))
	}

	// Manufacture TPM if needed.
	tpmd := filepath.Join(r.ld, "tpm")
	err := tpmFactory.Manufacture(ctx, tpmd, &TPMPlatform{
		Manufacturer: "Monogon",
		Version:      "1.0",
		Model:        "TestCluster",
	})
	if err != nil {
		return fmt.Errorf("could not manufacture TPM: %w", err)
	}

	// Start TPM emulator as a subprocess
	tpmCtx, tpmCancel := context.WithCancel(options.Runtime.ctxT)

	tpmEmuCmd := exec.CommandContext(tpmCtx, xSwtpmPath, "socket", "--tpm2", "--tpmstate", "dir="+tpmd, "--ctrl", "type=unixio,path="+tpmSocketPath)
	// Silence warnings from unsafe libtpms build (uses non-constant-time
	// cryptographic operations).
	tpmEmuCmd.Env = append(tpmEmuCmd.Env, "MONOGON_LIBTPMS_ACKNOWLEDGE_UNSAFE=yes")
	tpmEmuCmd.Stderr = os.Stderr
	tpmEmuCmd.Stdout = os.Stdout

	err = tpmEmuCmd.Start()
	if err != nil {
		tpmCancel()
		return fmt.Errorf("failed to start TPM emulator: %w", err)
	}

	// Wait for the socket to be created by the TPM emulator before launching
	// QEMU.
	for {
		_, err := os.Stat(tpmSocketPath)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			tpmCancel()
			return fmt.Errorf("while stat-ing TPM socket path: %w", err)
		}
		if err := tpmCtx.Err(); err != nil {
			tpmCancel()
			return fmt.Errorf("while waiting for the TPM socket: %w", err)
		}
		time.Sleep(time.Millisecond * 100)
	}

	// Start the main qemu binary
	systemCmd := exec.CommandContext(options.Runtime.ctxT, xQEMUPath, qemuArgs...)
	if options.ConnectToSocket != nil {
		systemCmd.ExtraFiles = []*os.File{options.ConnectToSocket}
	}

	var stdErrBuf bytes.Buffer
	systemCmd.Stderr = &stdErrBuf
	systemCmd.Stdout = options.SerialPort

	qemu.PrettyPrintQemuArgs(options.Name, systemCmd.Args)

	go func() {
		logf("Node: Starting...")
		err = systemCmd.Run()
		logf("Node: Returned: %v", err)

		// Stop TPM emulator and wait for it to exit to properly reap the child process
		tpmCancel()
		logf("Node: Waiting for TPM emulator to exit")
		// Wait returns a SIGKILL error because we just cancelled its context.
		// We still need to call it to avoid creating zombies.
		errTpm := tpmEmuCmd.Wait()
		logf("Node: TPM emulator done: %v", errTpm)

		var exerr *exec.ExitError
		if err != nil && errors.As(err, &exerr) {
			status := exerr.ProcessState.Sys().(syscall.WaitStatus)
			if status.Signaled() && status.Signal() == syscall.SIGKILL {
				// Process was killed externally (most likely by our context being canceled).
				// This is a normal exit for us, so return nil
				doneC <- nil
				return
			}
			exerr.Stderr = stdErrBuf.Bytes()
			newErr := qemu.QEMUError(*exerr)
			logf("Node: %q", stdErrBuf.String())
			doneC <- &newErr
			return
		}
		doneC <- err
	}()
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("when opening source: %w", err)
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("when creating destination: %w", err)
	}
	defer out.Close()

	endPos, err := in.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("when getting source end: %w", err)
	}

	// Copy the file while preserving its sparseness. The image files are very
	// sparse (less than 10% allocated), so this is a lot faster.
	var lastHoleStart int64
	for {
		dataStart, err := in.Seek(lastHoleStart, unix.SEEK_DATA)
		if err != nil {
			return fmt.Errorf("when seeking to next data block: %w", err)
		}
		holeStart, err := in.Seek(dataStart, unix.SEEK_HOLE)
		if err != nil {
			return fmt.Errorf("when seeking to next hole: %w", err)
		}
		lastHoleStart = holeStart
		if _, err := in.Seek(dataStart, io.SeekStart); err != nil {
			return fmt.Errorf("when seeking to current data block: %w", err)
		}
		if _, err := out.Seek(dataStart, io.SeekStart); err != nil {
			return fmt.Errorf("when seeking output to next data block: %w", err)
		}
		if _, err := io.CopyN(out, in, holeStart-dataStart); err != nil {
			return fmt.Errorf("when copying file: %w", err)
		}
		if endPos == holeStart {
			// The next hole is at the end of the file, we're done here.
			break
		}
	}

	return out.Close()
}

// getNodes wraps around Management.GetNodes to return a list of nodes in a
// cluster.
func getNodes(ctx context.Context, mgmt apb.ManagementClient) ([]*apb.Node, error) {
	var res []*apb.Node
	bo := backoff.WithContext(backoff.NewExponentialBackOff(), ctx)
	err := backoff.Retry(func() error {
		res = nil
		srvN, err := mgmt.GetNodes(ctx, &apb.GetNodesRequest{})
		if err != nil {
			return fmt.Errorf("GetNodes: %w", err)
		}
		for {
			node, err := srvN.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("GetNodes.Recv: %w", err)
			}
			res = append(res, node)
		}
		return nil
	}, bo)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// getNode wraps Management.GetNodes. It returns node information matching
// given node ID.
func getNode(ctx context.Context, mgmt apb.ManagementClient, id string) (*apb.Node, error) {
	nodes, err := getNodes(ctx, mgmt)
	if err != nil {
		return nil, fmt.Errorf("could not get nodes: %w", err)
	}
	for _, n := range nodes {
		if n.Id == id {
			return n, nil
		}
	}
	return nil, fmt.Errorf("no such node")
}

// Gets a random EUI-48 Ethernet MAC address
func generateRandomEthernetMAC() (*net.HardwareAddr, error) {
	macBuf := make([]byte, 6)
	_, err := rand.Read(macBuf)
	if err != nil {
		return nil, fmt.Errorf("failed to read randomness for MAC: %w", err)
	}

	// Set U/L bit and clear I/G bit (locally administered individual MAC)
	// Ref IEEE 802-2014 Section 8.2.2
	macBuf[0] = (macBuf[0] | 2) & 0xfe
	mac := net.HardwareAddr(macBuf)
	return &mac, nil
}

const SOCKSPort uint16 = 1080

// ClusterPorts contains all ports handled by Nanoswitch.
var ClusterPorts = []uint16{
	// Forwarded to the first node.
	uint16(allocs.PortCuratorService),
	uint16(allocs.PortDebugService),
	uint16(allocs.PortKubernetesAPI),
	uint16(allocs.PortKubernetesAPIWrapped),

	// SOCKS proxy to the switch network
	SOCKSPort,
}

// ClusterOptions contains all options for launching a Metropolis cluster.
type ClusterOptions struct {
	// The number of nodes this cluster should be started with.
	NumNodes int

	// Node are default options of all nodes.
	Node NodeOptions

	// If true, node logs will be saved to individual files instead of being printed
	// out to stderr. The path of these files will be still printed to stdout.
	//
	// The files will be located within the launch directory inside TEST_TMPDIR (or
	// the default tempdir location, if not set).
	NodeLogsToFiles bool

	// LeaveNodesNew, if set, will leave all non-bootstrap nodes in NEW, without
	// bootstrapping them. The nodes' address information in Cluster.Nodes will be
	// incomplete.
	LeaveNodesNew bool

	// Optional local registry which will be made available to the cluster to
	// pull images from. This is a more efficient alternative to preseeding all
	// images used for testing.
	LocalRegistry *registry.Server

	// InitialClusterConfiguration will be passed to the first node when creating the
	// cluster, and defines some basic properties of the cluster. If not specified,
	// the cluster will default to defaults as defined in
	// metropolis.proto.api.NodeParameters.
	InitialClusterConfiguration *cpb.ClusterConfiguration
}

// Cluster is the running Metropolis cluster launched using the LaunchCluster
// function.
type Cluster struct {
	// Owner is the TLS Certificate of the owner of the test cluster. This can be
	// used to authenticate further clients to the running cluster.
	Owner tls.Certificate
	// Ports is the PortMap used to access the first nodes' services (defined in
	// ClusterPorts) and the SOCKS proxy (at SOCKSPort).
	Ports qemu.PortMap

	// Nodes is a map from Node ID to its runtime information.
	Nodes map[string]*NodeInCluster
	// NodeIDs is a list of node IDs that are backing this cluster, in order of
	// creation.
	NodeIDs []string

	// CACertificate is the cluster's CA certificate.
	CACertificate *x509.Certificate

	// nodesDone is a list of channels populated with the return codes from all the
	// nodes' qemu instances. It's used by Close to ensure all nodes have
	// successfully been stopped.
	nodesDone []chan error
	// nodeOpts are the cluster member nodes' mutable launch options, kept here
	// to facilitate reboots.
	nodeOpts []NodeOptions
	// launchDir points at the directory keeping the nodes' state, such as storage
	// images, firmware variable files, TPM state.
	launchDir string
	// socketDir points at the directory keeping UNIX socket files, such as these
	// used to facilitate communication between QEMU and swtpm. It's different
	// from launchDir, and anchored nearer the file system root, due to the
	// socket path length limitation imposed by the kernel.
	socketDir   string
	metroctlDir string

	// SOCKSDialer is used by DialNode to establish connections to nodes via the
	// SOCKS server ran by nanoswitch.
	SOCKSDialer proxy.Dialer

	// authClient is a cached authenticated owner connection to a Curator
	// instance within the cluster.
	authClient *grpc.ClientConn

	// ctxT is the context individual node contexts are created from.
	ctxT context.Context
	// ctxC is used by Close to cancel the context under which the nodes are
	// running.
	ctxC context.CancelFunc

	tpmFactory *TPMFactory
}

// NodeInCluster represents information about a node that's part of a Cluster.
type NodeInCluster struct {
	// ID of the node, which can be used to dial this node's services via
	// NewNodeClient.
	ID     string
	Pubkey []byte
	// Address of the node on the network ran by nanoswitch. Not reachable from
	// the host unless dialed via NewNodeClient or via the nanoswitch SOCKS
	// proxy (reachable on Cluster.Ports[SOCKSPort]).
	ManagementAddress string
}

// firstConnection performs the initial owner credential escrow with a newly
// started nanoswitch-backed cluster over SOCKS. It expects the first node to be
// running at 10.1.0.2, which is always the case with the current nanoswitch
// implementation.
//
// It returns the newly escrowed credentials as well as the first node's
// information as NodeInCluster.
func firstConnection(ctx context.Context, socksDialer proxy.Dialer) (*tls.Certificate, *NodeInCluster, error) {
	// Dial external service.
	remote := fmt.Sprintf("10.1.0.2:%s", allocs.PortCuratorService.PortString())
	initCreds, err := rpc.NewEphemeralCredentials(InsecurePrivateKey, rpc.WantInsecure())
	if err != nil {
		return nil, nil, fmt.Errorf("NewEphemeralCredentials: %w", err)
	}
	initDialer := func(_ context.Context, addr string) (net.Conn, error) {
		return socksDialer.Dial("tcp", addr)
	}
	initClient, err := grpc.NewClient(remote, grpc.WithContextDialer(initDialer), grpc.WithTransportCredentials(initCreds))
	if err != nil {
		return nil, nil, fmt.Errorf("creating client with ephemeral credentials failed: %w", err)
	}
	defer initClient.Close()

	// Retrieve owner certificate - this can take a while because the node is still
	// coming up, so do it in a backoff loop.
	logf("Cluster: retrieving owner certificate (this can take a few seconds while the first node boots)...")
	aaa := apb.NewAAAClient(initClient)
	var cert *tls.Certificate
	err = backoff.Retry(func() error {
		cert, err = rpc.RetrieveOwnerCertificate(ctx, aaa, InsecurePrivateKey)
		if st, ok := status.FromError(err); ok {
			if st.Code() == codes.Unavailable {
				logf("Cluster: cluster UNAVAILABLE: %v", st.Message())
				return err
			}
		}
		return backoff.Permanent(err)
	}, backoff.WithContext(backoff.NewExponentialBackOff(backoff.WithMaxElapsedTime(time.Minute)), ctx))
	if err != nil {
		return nil, nil, fmt.Errorf("couldn't retrieve owner certificate: %w", err)
	}
	logf("Cluster: retrieved owner certificate.")

	// Now connect authenticated and get the node ID.
	creds := rpc.NewAuthenticatedCredentials(*cert, rpc.WantInsecure())
	authClient, err := grpc.NewClient(remote, grpc.WithContextDialer(initDialer), grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, nil, fmt.Errorf("creating client with owner credentials failed: %w", err)
	}
	defer authClient.Close()
	mgmt := apb.NewManagementClient(authClient)

	var node *NodeInCluster
	err = backoff.Retry(func() error {
		nodes, err := getNodes(ctx, mgmt)
		if err != nil {
			return fmt.Errorf("retrieving nodes failed: %w", err)
		}
		if len(nodes) != 1 {
			return fmt.Errorf("expected one node, got %d", len(nodes))
		}
		n := nodes[0]
		if n.Status == nil || n.Status.ExternalAddress == "" {
			return fmt.Errorf("node has no status and/or address")
		}
		node = &NodeInCluster{
			ID:                n.Id,
			ManagementAddress: n.Status.ExternalAddress,
		}
		return nil
	}, backoff.WithContext(backoff.NewExponentialBackOff(), ctx))
	if err != nil {
		return nil, nil, err
	}

	return cert, node, nil
}

func NewSerialFileLogger(p string) (io.ReadWriter, error) {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// LaunchCluster launches a cluster of Metropolis node VMs together with a
// Nanoswitch instance to network them all together.
//
// The given context will be used to run all qemu instances in the cluster, and
// canceling the context or calling Close() will terminate them.
func LaunchCluster(ctx context.Context, opts ClusterOptions) (*Cluster, error) {
	if opts.NumNodes <= 0 {
		return nil, errors.New("refusing to start cluster with zero nodes")
	}

	// Prepare the node options. These will be kept as part of Cluster.
	// nodeOpts[].Runtime will be initialized by LaunchNode during the first
	// launch. The runtime information can be later used to restart a node.
	// The 0th node will be initialized first. The rest will follow after it
	// had bootstrapped the cluster.
	nodeOpts := make([]NodeOptions, opts.NumNodes)
	for i := range opts.NumNodes {
		nodeOpts[i] = opts.Node
		nodeOpts[i].Name = fmt.Sprintf("node%d", i)
		nodeOpts[i].SerialPort = newPrefixedStdio(i)
		nodeOpts[i].RunVNC = true
	}
	nodeOpts[0].NodeParameters = &apb.NodeParameters{
		Cluster: &apb.NodeParameters_ClusterBootstrap_{
			ClusterBootstrap: &apb.NodeParameters_ClusterBootstrap{
				OwnerPublicKey:              InsecurePublicKey,
				InitialClusterConfiguration: opts.InitialClusterConfiguration,
				Labels: &cpb.NodeLabels{
					Pairs: []*cpb.NodeLabels_Pair{
						{Key: NodeNumberKey, Value: "0"},
					},
				},
			},
		},
	}
	nodeOpts[0].PcapDump = true

	// Create the launch directory.
	ld, err := os.MkdirTemp(os.Getenv("TEST_TMPDIR"), "cluster-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create the launch directory: %w", err)
	}

	nodeLogDir := ld
	if os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR") != "" {
		nodeLogDir = os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR")
	}

	// Create the metroctl config directory. We keep it in /tmp because in some
	// scenarios it's end-user visible and we want it short.
	md, err := os.MkdirTemp("/tmp", "metroctl-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create the metroctl directory: %w", err)
	}

	// Create the socket directory. We keep it in /tmp because of socket path limits.
	sd, err := os.MkdirTemp("/tmp", "cluster-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create the socket directory: %w", err)
	}

	// Set up TPM factory.
	tpmf, err := NewTPMFactory(filepath.Join(ld, "tpm"))
	if err != nil {
		return nil, fmt.Errorf("failed to create TPM factory: %w", err)
	}

	// Prepare links between nodes and nanoswitch.
	var switchPorts []*os.File
	for i := range opts.NumNodes {
		switchPort, vmPort, err := qemu.NewSocketPair()
		if err != nil {
			return nil, fmt.Errorf("failed to get socketpair: %w", err)
		}
		switchPorts = append(switchPorts, switchPort)
		nodeOpts[i].ConnectToSocket = vmPort
	}

	// Make a list of channels that will be populated by all running node qemu
	// processes.
	done := make([]chan error, opts.NumNodes)
	for i := range done {
		done[i] = make(chan error, 1)
	}

	if opts.NodeLogsToFiles {
		for i := range opts.NumNodes {
			path := path.Join(nodeLogDir, fmt.Sprintf("node-%d.txt", i))
			port, err := NewSerialFileLogger(path)
			if err != nil {
				return nil, fmt.Errorf("could not open log file for node %d: %w", i, err)
			}
			logf("Node %d logs at %s", i, path)
			nodeOpts[i].SerialPort = port
		}
	}

	// Start the first node.
	ctxT, ctxC := context.WithCancel(ctx)
	logf("Cluster: Starting node %d...", 0)
	if err := LaunchNode(ctxT, ld, sd, tpmf, &nodeOpts[0], done[0]); err != nil {
		ctxC()
		return nil, fmt.Errorf("failed to launch first node: %w", err)
	}

	localRegistryAddr := net.TCPAddr{
		IP:   net.IPv4(10, 42, 0, 82),
		Port: 5000,
	}

	var guestSvcMap qemu.GuestServiceMap
	if opts.LocalRegistry != nil {
		l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			ctxC()
			return nil, fmt.Errorf("failed to create TCP listener for local registry: %w", err)
		}
		s := http.Server{
			Handler: opts.LocalRegistry,
		}
		go s.Serve(l)
		go func() {
			<-ctxT.Done()
			s.Close()
		}()
		guestSvcMap = qemu.GuestServiceMap{
			&localRegistryAddr: *l.Addr().(*net.TCPAddr),
		}
	}

	// Launch nanoswitch.
	portMap, err := qemu.ConflictFreePortMap(ClusterPorts)
	if err != nil {
		ctxC()
		return nil, fmt.Errorf("failed to allocate ephemeral ports: %w", err)
	}

	go func() {
		var serialPort io.ReadWriter
		var err error
		if opts.NodeLogsToFiles {

			loggerPath := path.Join(nodeLogDir, "nanoswitch.txt")
			serialPort, err = NewSerialFileLogger(loggerPath)
			if err != nil {
				logf("Could not open log file for nanoswitch: %v", err)
				os.Exit(1)
			}
			logf("Nanoswitch logs at %s", loggerPath)
		} else {
			serialPort = newPrefixedStdio(99)
		}
		if err := qemu.RunMicroVM(ctxT, &qemu.MicroVMOptions{
			Name:                   "nanoswitch",
			KernelPath:             xKernelPath,
			InitramfsPath:          xInitramfsPath,
			ExtraNetworkInterfaces: switchPorts,
			PortMap:                portMap,
			GuestServiceMap:        guestSvcMap,
			SerialPort:             serialPort,
			PcapDump:               path.Join(nodeLogDir, "nanoswitch.pcap"),
		}); err != nil {
			if !errors.Is(err, ctxT.Err()) {
				logf("Failed to launch nanoswitch: %v", err)
				os.Exit(1)
			}
		}
	}()

	// Build SOCKS dialer.
	socksRemote := fmt.Sprintf("localhost:%v", portMap[SOCKSPort])
	socksDialer, err := proxy.SOCKS5("tcp", socksRemote, nil, proxy.Direct)
	if err != nil {
		ctxC()
		return nil, fmt.Errorf("failed to build SOCKS dialer: %w", err)
	}

	// Retrieve owner credentials and first node.
	cert, firstNode, err := firstConnection(ctxT, socksDialer)
	if err != nil {
		ctxC()
		return nil, err
	}

	// Write credentials to the metroctl directory.
	if err := metroctl.WriteOwnerKey(md, cert.PrivateKey.(ed25519.PrivateKey)); err != nil {
		ctxC()
		return nil, fmt.Errorf("could not write owner key: %w", err)
	}
	if err := metroctl.WriteOwnerCertificate(md, cert.Certificate[0]); err != nil {
		ctxC()
		return nil, fmt.Errorf("could not write owner certificate: %w", err)
	}

	logf("Cluster: Node %d is %s", 0, firstNode.ID)

	// Set up a partially initialized cluster instance, to be filled in the
	// later steps.
	cluster := &Cluster{
		Owner: *cert,
		Ports: portMap,
		Nodes: map[string]*NodeInCluster{
			firstNode.ID: firstNode,
		},
		NodeIDs: []string{
			firstNode.ID,
		},

		nodesDone:   done,
		nodeOpts:    nodeOpts,
		launchDir:   ld,
		socketDir:   sd,
		metroctlDir: md,

		SOCKSDialer: socksDialer,

		ctxT: ctxT,
		ctxC: ctxC,

		tpmFactory: tpmf,
	}

	// Now start the rest of the nodes and register them into the cluster.

	// Get an authenticated owner client within the cluster.
	curC, err := cluster.CuratorClient()
	if err != nil {
		ctxC()
		return nil, fmt.Errorf("CuratorClient: %w", err)
	}
	mgmt := apb.NewManagementClient(curC)

	// Retrieve register ticket to register further nodes.
	logf("Cluster: retrieving register ticket...")
	resT, err := mgmt.GetRegisterTicket(ctx, &apb.GetRegisterTicketRequest{})
	if err != nil {
		ctxC()
		return nil, fmt.Errorf("GetRegisterTicket: %w", err)
	}
	ticket := resT.Ticket
	logf("Cluster: retrieved register ticket (%d bytes).", len(ticket))

	// Retrieve cluster info (for directory and ca public key) to register further
	// nodes.
	resI, err := mgmt.GetClusterInfo(ctx, &apb.GetClusterInfoRequest{})
	if err != nil {
		ctxC()
		return nil, fmt.Errorf("GetClusterInfo: %w", err)
	}
	caCert, err := x509.ParseCertificate(resI.CaCertificate)
	if err != nil {
		ctxC()
		return nil, fmt.Errorf("ParseCertificate: %w", err)
	}
	cluster.CACertificate = caCert

	// Use the retrieved information to configure the rest of the node options.
	for i := 1; i < opts.NumNodes; i++ {
		nodeOpts[i].NodeParameters = &apb.NodeParameters{
			Cluster: &apb.NodeParameters_ClusterRegister_{
				ClusterRegister: &apb.NodeParameters_ClusterRegister{
					RegisterTicket:   ticket,
					ClusterDirectory: resI.ClusterDirectory,
					CaCertificate:    resI.CaCertificate,
					Labels: &cpb.NodeLabels{
						Pairs: []*cpb.NodeLabels_Pair{
							{Key: NodeNumberKey, Value: fmt.Sprintf("%d", i)},
						},
					},
				},
			},
		}
	}

	// Now run the rest of the nodes.
	for i := 1; i < opts.NumNodes; i++ {
		logf("Cluster: Starting node %d...", i)
		err := LaunchNode(ctxT, ld, sd, tpmf, &nodeOpts[i], done[i])
		if err != nil {
			return nil, fmt.Errorf("failed to launch node %d: %w", i, err)
		}
	}

	// Wait for nodes to appear as NEW, populate a map from node number (index into
	// nodeOpts, etc.) to Metropolis Node ID.
	seenNodes := make(map[string]bool)
	nodeNumberToID := make(map[int]string)
	logf("Cluster: waiting for nodes to appear as NEW...")
	for i := 1; i < opts.NumNodes; i++ {
		for {
			nodes, err := getNodes(ctx, mgmt)
			if err != nil {
				ctxC()
				return nil, fmt.Errorf("could not get nodes: %w", err)
			}
			for _, n := range nodes {
				if n.State != cpb.NodeState_NODE_STATE_NEW {
					continue
				}
				if seenNodes[n.Id] {
					continue
				}
				seenNodes[n.Id] = true
				cluster.Nodes[n.Id] = &NodeInCluster{
					ID:     n.Id,
					Pubkey: n.Pubkey,
				}

				num, err := strconv.Atoi(node.GetNodeLabel(n.Labels, NodeNumberKey))
				if err != nil {
					return nil, fmt.Errorf("node %s has undecodable number label: %w", n.Id, err)
				}
				logf("Cluster: Node %d is %s", num, n.Id)
				nodeNumberToID[num] = n.Id
			}

			if len(seenNodes) == opts.NumNodes-1 {
				break
			}
			time.Sleep(1 * time.Second)
		}
	}
	logf("Found all expected nodes")

	// Build the rest of NodeIDs from map.
	for i := 1; i < opts.NumNodes; i++ {
		cluster.NodeIDs = append(cluster.NodeIDs, nodeNumberToID[i])
	}

	approvedNodes := make(map[string]bool)
	upNodes := make(map[string]bool)
	if !opts.LeaveNodesNew {
		for {
			nodes, err := getNodes(ctx, mgmt)
			if err != nil {
				ctxC()
				return nil, fmt.Errorf("could not get nodes: %w", err)
			}
			for _, node := range nodes {
				if !seenNodes[node.Id] {
					// Skip nodes that weren't NEW in the previous step.
					continue
				}

				if node.State == cpb.NodeState_NODE_STATE_UP && node.Status != nil && node.Status.ExternalAddress != "" {
					logf("Cluster: node %s is up", node.Id)
					upNodes[node.Id] = true
					cluster.Nodes[node.Id].ManagementAddress = node.Status.ExternalAddress
				}
				if upNodes[node.Id] {
					continue
				}

				if !approvedNodes[node.Id] {
					logf("Cluster: approving node %s", node.Id)
					_, err := mgmt.ApproveNode(ctx, &apb.ApproveNodeRequest{
						Pubkey: node.Pubkey,
					})
					if err != nil {
						ctxC()
						return nil, fmt.Errorf("ApproveNode(%s): %w", node.Id, err)
					}
					approvedNodes[node.Id] = true
				}
			}

			logf("Cluster: want %d up nodes, have %d", opts.NumNodes, len(upNodes)+1)
			if len(upNodes) == opts.NumNodes-1 {
				break
			}
			time.Sleep(time.Second)
		}
	}

	logf("Cluster: all nodes up:")
	for i, nodeID := range cluster.NodeIDs {
		logf("Cluster:  %d. %s at %s", i, nodeID, cluster.Nodes[nodeID].ManagementAddress)
	}
	logf("Cluster: starting tests...")

	return cluster, nil
}

// RebootNode reboots the cluster member node matching the given index, and
// waits for it to rejoin the cluster. It will use the given context ctx to run
// cluster API requests, whereas the resulting QEMU process will be created
// using the cluster's context c.ctxT. The nodes are indexed starting at 0.
func (c *Cluster) RebootNode(ctx context.Context, idx int) error {
	if idx < 0 || idx >= len(c.NodeIDs) {
		return fmt.Errorf("index out of bounds")
	}
	if c.nodeOpts[idx].Runtime == nil {
		return fmt.Errorf("node not running")
	}
	id := c.NodeIDs[idx]

	// Get an authenticated owner client within the cluster.
	curC, err := c.CuratorClient()
	if err != nil {
		return err
	}
	mgmt := apb.NewManagementClient(curC)

	nodeC, err := c.NodeClient(c.Nodes[id])
	if err != nil {
		return err
	}

	// Shut down the Node. QEMU will exit once the node powers it off.
	nmgmt := apb.NewNodeManagementClient(nodeC)
	_, err = nmgmt.Reboot(ctx, &apb.RebootRequest{
		Type:     apb.RebootRequest_TYPE_POWER_OFF,
		NextBoot: apb.RebootRequest_NEXT_BOOT_START_NORMAL,
	})
	if err != nil {
		return err
	}

	logf("Cluster: waiting for node %d (%s) to stop.", idx, id)
	err = <-c.nodesDone[idx]
	if err != nil {
		return fmt.Errorf("while restarting node: %w", err)
	}

	// Start QEMU again.
	logf("Cluster: restarting node %d (%s).", idx, id)
	if err := LaunchNode(c.ctxT, c.launchDir, c.socketDir, c.tpmFactory, &c.nodeOpts[idx], c.nodesDone[idx]); err != nil {
		return fmt.Errorf("failed to launch node %d: %w", idx, err)
	}

	start := time.Now()

	// Poll Management.GetNodes until the node is healthy.
	for {
		cs, err := getNode(ctx, mgmt, id)
		if err != nil {
			logf("Cluster: node get error: %v", err)
			return err
		}
		logf("Cluster: node health: %+v", cs.Health)

		lhb := time.Now().Add(-cs.TimeSinceHeartbeat.AsDuration())
		if lhb.After(start) && cs.Health == apb.Node_HEALTH_HEALTHY {
			break
		}
		time.Sleep(time.Second)
	}
	logf("Cluster: node %d (%s) has rejoined the cluster.", idx, id)
	return nil
}

// ShutdownNode performs an ungraceful shutdown (i.e. power off) of the node
// given by idx. If the node is already shut down, this is a no-op.
func (c *Cluster) ShutdownNode(idx int) error {
	if idx < 0 || idx >= len(c.NodeIDs) {
		return fmt.Errorf("index out of bounds")
	}
	// Return if node is already stopped.
	select {
	case <-c.nodeOpts[idx].Runtime.ctxT.Done():
		return nil
	default:
	}
	id := c.NodeIDs[idx]

	// Cancel the node's context. This will shut down QEMU.
	c.nodeOpts[idx].Runtime.CtxC()
	logf("Cluster: waiting for node %d (%s) to stop.", idx, id)
	err := <-c.nodesDone[idx]
	if err != nil {
		return fmt.Errorf("while shutting down node: %w", err)
	}
	logf("Cluster: node %d (%s) stopped.", idx, id)
	return nil
}

// StartNode performs a power on of the node given by idx. If the node is already
// running, this is a no-op.
func (c *Cluster) StartNode(idx int) error {
	if idx < 0 || idx >= len(c.NodeIDs) {
		return fmt.Errorf("index out of bounds")
	}
	id := c.NodeIDs[idx]
	// Return if node is already running.
	select {
	case <-c.nodeOpts[idx].Runtime.ctxT.Done():
	default:
		return nil
	}

	// Start QEMU again.
	logf("Cluster: starting node %d (%s).", idx, id)
	if err := LaunchNode(c.ctxT, c.launchDir, c.socketDir, c.tpmFactory, &c.nodeOpts[idx], c.nodesDone[idx]); err != nil {
		return fmt.Errorf("failed to launch node %d: %w", idx, err)
	}
	logf("Cluster: node %d (%s) started.", idx, id)
	return nil
}

// Close cancels the running clusters' context and waits for all virtualized
// nodes to stop. It returns an error if stopping the nodes failed, or one of
// the nodes failed to fully start in the first place.
func (c *Cluster) Close() error {
	logf("Cluster: stopping...")
	if c.authClient != nil {
		c.authClient.Close()
	}
	c.ctxC()

	var errs []error
	logf("Cluster: waiting for nodes to exit...")
	for _, c := range c.nodesDone {
		err := <-c
		if err != nil {
			errs = append(errs, err)
		}
	}
	logf("Cluster: removing nodes' state files (%s) and sockets (%s).", c.launchDir, c.socketDir)
	os.RemoveAll(c.launchDir)
	os.RemoveAll(c.socketDir)
	os.RemoveAll(c.metroctlDir)
	logf("Cluster: done")
	return multierr.Combine(errs...)
}

// DialNode is a grpc.WithContextDialer compatible dialer which dials nodes by
// their ID. This is performed by connecting to the cluster nanoswitch via its
// SOCKS proxy, and using the cluster node list for name resolution.
//
// For example:
//
//	grpc.NewClient("passthrough:///metropolis-deadbeef:1234", grpc.WithContextDialer(c.DialNode))
func (c *Cluster) DialNode(_ context.Context, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid host:port: %w", err)
	}
	// Already an IP address?
	if net.ParseIP(host) != nil {
		return c.SOCKSDialer.Dial("tcp", addr)
	}

	// Otherwise, expect a node name.
	node, ok := c.Nodes[host]
	if !ok {
		return nil, fmt.Errorf("unknown node %q", host)
	}
	addr = net.JoinHostPort(node.ManagementAddress, port)
	return c.SOCKSDialer.Dial("tcp", addr)
}

// GetKubeClientSet gets a Kubernetes client set accessing the Metropolis
// Kubernetes authenticating proxy using the cluster owner identity.
// It currently has access to everything (i.e. the cluster-admin role)
// via the owner-admin binding.
func (c *Cluster) GetKubeClientSet() (kubernetes.Interface, *rest.Config, error) {
	pkcs8Key, err := x509.MarshalPKCS8PrivateKey(c.Owner.PrivateKey)
	if err != nil {
		// We explicitly pass an Ed25519 private key in, so this can't happen
		panic(err)
	}

	host := net.JoinHostPort(c.NodeIDs[0], allocs.PortKubernetesAPIWrapped.PortString())
	clientConfig := rest.Config{
		Host: host,
		TLSClientConfig: rest.TLSClientConfig{
			// TODO(q3k): use CA certificate
			Insecure:   true,
			ServerName: "kubernetes.default.svc",
			CertData:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Owner.Certificate[0]}),
			KeyData:    pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8Key}),
		},
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return c.DialNode(ctx, address)
		},
	}
	clientSet, err := kubernetes.NewForConfig(&clientConfig)
	if err != nil {
		return nil, nil, err
	}
	return clientSet, &clientConfig, nil
}

// KubernetesControllerNodeAddresses returns the list of IP addresses of nodes
// which are currently Kubernetes controllers, ie. run an apiserver. This list
// might be empty if no node is currently configured with the
// 'KubernetesController' node.
func (c *Cluster) KubernetesControllerNodeAddresses(ctx context.Context) ([]string, error) {
	curC, err := c.CuratorClient()
	if err != nil {
		return nil, err
	}
	mgmt := apb.NewManagementClient(curC)
	srv, err := mgmt.GetNodes(ctx, &apb.GetNodesRequest{
		Filter: "has(node.roles.kubernetes_controller)",
	})
	if err != nil {
		return nil, err
	}
	defer srv.CloseSend()
	var res []string
	for {
		n, err := srv.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if n.Status == nil || n.Status.ExternalAddress == "" {
			continue
		}
		res = append(res, n.Status.ExternalAddress)
	}
	return res, nil
}

// AllNodesHealthy returns nil if all the nodes in the cluster are seemingly
// healthy.
func (c *Cluster) AllNodesHealthy(ctx context.Context) error {
	// Get an authenticated owner client within the cluster.
	curC, err := c.CuratorClient()
	if err != nil {
		return err
	}
	mgmt := apb.NewManagementClient(curC)
	nodes, err := getNodes(ctx, mgmt)
	if err != nil {
		return err
	}

	var unhealthy []string
	for _, node := range nodes {
		if node.Health == apb.Node_HEALTH_HEALTHY {
			continue
		}
		unhealthy = append(unhealthy, node.Id)
	}
	if len(unhealthy) == 0 {
		return nil
	}
	return fmt.Errorf("nodes unhealthy: %s", strings.Join(unhealthy, ", "))
}

// ApproveNode approves a node by ID, waiting for it to become UP.
func (c *Cluster) ApproveNode(ctx context.Context, id string) error {
	curC, err := c.CuratorClient()
	if err != nil {
		return err
	}
	mgmt := apb.NewManagementClient(curC)

	_, err = mgmt.ApproveNode(ctx, &apb.ApproveNodeRequest{
		Pubkey: c.Nodes[id].Pubkey,
	})
	if err != nil {
		return fmt.Errorf("ApproveNode: %w", err)
	}
	logf("Cluster: %s: approved, waiting for UP", id)
	for {
		nodes, err := mgmt.GetNodes(ctx, &apb.GetNodesRequest{})
		if err != nil {
			return fmt.Errorf("GetNodes: %w", err)
		}
		found := false
		for {
			node, err := nodes.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return fmt.Errorf("Nodes.Recv: %w", err)
			}
			if node.Id != id {
				continue
			}
			if node.State != cpb.NodeState_NODE_STATE_UP {
				continue
			}
			found = true
			break
		}
		nodes.CloseSend()

		if found {
			break
		}
		time.Sleep(time.Second)
	}
	logf("Cluster: %s: UP", id)
	return nil
}

// MakeKubernetesWorker adds the KubernetesWorker role to a node by ID.
func (c *Cluster) MakeKubernetesWorker(ctx context.Context, id string) error {
	curC, err := c.CuratorClient()
	if err != nil {
		return err
	}
	mgmt := apb.NewManagementClient(curC)

	logf("Cluster: %s: adding KubernetesWorker", id)
	_, err = mgmt.UpdateNodeRoles(ctx, &apb.UpdateNodeRolesRequest{
		Node: &apb.UpdateNodeRolesRequest_Id{
			Id: id,
		},
		KubernetesWorker: ptr.To(true),
	})
	return err
}

// MakeKubernetesController adds the KubernetesController role to a node by ID.
func (c *Cluster) MakeKubernetesController(ctx context.Context, id string) error {
	curC, err := c.CuratorClient()
	if err != nil {
		return err
	}
	mgmt := apb.NewManagementClient(curC)

	logf("Cluster: %s: adding KubernetesController", id)
	_, err = mgmt.UpdateNodeRoles(ctx, &apb.UpdateNodeRolesRequest{
		Node: &apb.UpdateNodeRolesRequest_Id{
			Id: id,
		},
		KubernetesController: ptr.To(true),
	})
	return err
}

// MakeConsensusMember adds the ConsensusMember role to a node by ID.
func (c *Cluster) MakeConsensusMember(ctx context.Context, id string) error {
	curC, err := c.CuratorClient()
	if err != nil {
		return err
	}
	mgmt := apb.NewManagementClient(curC)
	cur := ipb.NewCuratorClient(curC)

	logf("Cluster: %s: adding ConsensusMember", id)
	bo := backoff.NewExponentialBackOff()
	bo.MaxElapsedTime = 10 * time.Second

	backoff.Retry(func() error {
		_, err = mgmt.UpdateNodeRoles(ctx, &apb.UpdateNodeRolesRequest{
			Node: &apb.UpdateNodeRolesRequest_Id{
				Id: id,
			},
			ConsensusMember: ptr.To(true),
		})
		if err != nil {
			logf("Cluster: %s: UpdateNodeRoles failed: %v", id, err)
		}
		return err
	}, backoff.WithContext(bo, ctx))
	if err != nil {
		return err
	}

	logf("Cluster: %s: waiting for learner/full members...", id)

	learner := false
	for {
		res, err := cur.GetConsensusStatus(ctx, &ipb.GetConsensusStatusRequest{})
		if err != nil {
			return fmt.Errorf("GetConsensusStatus: %w", err)
		}
		for _, member := range res.EtcdMember {
			if member.Id != id {
				continue
			}
			switch member.Status {
			case ipb.GetConsensusStatusResponse_EtcdMember_STATUS_LEARNER:
				if !learner {
					learner = true
					logf("Cluster: %s: became a learner, waiting for full member...", id)
				}
			case ipb.GetConsensusStatusResponse_EtcdMember_STATUS_FULL:
				logf("Cluster: %s: became a full member", id)
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}
