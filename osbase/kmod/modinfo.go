// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package kmod

import (
	"bufio"
	"bytes"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ModuleInfo contains Linux kernel module metadata. It maps keys to a list
// of values. For known keys accessor functions are provided.
type ModuleInfo map[string][]string

// Get returns the first value of the given key or an empty string if the key
// does not exist.
func (i ModuleInfo) Get(key string) string {
	if len(i[key]) > 0 {
		return i[key][0]
	}
	return ""
}

// Name returns the name of the module as defined in kbuild.
func (i ModuleInfo) Name() string {
	return i.Get("name")
}

// Authors returns the list of a authors of the module.
func (i ModuleInfo) Authors() []string {
	return i["author"]
}

// Description returns a human-readable description of the module or an empty
// string if it is not available.
func (i ModuleInfo) Description() string {
	return i.Get("description")
}

// GetDependencies returns a list of module names which need to be loaded
// before this one.
func (i ModuleInfo) GetDependencies() []string {
	if len(i["depends"]) == 1 && i["depends"][0] == "" {
		return nil
	}
	out := make([]string, 0, len(i["depends"]))
	for _, dep := range i["depends"] {
		parts := strings.Split(dep, ",")
		for _, part := range parts {
			out = append(out, strings.ReplaceAll(part, "-", "_"))
		}
	}
	return out
}

type OptionalDependencies struct {
	// Pre contains a list of module names to be optionally loaded before the
	// module itself.
	Pre []string
	// Post contains a list of module names to be optionally loaded after the
	// module itself.
	Post []string
}

// GetOptionalDependencies returns a set of modules which are recommended to
// be loaded before and after this module. These are optional, but enhance
// the functionality of this module.
func (i ModuleInfo) GetOptionalDependencies() OptionalDependencies {
	var out OptionalDependencies
	for _, s := range i["softdep"] {
		tokens := strings.Fields(s)
		const (
			MODE_IDLE = 0
			MODE_PRE  = 1
			MODE_POST = 2
		)
		var state = MODE_IDLE
		for _, token := range tokens {
			switch token {
			case "pre:":
				state = MODE_PRE
			case "post:":
				state = MODE_POST
			default:
				switch state {
				case MODE_PRE:
					out.Pre = append(out.Pre, token)
				case MODE_POST:
					out.Post = append(out.Post, token)
				default:
				}
			}
		}
	}
	return out
}

// Aliases returns a list of match expressions for matching devices handled
// by this module. Every returned string consists of a literal as well as '*'
// wildcards matching one or more characters. These should be matched against
// the kobject MODALIAS field or the modalias sysfs file.
func (i ModuleInfo) Aliases() []string {
	return i["alias"]
}

// Firmware returns a list of firmware file paths required by this module.
// These paths are usually relative to the root of a linux-firmware install
// unless the firmware is non-redistributable.
func (i ModuleInfo) Firmware() []string {
	return i["firmware"]
}

// Licenses returns the licenses use of this module is governed by.
// For mainline modules, the list of valid license strings is
// documented in the kernel's Documentation/process/license-rules.rst file
// under the `MODULE_LICENSE` section.
func (i ModuleInfo) Licenses() []string {
	return i["license"]
}

// IsInTree returns true if the module was built in the Linux source tree and
// not externally. This does not necessarily mean that the module is in the
// mainline kernel.
func (i ModuleInfo) IsInTree() bool {
	return i.Get("intree") == "Y"
}

// vermagic and retpoline are intentionally not exposed here, if you need them
// you should know how to get them out of the map yourself as AFAIK these
// are not a stable interface and most programs should not process them.

func nullByteSplit(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, 0x00); i >= 0 {
		return i + 1, bytes.TrimLeft(data[0:i], "\x00"), nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// GetModuleInfo looks for a ".modinfo" section in the passed ELF Linux kernel
// module and parses it into a ModuleInfo structure.
func GetModuleInfo(e *elf.File) (ModuleInfo, error) {
	for _, section := range e.Sections {
		if section.Name == ".modinfo" {
			out := make(ModuleInfo)
			s := bufio.NewScanner(io.NewSectionReader(section, 0, int64(section.Size)))
			s.Split(nullByteSplit)

			for s.Scan() {
				// Format is <key>=<value>
				key, value, ok := bytes.Cut(s.Bytes(), []byte("="))
				if !ok {
					continue
				}
				keyStr := string(key)
				out[keyStr] = append(out[keyStr], string(value))
			}
			return out, nil
		}
	}
	return nil, errors.New("no .modinfo section found")
}

// GetBuiltinModulesInfo parses all modinfo structures for builtin modules from
// a modinfo file (modules.builtin.modinfo).
func GetBuiltinModulesInfo(f io.Reader) ([]ModuleInfo, error) {
	var out []ModuleInfo
	s := bufio.NewScanner(f)
	s.Split(nullByteSplit)

	currModule := make(ModuleInfo)
	for s.Scan() {
		if s.Err() != nil {
			return nil, fmt.Errorf("failed scanning for next token: %w", s.Err())
		}
		// Format is <module>.<key>=<value>
		modName, entry, ok := bytes.Cut(s.Bytes(), []byte{'.'})
		if !ok {
			continue
		}
		if string(modName) != currModule.Name() {
			if currModule.Name() != "" {
				out = append(out, currModule)
			}
			currModule = make(ModuleInfo)
			currModule["name"] = []string{string(modName)}
		}
		key, value, ok := bytes.Cut(entry, []byte("="))
		if !ok {
			continue
		}
		keyStr := string(key)
		currModule[keyStr] = append(currModule[keyStr], string(value))
	}
	if currModule.Name() != "" {
		out = append(out, currModule)
	}
	return out, nil
}
