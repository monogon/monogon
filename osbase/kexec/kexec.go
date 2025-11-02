// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build amd64 || arm64 || riscv64

// Package kexec allows executing subsequent kernels from Linux userspace.
package kexec

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// FileLoad loads the given kernel as the new kernel with the given initramfs
// and cmdline. It also performs auxiliary work like adding the ACPI RSDP
// physical address to command line if using EFI. The kernel can be started by
// calling unix.Reboot(unix.LINUX_REBOOT_CMD_KEXEC).
// The underlying syscall is only available on x86_64, arm64 and riscv.
// Parts of this function are taken from u-root's kexec package.
func FileLoad(kernel, initramfs *os.File, cmdline string) error {
	passedCmdline := cmdline
	systab, err := os.Open("/sys/firmware/efi/systab")
	if os.IsNotExist(err) {
		// No EFI, nothing to do
	} else if err != nil {
		return fmt.Errorf("unable to open EFI systab: %w", err)
	} else {
		s := bufio.NewScanner(systab)
		for s.Scan() {
			if errors.Is(s.Err(), io.EOF) {
				// We have no RSDP, no need to pass it
				break
			}
			if s.Err() != nil {
				return fmt.Errorf("failed to read EFI systab: %w", s.Err())
			}
			parts := strings.SplitN(s.Text(), "=", 2)
			// There are two ACPI RDSP revisions, 1.0 and 2.0.
			// Linux guarantees that the 2.0 always comes before the
			// 1.0 so just matching and breaking is good enough.
			if parts[0] == "ACPI20" || parts[0] == "ACPI" {
				// Technically this could be passed through as parsing a hexa-
				// decimal address and printing it back does nothing, but in
				// case unexpected values show up this could cause very hard-
				// to-debug crashes when the new kernel boots.
				var acpiRsdp int64
				if _, err := fmt.Sscanf(parts[1], "0x%x", &acpiRsdp); err != nil {
					return fmt.Errorf("failed to parse EFI systab ACP RSDP address: %w", err)
				}
				passedCmdline += fmt.Sprintf(" acpi_rsdp=0x%x", acpiRsdp)
				break
			}
		}
	}

	var flags int
	var initramfsfd int
	if initramfs != nil {
		initramfsfd = int(initramfs.Fd())
	} else {
		flags |= unix.KEXEC_FILE_NO_INITRAMFS
	}

	if err := unix.KexecFileLoad(int(kernel.Fd()), initramfsfd, passedCmdline, flags); err != nil {
		return fmt.Errorf("SYS_kexec_file_load(%d, %d, %s, %x) = %w", kernel.Fd(), initramfsfd, cmdline, flags, err)
	}
	runtime.KeepAlive(kernel)
	runtime.KeepAlive(initramfs)
	return nil
}
