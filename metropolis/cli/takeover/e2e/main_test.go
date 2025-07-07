// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
	xssh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/sys/unix"

	"source.monogon.dev/osbase/fat32"
	"source.monogon.dev/osbase/freeport"
	"source.monogon.dev/osbase/structfs"
)

var (
	// These are filled by bazel at linking time with the canonical path of
	// their corresponding file. Inside the init function we resolve it
	// with the rules_go runfiles package to the real path.
	xImagePath      string
	xOvmfVarsPath   string
	xOvmfCodePath   string
	xCloudImagePath string
	xTakeoverPath   string
	xMetroctlPath   string
	xQEMUPath       string
)

func init() {
	var err error
	for _, path := range []*string{
		&xCloudImagePath, &xOvmfVarsPath, &xOvmfCodePath,
		&xTakeoverPath, &xImagePath, &xMetroctlPath,
		&xQEMUPath,
	} {
		*path, err = runfiles.Rlocation(*path)
		if err != nil {
			panic(err)
		}
	}
}

const GiB = 1024 * 1024 * 1024

func TestE2E(t *testing.T) {
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	sshPubKey, err := xssh.NewPublicKey(pubKey)
	if err != nil {
		t.Fatal(err)
	}

	keyring := agent.NewKeyring()
	err = keyring.Add(agent.AddedKey{
		PrivateKey: privKey,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create the socket directory. We keep it in /tmp because of socket path limits.
	socketDir, err := os.MkdirTemp("/tmp", "test-sockets-*")
	if err != nil {
		t.Fatalf("Failed to create socket directory: %v", err)
	}
	defer os.RemoveAll(socketDir)

	// Start ssh agent server.
	sshAuthSock := socketDir + "/ssh-auth"
	agentListener, err := net.ListenUnix("unix", &net.UnixAddr{Name: sshAuthSock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := agentListener.AcceptUnix()
			if err != nil {
				return
			}
			err = agent.ServeAgent(keyring, conn)
			if err != nil && !errors.Is(err, io.EOF) {
				t.Logf("ServeAgent error: %v", err)
			}
			conn.Close()
		}
	}()

	// CloudConfig doesn't really have a rigid spec, so just put things into it
	cloudConfig := make(map[string]any)
	cloudConfig["ssh_authorized_keys"] = []string{
		strings.TrimSuffix(string(xssh.MarshalAuthorizedKey(sshPubKey)), "\n"),
	}
	cloudConfig["disable_root"] = false

	userData, err := json.Marshal(cloudConfig)
	if err != nil {
		t.Fatal(err)
	}

	root := structfs.Tree{
		structfs.File("user-data", structfs.Bytes("#cloud-config\n"+string(userData))),
		structfs.File("meta-data", structfs.Bytes("")),
	}
	cloudInitDataFile, err := os.CreateTemp("", "cidata*.img")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(cloudInitDataFile.Name())
	if err := fat32.WriteFS(cloudInitDataFile, root, fat32.Options{Label: "cidata"}); err != nil {
		t.Fatal(err)
	}

	rootDisk, err := os.CreateTemp("", "rootdisk")
	if err != nil {
		t.Fatal(err)
	}
	// Create a 10GiB sparse root disk
	if err := unix.Ftruncate(int(rootDisk.Fd()), 10*GiB); err != nil {
		t.Fatalf("ftruncate failed: %v", err)
	}

	defer os.Remove(rootDisk.Name())

	sshPort, sshPortCloser, err := freeport.AllocateTCPPort()
	if err != nil {
		t.Fatal(err)
	}

	qemuArgs := []string{
		"-machine", "q35", "-accel", "kvm", "-nographic", "-nodefaults", "-m", "1024",
		"-cpu", "host", "-smp", "sockets=1,cpus=1,cores=2,threads=2,maxcpus=4",
		"-drive", "if=pflash,format=raw,readonly=on,file=" + xOvmfCodePath,
		"-drive", "if=pflash,format=raw,snapshot=on,file=" + xOvmfVarsPath,
		"-drive", "if=none,format=raw,cache=unsafe,id=root,file=" + rootDisk.Name(),
		"-drive", "if=none,format=qcow2,snapshot=on,id=cloud,cache=unsafe,file=" + xCloudImagePath,
		"-device", "virtio-blk-pci,drive=root,bootindex=1",
		"-device", "virtio-blk-pci,drive=cloud,bootindex=2",
		"-drive", "if=virtio,format=raw,snapshot=on,file=" + cloudInitDataFile.Name(),
		"-netdev", fmt.Sprintf("user,id=net0,net=10.42.0.0/24,dhcpstart=10.42.0.10,hostfwd=tcp::%d-:22", sshPort),
		"-device", "virtio-net-pci,netdev=net0,mac=22:d5:8e:76:1d:07",
		"-device", "virtio-rng-pci",
		"-serial", "stdio",
	}
	qemuCmd := exec.Command(xQEMUPath, qemuArgs...)
	stdoutPipe, err := qemuCmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	sshdStarted := make(chan struct{})
	installSucceed := make(chan struct{})
	go func() {
		s := bufio.NewScanner(stdoutPipe)
		for s.Scan() {
			t.Logf("VM: %q", s.Text())
			if strings.Contains(s.Text(), "Started") &&
				strings.Contains(s.Text(), "Secure Shell server") {
				sshdStarted <- struct{}{}
				break
			}
		}
		for s.Scan() {
			t.Logf("VM: %q", s.Text())
			if strings.Contains(s.Text(), "_TESTOS_LAUNCH_SUCCESS_") {
				installSucceed <- struct{}{}
				break
			}
		}
		qemuCmd.Wait()
	}()
	qemuCmd.Stderr = os.Stderr
	sshPortCloser.Close()
	if err := qemuCmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer qemuCmd.Process.Kill()

	select {
	case <-sshdStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("Waiting for sshd start timed out")
	}

	// Create the config directory. We keep it in /tmp because of sandbox limitations.
	configDir, err := os.MkdirTemp("/tmp", "test-configs-*")
	if err != nil {
		t.Fatalf("Failed to create config directory: %v", err)
	}
	defer os.RemoveAll(configDir)

	installArgs := []string{
		"install", "ssh",
		fmt.Sprintf("root@localhost:%d", sshPort),
		"--disk", "vda",
		"--bootstrap",
		"--cluster", "cluster.internal",
		"--takeover", xTakeoverPath,
		"--image", xImagePath,
		"--config", configDir,
	}
	installCmd := exec.Command(xMetroctlPath, installArgs...)
	installCmd.Env = append(installCmd.Environ(), fmt.Sprintf("SSH_AUTH_SOCK=%s", sshAuthSock))
	installCmd.Stdout = os.Stdout
	installCmd.Stderr = os.Stderr
	t.Logf("Running %s", installCmd.String())
	if err := installCmd.Run(); err != nil {
		t.Fatal(err)
	}

	select {
	case <-installSucceed:
		// Done, test passed
	case <-time.After(30 * time.Second):
		t.Fatal("Waiting for installation timed out")
	}
}
