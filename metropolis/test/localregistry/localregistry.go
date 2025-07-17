// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Package localregistry implements a read-only OCI Distribution / Docker
// V2 container image registry backed by local layers.
package localregistry

import (
	"fmt"
	"path"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"google.golang.org/protobuf/encoding/prototext"

	"source.monogon.dev/metropolis/test/localregistry/spec"
	"source.monogon.dev/osbase/oci"
	"source.monogon.dev/osbase/oci/registry"
)

func FromBazelManifest(mb []byte) (*registry.Server, error) {
	var bazelManifest spec.Manifest
	if err := prototext.Unmarshal(mb, &bazelManifest); err != nil {
		return nil, fmt.Errorf("failed to parse manifest: %w", err)
	}
	s := registry.NewServer()
	for _, i := range bazelManifest.Images {
		resolvedPath, err := runfiles.Rlocation(path.Join("_main", i.Path))
		if err != nil {
			return nil, fmt.Errorf("failed to resolve image path %q: %w", i.Path, err)
		}
		image, err := oci.ReadLayout(resolvedPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read image from %q: %w", i.Path, err)
		}
		err = s.AddRef(i.Repository, i.Tag, image)
		if err != nil {
			return nil, fmt.Errorf("failed to add image: %w", err)
		}
	}
	return s, nil
}
