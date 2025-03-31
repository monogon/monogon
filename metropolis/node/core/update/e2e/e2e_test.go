// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"

	"source.monogon.dev/metropolis/installer/install"
	"source.monogon.dev/osbase/blockdev"
	"source.monogon.dev/osbase/oci"
	"source.monogon.dev/osbase/oci/osimage"
	"source.monogon.dev/osbase/oci/registry"
	"source.monogon.dev/osbase/structfs"
)

var (
	// These are filled by bazel at linking time with the canonical path of
	// their corresponding file. Inside the init function we resolve it
	// with the rules_go runfiles package to the real path.
	xImageXPath   string
	xImageYPath   string
	xImageZPath   string
	xOvmfVarsPath string
	xOvmfCodePath string
	xAbloaderPath string
	xQEMUPath     string
)

func init() {
	var err error
	for _, path := range []*string{
		&xImageXPath, &xImageYPath, &xImageZPath,
		&xOvmfVarsPath, &xOvmfCodePath,
		&xAbloaderPath, &xQEMUPath,
	} {
		*path, err = runfiles.Rlocation(*path)
		if err != nil {
			panic(err)
		}
	}
}

var variantRegexp = regexp.MustCompile(`TESTOS_VARIANT=([A-Z])`)

func stdoutHandler(t *testing.T, cmd *exec.Cmd, cancel context.CancelFunc, testosStarted chan string) {
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	s := bufio.NewScanner(stdoutPipe)
	go func() {
		for s.Scan() {
			if strings.HasPrefix(s.Text(), "[") {
				continue
			}
			errIdx := strings.Index(s.Text(), "Error installing new image")
			if errIdx != -1 {
				cancel()
			}
			fmt.Printf("vm: %q\n", s.Text())
			if m := variantRegexp.FindStringSubmatch(s.Text()); len(m) == 2 {
				select {
				case testosStarted <- m[1]:
				default:
				}
			}
		}
	}()
}

func stderrHandler(t *testing.T, cmd *exec.Cmd) {
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	s := bufio.NewScanner(stderrPipe)
	go func() {
		for s.Scan() {
			if strings.HasPrefix(s.Text(), "[") {
				continue
			}
			fmt.Printf("qemu: %q\n", s.Text())
		}
	}()
}

func runAndCheckVariant(t *testing.T, expectedVariant string, qemuArgs []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	qemuCmdLaunch := exec.CommandContext(ctx, xQEMUPath, qemuArgs...)
	testosStarted := make(chan string, 1)
	stdoutHandler(t, qemuCmdLaunch, cancel, testosStarted)
	stderrHandler(t, qemuCmdLaunch)
	if err := qemuCmdLaunch.Start(); err != nil {
		t.Fatal(err)
	}
	procExit := make(chan error)
	go func() {
		procExit <- qemuCmdLaunch.Wait()
		close(procExit)
	}()
	select {
	case variant := <-testosStarted:
		if variant != expectedVariant {
			t.Fatalf("expected variant %s to launch, got %s", expectedVariant, variant)
		}
		select {
		case <-procExit:
		case <-ctx.Done():
			t.Fatal("Timed out waiting for VM to exit")
		}
	case err := <-procExit:
		t.Fatalf("QEMU exited unexpectedly: %v", err)
	case <-ctx.Done():
		t.Fatalf("Waiting for TestOS variant %s launch timed out", expectedVariant)
	}
}

// setup sets up a registry server as well as the initial boot disk
// and EFI variable storage. It also returns the required QEMU arguments.
func setup(t *testing.T) []string {
	t.Helper()
	registryAddr := net.TCPAddr{
		IP:   net.IPv4(10, 42, 0, 5),
		Port: 80,
	}

	imageX, err := oci.ReadLayout(xImageXPath)
	if err != nil {
		t.Fatal(err)
	}
	imageY, err := oci.ReadLayout(xImageYPath)
	if err != nil {
		t.Fatal(err)
	}
	imageZ, err := oci.ReadLayout(xImageZPath)
	if err != nil {
		t.Fatal(err)
	}

	osImageX, err := osimage.Read(imageX)
	if err != nil {
		t.Fatal(err)
	}

	registryServer := registry.NewServer()
	registryServer.AddImage("testos", "y", imageY)
	registryServer.AddImage("testos", "z", imageZ)
	registryLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { registryLis.Close() })
	registryListenAddr := registryLis.Addr().(*net.TCPAddr)
	go http.Serve(registryLis, registryServer)

	rootDevPath := filepath.Join(t.TempDir(), "root.img")
	// Make a 512 bytes * 2Mi = 1Gi file-backed block device
	rootDisk, err := blockdev.CreateFile(rootDevPath, 512, 2097152)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(rootDevPath) })
	defer rootDisk.Close()

	loader, err := structfs.OSPathBlob(xAbloaderPath)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := install.Write(&install.Params{
		Output:   rootDisk,
		OSImage:  osImageX,
		ABLoader: loader,
		PartitionSize: install.PartitionSizeInfo{
			ESP:    128,
			System: 256,
			Data:   10,
		},
	}); err != nil {
		t.Fatalf("unable to generate starting point image: %v", err)
	}

	registryGuestFwd := fmt.Sprintf("guestfwd=tcp:%s-tcp:127.0.0.1:%d", registryAddr.String(), registryListenAddr.Port)

	ovmfVars, err := os.CreateTemp("", "ab-ovmf-vars")
	if err != nil {
		t.Fatal(err)
	}
	defer ovmfVars.Close()
	t.Cleanup(func() { os.Remove(ovmfVars.Name()) })
	ovmfVarsTmpl, err := os.Open(xOvmfVarsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ovmfVarsTmpl.Close()
	if _, err := io.Copy(ovmfVars, ovmfVarsTmpl); err != nil {
		t.Fatal(err)
	}

	qemuArgs := []string{
		"-machine", "q35", "-accel", "kvm", "-nographic", "-nodefaults", "-m", "1024",
		"-cpu", "max", "-smp", "sockets=1,cpus=1,cores=2,threads=2,maxcpus=4",
		"-drive", "if=pflash,format=raw,readonly=on,file=" + xOvmfCodePath,
		"-drive", "if=pflash,format=raw,file=" + ovmfVars.Name(),
		"-drive", "if=virtio,format=raw,cache=unsafe,file=" + rootDevPath,
		"-netdev", fmt.Sprintf("user,id=net0,net=10.42.0.0/24,dhcpstart=10.42.0.10,%s", registryGuestFwd),
		"-device", "virtio-net-pci,netdev=net0,mac=22:d5:8e:76:1d:07",
		"-device", "virtio-rng-pci",
		"-serial", "stdio",
		"-no-reboot",
		"-fw_cfg", "name=opt/testos_y_digest,string=" + imageY.ManifestDigest,
		"-fw_cfg", "name=opt/testos_z_digest,string=" + imageZ.ManifestDigest,
	}
	return qemuArgs
}

func TestABUpdateSequenceReboot(t *testing.T) {
	qemuArgs := setup(t)

	fmt.Println("Launching X image to install Y")
	runAndCheckVariant(t, "X", qemuArgs)

	fmt.Println("Launching Y on slot B to install Z on slot A")
	runAndCheckVariant(t, "Y", qemuArgs)

	fmt.Println("Launching Z on slot A")
	runAndCheckVariant(t, "Z", qemuArgs)
}

func TestABUpdateSequenceKexec(t *testing.T) {
	qemuArgs := setup(t)
	qemuArgs = append(qemuArgs, "-fw_cfg", "name=opt/use_kexec,string=1")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	qemuCmdLaunch := exec.CommandContext(ctx, xQEMUPath, qemuArgs...)
	testosStarted := make(chan string, 1)
	stdoutHandler(t, qemuCmdLaunch, cancel, testosStarted)
	stderrHandler(t, qemuCmdLaunch)
	if err := qemuCmdLaunch.Start(); err != nil {
		t.Fatal(err)
	}
	procExit := make(chan error)
	go func() {
		procExit <- qemuCmdLaunch.Wait()
		close(procExit)
	}()
	var expectedVariant = "X"
	for {
		select {
		case variant := <-testosStarted:
			if variant != expectedVariant {
				t.Fatalf("expected variant %s to launch, got %s", expectedVariant, variant)
			}
			switch expectedVariant {
			case "X":
				expectedVariant = "Y"
			case "Y":
				expectedVariant = "Z"
			case "Z":
				// We're done, wait for everything to wind down and return
				select {
				case <-procExit:
					return
				case <-ctx.Done():
					t.Error("Timed out waiting for VM to exit")
					cancel()
					<-procExit
					return
				}
			}
			fmt.Printf("Got %s, installing %s\n", variant, expectedVariant)
		case err := <-procExit:
			t.Fatalf("QEMU exited unexpectedly: %v", err)
		case <-ctx.Done():
			t.Fatalf("Waiting for TestOS variant %s launch timed out", expectedVariant)
		}
	}
}
