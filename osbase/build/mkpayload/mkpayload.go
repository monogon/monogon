// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// mkpayload is an objcopy wrapper that builds EFI unified kernel images. It
// performs actions that can't be realized by either objcopy or the
// buildsystem.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
)

type stringList []string

func (l *stringList) String() string {
	if l == nil {
		return ""
	}
	return strings.Join(*l, ", ")
}

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

type section struct {
	name     string
	descr    string
	required bool
	file     *string
}

var (
	// sections contains VMAs and source files of the payload PE sections. The
	// file path pointers will be filled in when the flags are parsed. It's used
	// to generate objcopy command line arguments. Entries that are "required"
	// will cause the program to stop and print usage information if not provided
	// as command line parameters.
	sections = []section{
		{"linux", "Linux kernel image", true, nil},
		{"initrd", "initramfs", false, nil},
		{"osrel", "OS release file in text format", false, nil},
		{"cmdline", "a file containting additional kernel command line parameters", false, nil},
		{"splash", "a splash screen image in BMP format", false, nil},
	}
	initrdList      stringList
	objcopy         = flag.String("objcopy", "", "objcopy executable")
	stub            = flag.String("stub", "", "the EFI stub executable")
	output          = flag.String("output", "", "objcopy output")
	rootfs_dm_table = flag.String("rootfs_dm_table", "", "a text file containing the DeviceMapper rootfs target table")
)

func getSection(name string) *section {
	for i := range sections {
		s := &sections[i]
		if s.name == name {
			return s
		}
	}
	return nil
}

func main() {
	flag.Var(&initrdList, "initrd", "Path to initramfs, can be given multiple times")
	// Register parameters related to the EFI payload sections, then parse the flags.
	for i := range sections {
		s := &sections[i]
		if s.name == "initrd" { // initrd is special because it accepts multiple payloads
			continue
		}
		s.file = flag.String(s.name, "", s.descr)
	}
	flag.Parse()

	// Ensure all the required parameters are filled in.
	for _, s := range sections {
		if s.required && *s.file == "" {
			log.Fatalf("-%s parameter is missing.", s.name)
		}
	}
	if *objcopy == "" {
		log.Fatalf("-objcopy parameter is missing.")
	}
	if *stub == "" {
		log.Fatalf("-stub parameter is missing.")
	}
	if *output == "" {
		log.Fatalf("-output parameter is missing.")
	}

	// If a DeviceMapper table was passed, configure the kernel to boot from the
	// device described by it, while keeping any other kernel command line
	// parameters that might have been passed through "-cmdline".
	if *rootfs_dm_table != "" {
		var cmdline string
		p := *getSection("cmdline").file
		if p != "" {
			c, err := os.ReadFile(p)
			if err != nil {
				log.Fatalf("%v", err)
			}
			cmdline = string(c[:])

			if strings.Contains(cmdline, "root=") {
				log.Fatalf("A DeviceMapper table was passed, however the kernel command line already contains a \"root=\" statement.")
			}
		}

		vt, err := os.ReadFile(*rootfs_dm_table)
		if err != nil {
			log.Fatalf("%v", err)
		}
		cmdline += fmt.Sprintf(" dm-mod.create=\"rootfs,,,ro,%s\" root=/dev/dm-0", vt)

		out, err := os.CreateTemp(".", "cmdline")
		if err != nil {
			log.Fatalf("%v", err)
		}
		defer os.Remove(out.Name())
		if _, err = out.Write([]byte(cmdline[:])); err != nil {
			log.Fatalf("%v", err)
		}
		out.Close()

		*getSection("cmdline").file = out.Name()
	}

	var initrdPath string
	if len(initrdList) > 0 {
		initrd, err := os.CreateTemp(".", "initrd")
		if err != nil {
			log.Fatalf("Failed to create temporary initrd: %v", err)
		}
		defer os.Remove(initrd.Name())
		for _, initrdPath := range initrdList {
			initrdSrc, err := os.Open(initrdPath)
			if err != nil {
				log.Fatalf("Failed to open initrd file: %v", err)
			}
			if _, err := io.Copy(initrd, initrdSrc); err != nil {
				initrdSrc.Close()
				log.Fatalf("Failed concatinating initrd: %v", err)
			}
			initrdSrc.Close()
		}
		initrdPath = initrd.Name()
	}
	getSection("initrd").file = &initrdPath

	// Execute objcopy
	var args []string
	for _, s := range sections {
		if *s.file != "" {
			args = append(args,
				"--add-section", fmt.Sprintf(".%s=%s", s.name, *s.file),
				fmt.Sprintf("--set-section-flags=.%s=data", s.name),
			)
		}
	}
	args = append(args,
		*stub,
		*output,
	)
	cmd := exec.Command(*objcopy, args...)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	err := cmd.Run()
	if err == nil {
		return
	}
	// Exit with objcopy's return code.
	var e *exec.ExitError
	if errors.As(err, &e) {
		os.Exit(e.ExitCode())
	}
	log.Fatalf("Could not start command: %v", err)
}
