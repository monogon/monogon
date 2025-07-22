// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package kmod

import (
	"fmt"

	kmodpb "source.monogon.dev/osbase/kmod/spec"
)

// MakeMetaFromModuleInfo is a more flexible alternative to MakeMeta. It only
// relies on ModuleInfo structures, additional processing can be done outside of
// this function. It does however require that for dynamically-loaded modules
// the "path" key is set to the path of the .ko file relative to the module
// root.
func MakeMetaFromModuleInfo(modinfos []ModuleInfo) (*kmodpb.Meta, error) {
	modIndices := make(map[string]uint32)
	modInfoMap := make(map[string]ModuleInfo)
	var meta kmodpb.Meta
	meta.ModuleDeviceMatches = &kmodpb.RadixNode{
		Type: kmodpb.RadixNode_TYPE_ROOT,
	}
	var i uint32
	for _, m := range modinfos {
		meta.Modules = append(meta.Modules, &kmodpb.Module{
			Name: m.Name(),
			Path: m.Get("path"),
		})
		for _, p := range m.Aliases() {
			if m.Get("path") == "" {
				// Ignore built-in modaliases as they do not need to be loaded.
				continue
			}
			if err := AddPattern(meta.ModuleDeviceMatches, p, i); err != nil {
				return nil, fmt.Errorf("failed adding device match %q: %w", p, err)
			}
		}
		modIndices[m.Name()] = i
		modInfoMap[m.Name()] = m
		i++
	}
	for _, m := range meta.Modules {
		for _, dep := range modInfoMap[m.Name].GetDependencies() {
			if _, ok := modIndices[dep]; !ok {
				return nil, fmt.Errorf("unknown dependency %q for module %q", dep, m.Name)
			}
			m.Depends = append(m.Depends, modIndices[dep])
		}
	}
	return &meta, nil
}
