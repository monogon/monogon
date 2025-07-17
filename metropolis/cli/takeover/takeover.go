// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/cavaliergopher/cpio"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	apb "source.monogon.dev/metropolis/proto/api"
	netapi "source.monogon.dev/osbase/net/proto"

	"source.monogon.dev/metropolis/installer/install"
	"source.monogon.dev/osbase/bootparam"
	"source.monogon.dev/osbase/kexec"
	netdump "source.monogon.dev/osbase/net/dump"
	"source.monogon.dev/osbase/oci"
	"source.monogon.dev/osbase/structfs"
)

//go:embed third_party/linux/Image
var kernel []byte

//go:embed third_party/ucode.cpio
var ucode []byte

//go:embed initramfs.cpio.zst
var initramfs []byte

// newMemfile creates a new file which is not located on a specific filesystem,
// but is instead backed by anonymous memory.
func newMemfile(name string, flags int) (*os.File, error) {
	fd, err := unix.MemfdCreate(name, flags)
	if err != nil {
		return nil, fmt.Errorf("memfd_create failed: %w", err)
	}
	return os.NewFile(uintptr(fd), name), nil
}

func writeCPIO(w io.Writer, root structfs.Tree) error {
	cpioW := cpio.NewWriter(w)
	for path, node := range root.Walk() {
		switch {
		case node.Mode.IsDir():
			err := cpioW.WriteHeader(&cpio.Header{
				Name: "/" + path,
				Mode: cpio.TypeDir | (cpio.FileMode(node.Mode) & cpio.ModePerm),
			})
			if err != nil {
				return err
			}
		case node.Mode.IsRegular():
			err := cpioW.WriteHeader(&cpio.Header{
				Name: "/" + path,
				Size: node.Content.Size(),
				Mode: cpio.TypeReg | (cpio.FileMode(node.Mode) & cpio.ModePerm),
			})
			if err != nil {
				return err
			}
			content, err := node.Content.Open()
			if err != nil {
				return fmt.Errorf("cpio write %q: %w", path, err)
			}
			_, err = io.Copy(cpioW, content)
			content.Close()
			if err != nil {
				return fmt.Errorf("cpio write %q: %w", path, err)
			}
		default:
			return fmt.Errorf("cpio write %q: unsupported file type %s", path, node.Mode.Type().String())
		}
	}
	return cpioW.Close()
}

func setupTakeover(nodeParamsRaw []byte, target string) ([]string, error) {
	// Validate we are running via EFI.
	if _, err := os.Stat("/sys/firmware/efi"); os.IsNotExist(err) {
		//nolint:ST1005
		return nil, fmt.Errorf("Monogon OS can only be installed on EFI-booted machines, this one is not")
	}

	currPath, err := os.Executable()
	if err != nil {
		return nil, err
	}

	image, err := oci.AsImage(oci.ReadLayout(filepath.Join(filepath.Dir(currPath), "osimage")))
	if err != nil {
		return nil, fmt.Errorf("failed to read OS image: %w", err)
	}

	// Dump the current network configuration
	netconf, warnings, err := netdump.Dump()
	if err != nil {
		return nil, fmt.Errorf("failed to dump network configuration: %w", err)
	}

	if len(netconf.Nameserver) == 0 {
		netconf.Nameserver = []*netapi.Nameserver{{
			Ip: "8.8.8.8",
		}, {
			Ip: "1.1.1.1",
		}}
	}

	var params apb.NodeParameters
	if err := proto.Unmarshal(nodeParamsRaw, &params); err != nil {
		return nil, fmt.Errorf("failed to unmarshal node parameters: %w", err)
	}

	// Override the NodeParameters.NetworkConfig with the current NetworkConfig
	// if it's missing.
	if params.NetworkConfig == nil {
		params.NetworkConfig = netconf
	}

	// Marshal NodeParameters again.
	nodeParamsRaw, err = proto.Marshal(&params)
	if err != nil {
		return nil, fmt.Errorf("failed marshaling: %w", err)
	}

	oParams, err := setupOSImageParams(image, nodeParamsRaw, target)
	if err != nil {
		return nil, err
	}

	// Validate that this installation will not fail because of disk issues
	if _, err := install.Plan(oParams); err != nil {
		return nil, fmt.Errorf("failed to plan installation: %w", err)
	}

	// Load data from embedded files into memfiles as the kexec load syscall
	// requires file descriptors.
	kernelFile, err := newMemfile("kernel", 0)
	if err != nil {
		return nil, fmt.Errorf("failed to create kernel memfile: %w", err)
	}
	initramfsFile, err := newMemfile("initramfs", 0)
	if err != nil {
		return nil, fmt.Errorf("failed to create initramfs memfile: %w", err)
	}
	if _, err := kernelFile.Write(kernel); err != nil {
		return nil, fmt.Errorf("failed to write kernel into memory-backed file: %w", err)
	}
	if _, err := initramfsFile.Write(ucode); err != nil {
		return nil, fmt.Errorf("failed to write ucode into memory-backed file: %w", err)
	}
	if _, err := initramfsFile.Write(initramfs); err != nil {
		return nil, fmt.Errorf("failed to write initramfs into memory-backed file: %w", err)
	}

	// Append this executable, node params and OS image to initramfs.
	self, err := structfs.OSPathBlob("/proc/self/exe")
	if err != nil {
		return nil, err
	}
	imageLayout, err := oci.CreateLayout(image)
	if err != nil {
		return nil, err
	}
	root := structfs.Tree{
		structfs.File("init", self, structfs.WithPerm(0o755)),
		structfs.File("params.pb", structfs.Bytes(nodeParamsRaw)),
		structfs.Dir("osimage", imageLayout),
	}
	compressedW, err := zstd.NewWriter(initramfsFile, zstd.WithEncoderLevel(1))
	if err != nil {
		return nil, fmt.Errorf("while creating zstd writer: %w", err)
	}
	err = writeCPIO(compressedW, root)
	if err != nil {
		return nil, err
	}
	err = compressedW.Close()
	if err != nil {
		return nil, err
	}

	initParams := bootparam.Params{
		bootparam.Param{Param: "quiet"},
		bootparam.Param{Param: launchModeEnv, Value: launchModeInit},
		bootparam.Param{Param: EnvInstallTarget, Value: target},
		bootparam.Param{Param: "init", Value: "/init"},
	}

	var customConsoles bool
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		warnings = append(warnings, fmt.Errorf("unable to read current kernel command line: %w", err))
	} else {
		params, _, err := bootparam.Unmarshal(string(cmdline))
		// If the existing command line is well-formed, add all existing console
		// parameters to the console for the agent
		if err == nil {
			for _, p := range params {
				if p.Param == "console" {
					initParams = append(initParams, p)
					customConsoles = true
				}
			}
		}
	}
	if !customConsoles {
		// Add the "default" console on x86
		initParams = append(initParams, bootparam.Param{Param: "console", Value: "ttyS0,115200"})
	}
	agentCmdline, err := bootparam.Marshal(initParams, "")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal bootparams: %w", err)
	}
	// Stage agent payload into kernel memory
	if err := kexec.FileLoad(kernelFile, initramfsFile, agentCmdline); err != nil {
		return nil, fmt.Errorf("failed to load kexec payload: %w", err)
	}
	var warningsStrs []string
	for _, w := range warnings {
		warningsStrs = append(warningsStrs, w.Error())
	}
	return warningsStrs, nil
}
