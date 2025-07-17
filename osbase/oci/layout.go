// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go"
	ocispecv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"source.monogon.dev/osbase/structfs"
)

// ReadLayoutIndex reads the index from an OS path to an OCI layout directory.
func ReadLayoutIndex(path string) (*Index, error) {
	// Read the oci-layout marker file.
	layoutBytes, err := os.ReadFile(filepath.Join(path, "oci-layout"))
	if err != nil {
		return nil, err
	}
	layout := ocispecv1.ImageLayout{}
	err = json.Unmarshal(layoutBytes, &layout)
	if err != nil {
		return nil, fmt.Errorf("failed to parse oci-layout: %w", err)
	}
	if layout.Version != "1.0.0" {
		return nil, fmt.Errorf("unknown oci-layout version %q", layout.Version)
	}

	// Read the index.
	indexBytes, err := os.ReadFile(filepath.Join(path, "index.json"))
	if err != nil {
		return nil, err
	}
	blobs := &layoutBlobs{path: path}
	ref, err := NewRef(indexBytes, ocispecv1.MediaTypeImageIndex, "", blobs)
	if err != nil {
		return nil, err
	}
	return ref.(*Index), nil
}

// ReadLayout reads a manifest from an OS path to an OCI layout directory.
// It expects the index to point to exactly one manifest, which is common.
func ReadLayout(path string) (Ref, error) {
	index, err := ReadLayoutIndex(path)
	if err != nil {
		return nil, err
	}

	if len(index.Manifest.Manifests) == 0 {
		return nil, fmt.Errorf("index.json contains no manifests")
	}
	if len(index.Manifest.Manifests) != 1 {
		return nil, fmt.Errorf("index.json files containing multiple manifests are not supported")
	}
	return index.Ref(&index.Manifest.Manifests[0])
}

type layoutBlobs struct {
	path string
}

func (r *layoutBlobs) Blob(descriptor *ocispecv1.Descriptor) (io.ReadCloser, error) {
	algorithm, encoded, err := ParseDigest(string(descriptor.Digest))
	if err != nil {
		return nil, fmt.Errorf("failed to parse digest in descriptor: %w", err)
	}
	return os.Open(filepath.Join(r.path, "blobs", algorithm, encoded))
}

func (r *layoutBlobs) Manifest(descriptor *ocispecv1.Descriptor) ([]byte, error) {
	blob, err := r.Blob(descriptor)
	if err != nil {
		return nil, err
	}
	defer blob.Close()
	manifestBytes := make([]byte, descriptor.Size)
	_, err = io.ReadFull(blob, manifestBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest: %w", err)
	}
	return manifestBytes, nil
}

func (r *layoutBlobs) Blobs(_ *ocispecv1.Descriptor) (Blobs, error) {
	return r, nil
}

// CreateLayout builds an OCI layout from a Ref.
func CreateLayout(ref Ref) (structfs.Tree, error) {
	// Build the index.
	artifactType := ""
	if image, ok := ref.(*Image); ok {
		// According to the OCI spec, the artifactType is the config descriptor
		// mediaType, and is only set when the descriptor references the image
		// manifest of an artifact.
		artifactType = image.Manifest.Config.MediaType
		if artifactType == ocispecv1.MediaTypeImageConfig {
			artifactType = ""
		}
	}
	imageIndex := ocispecv1.Index{
		Versioned: ocispec.Versioned{SchemaVersion: 2},
		MediaType: ocispecv1.MediaTypeImageIndex,
		Manifests: []ocispecv1.Descriptor{{
			MediaType:    ocispecv1.MediaTypeImageManifest,
			ArtifactType: artifactType,
			Digest:       digest.Digest(ref.Digest()),
			Size:         int64(len(ref.RawManifest())),
		}},
	}
	imageIndexBytes, err := json.MarshalIndent(imageIndex, "", "\t")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal image index: %w", err)
	}
	imageIndexBytes = append(imageIndexBytes, '\n')

	root := structfs.Tree{
		structfs.File("oci-layout", structfs.Bytes(`{"imageLayoutVersion": "1.0.0"}`+"\n")),
		structfs.File("index.json", structfs.Bytes(imageIndexBytes)),
	}

	hasBlob := make(map[string]bool)
	blobDirs := make(map[string]*structfs.Node)
	addBlob := func(digest string, blob structfs.Blob) error {
		if hasBlob[digest] {
			// If multiple blobs have the same digest, we only need the first one.
			return nil
		}
		hasBlob[digest] = true
		algorithm, encoded, err := ParseDigest(digest)
		if err != nil {
			return fmt.Errorf("failed to parse manifest digest: %w", err)
		}
		blobDir, ok := blobDirs[algorithm]
		if !ok {
			blobDir = structfs.Dir(algorithm, nil)
			err = root.Place("blobs", blobDir)
			if err != nil {
				return err
			}
			blobDirs[algorithm] = blobDir
		}
		// root.PlaceFile is not used here because then running time would be
		// quadratic in the number of blobs.
		blobDir.Children = append(blobDir.Children, structfs.File(encoded, blob))
		return nil
	}
	err = WalkRefs(string(imageIndex.Manifests[0].Digest), ref, func(digest string, ref Ref) error {
		err := addBlob(digest, structfs.Bytes(ref.RawManifest()))
		if err != nil {
			return err
		}
		if image, ok := ref.(*Image); ok {
			for descriptor := range image.Descriptors() {
				err := addBlob(string(descriptor.Digest), image.StructfsBlob(descriptor))
				if err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return root, nil
}
