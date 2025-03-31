// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/proto"

	"source.monogon.dev/cloud/agent/api"

	"source.monogon.dev/osbase/fat32"
	"source.monogon.dev/osbase/freeport"
	"source.monogon.dev/osbase/structfs"
)

var (
	// These are filled by bazel at linking time with the canonical path of
	// their corresponding file. Inside the init function we resolve it
	// with the rules_go runfiles package to the real path.
	xCloudImagePath string
	xOvmfVarsPath   string
	xOvmfCodePath   string
	xTakeoverPath   string
	xQEMUPath       string
)

func init() {
	var err error
	for _, path := range []*string{
		&xCloudImagePath, &xOvmfVarsPath, &xOvmfCodePath,
		&xTakeoverPath, &xQEMUPath,
	} {
		*path, err = runfiles.Rlocation(*path)
		if err != nil {
			panic(err)
		}
	}
}

func TestE2E(t *testing.T) {
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	sshPubKey, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		t.Fatal(err)
	}

	sshPrivkey, err := ssh.NewSignerFromKey(privKey)
	if err != nil {
		t.Fatal(err)
	}

	// CloudConfig doesn't really have a rigid spec, so just put things into it
	cloudConfig := make(map[string]any)
	cloudConfig["ssh_authorized_keys"] = []string{
		strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(sshPubKey)), "\n"),
	}

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

	sshPort, sshPortCloser, err := freeport.AllocateTCPPort()
	if err != nil {
		t.Fatal(err)
	}

	qemuArgs := []string{
		"-machine", "q35", "-accel", "kvm", "-nographic", "-nodefaults", "-m", "1024",
		"-cpu", "host", "-smp", "sockets=1,cpus=1,cores=2,threads=2,maxcpus=4",
		"-drive", "if=pflash,format=raw,readonly=on,file=" + xOvmfCodePath,
		"-drive", "if=pflash,format=raw,snapshot=on,file=" + xOvmfVarsPath,
		"-drive", "if=virtio,format=qcow2,snapshot=on,cache=unsafe,file=" + xCloudImagePath,
		"-drive", "if=virtio,format=raw,snapshot=on,file=" + cloudInitDataFile.Name(),
		"-netdev", fmt.Sprintf("user,id=net0,net=10.42.0.0/24,dhcpstart=10.42.0.10,hostfwd=tcp::%d-:22", sshPort),
		"-device", "virtio-net-pci,netdev=net0,mac=22:d5:8e:76:1d:07",
		"-device", "virtio-rng-pci",
		"-serial", "stdio",
		"-no-reboot",
	}
	qemuCmd := exec.Command(xQEMUPath, qemuArgs...)
	stdoutPipe, err := qemuCmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	agentStarted := make(chan struct{})
	go func() {
		s := bufio.NewScanner(stdoutPipe)
		for s.Scan() {
			t.Log("kernel: " + s.Text())
			if strings.Contains(s.Text(), "Monogon BMaaS Agent started") {
				agentStarted <- struct{}{}
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

	var c *ssh.Client
	for {
		c, err = ssh.Dial("tcp", net.JoinHostPort("localhost", fmt.Sprintf("%d", sshPort)), &ssh.ClientConfig{
			User:            "debian",
			Auth:            []ssh.AuthMethod{ssh.PublicKeys(sshPrivkey)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			Timeout:         5 * time.Second,
		})
		if err != nil {
			t.Logf("error connecting via SSH, retrying: %v", err)
			time.Sleep(1 * time.Second)
			continue
		}
		break
	}
	defer c.Close()
	sc, err := sftp.NewClient(c)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	takeoverFile, err := sc.Create("takeover")
	if err != nil {
		t.Fatal(err)
	}
	defer takeoverFile.Close()
	if err := takeoverFile.Chmod(0o755); err != nil {
		t.Fatal(err)
	}
	takeoverSrcFile, err := os.Open(xTakeoverPath)
	if err != nil {
		t.Fatal(err)
	}
	defer takeoverSrcFile.Close()

	if _, err := io.Copy(takeoverFile, takeoverSrcFile); err != nil {
		t.Fatal(err)
	}
	if err := takeoverFile.Close(); err != nil {
		t.Fatal(err)
	}
	sc.Close()

	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	init := api.TakeoverInit{
		MachineId:     "test",
		BmaasEndpoint: "localhost:1234",
	}
	initRaw, err := proto.Marshal(&init)
	if err != nil {
		t.Fatal(err)
	}
	sess.Stdin = bytes.NewReader(initRaw)
	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	sess.Stdout = &stdoutBuf
	sess.Stderr = &stderrBuf
	if err := sess.Run("sudo ./takeover"); err != nil {
		t.Errorf("stderr:\n%s\n\n", stderrBuf.String())
		t.Fatal(err)
	}
	var resp api.TakeoverResponse
	if err := proto.Unmarshal(stdoutBuf.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	switch res := resp.Result.(type) {
	case *api.TakeoverResponse_Success:
		if res.Success.InitMessage.BmaasEndpoint != init.BmaasEndpoint {
			t.Fatal("InitMessage not passed through properly")
		}
	case *api.TakeoverResponse_Error:
		t.Fatalf("takeover returned error: %v", res.Error.Message)
	}
	select {
	case <-agentStarted:
		// Done, test passed
	case <-time.After(30 * time.Second):
		t.Fatal("Waiting for BMaaS agent startup timed out")
	}
}
