// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Package oci contains tools for handling OCI images.
package oci

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"strings"

	ocispecv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"source.monogon.dev/osbase/structfs"
)

// Index represents an OCI image index.
type Index struct {
	// Manifest contains the parsed index manifest.
	Manifest    *ocispecv1.Index
	rawManifest []byte
	digest      string
	blobs       Blobs
}

// Image represents an OCI image.
type Image struct {
	// Manifest contains the parsed image manifest.
	Manifest    *ocispecv1.Manifest
	rawManifest []byte
	digest      string
	blobs       Blobs
}

// Ref is either an [*Index] or [*Image].
type Ref interface {
	// RawManifest returns the bytes of the manifest.
	// The returned value is shared and must not be modified.
	RawManifest() []byte
	// Digest returns the computed digest of RawManifest, in the default digest
	// algorithm. Only sha256 is supported currently.
	Digest() string
	// MediaType returns the media type of the manifest.
	MediaType() string
	// isRef is an unexported marker to disallow implementations of the interface
	// outside this package.
	isRef()
}

func (i *Index) RawManifest() []byte { return i.rawManifest }
func (i *Index) Digest() string      { return i.digest }
func (i *Index) MediaType() string   { return ocispecv1.MediaTypeImageIndex }
func (i *Index) isRef()              {}

func (i *Image) RawManifest() []byte { return i.rawManifest }
func (i *Image) Digest() string      { return i.digest }
func (i *Image) MediaType() string   { return ocispecv1.MediaTypeImageManifest }
func (i *Image) isRef()              {}

// Blobs is the interface which image sources implement
// to retrieve the content of blobs and manifests.
type Blobs interface {
	// Blob returns the contents of a blob from its descriptor.
	// It does not verify the contents against the digest.
	//
	// This is only called on images.
	Blob(*ocispecv1.Descriptor) (io.ReadCloser, error)
	// Manifest returns the contents of a manifest from its descriptor.
	// It does not verify the contents against the digest.
	//
	// This is only called on indexes.
	Manifest(*ocispecv1.Descriptor) ([]byte, error)
	// Blobs returns the [Blobs] for the manifest from its descriptor.
	// Most implementations simply return the receiver itself, but this
	// allows combining Refs from different sources into an Index.
	//
	// This is only called on indexes.
	Blobs(*ocispecv1.Descriptor) (Blobs, error)
}

// NewRef verifies the manifest against the expected digest if not empty,
// then parses it according to mediaType and returns a [Ref].
func NewRef(rawManifest []byte, mediaType string, expectedDigest string, blobs Blobs) (Ref, error) {
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(rawManifest))
	if expectedDigest != "" && expectedDigest != digest {
		if _, _, err := ParseDigest(expectedDigest); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("failed verification of manifest: expected digest %q, computed %q", expectedDigest, digest)
	}

	switch mediaType {
	case ocispecv1.MediaTypeImageManifest:
		manifest := &ocispecv1.Manifest{}
		if err := json.Unmarshal(rawManifest, manifest); err != nil {
			return nil, fmt.Errorf("failed to parse image manifest: %w", err)
		}
		if manifest.MediaType != ocispecv1.MediaTypeImageManifest {
			return nil, fmt.Errorf("unexpected manifest media type %q, expected %q", manifest.MediaType, ocispecv1.MediaTypeImageManifest)
		}
		image := &Image{
			Manifest:    manifest,
			rawManifest: rawManifest,
			digest:      digest,
			blobs:       blobs,
		}
		for descriptor := range image.Descriptors() {
			// We validate this here such that StructfsBlob does not need an error return.
			if descriptor.Size < 0 {
				return nil, fmt.Errorf("invalid manifest: contains descriptor with negative size")
			}
		}
		return image, nil
	case ocispecv1.MediaTypeImageIndex:
		manifest := &ocispecv1.Index{}
		if err := json.Unmarshal(rawManifest, manifest); err != nil {
			return nil, fmt.Errorf("failed to parse index manifest: %w", err)
		}
		if manifest.MediaType != ocispecv1.MediaTypeImageIndex {
			return nil, fmt.Errorf("unexpected manifest media type %q, expected %q", manifest.MediaType, ocispecv1.MediaTypeImageIndex)
		}
		index := &Index{
			Manifest:    manifest,
			rawManifest: rawManifest,
			digest:      digest,
			blobs:       blobs,
		}
		return index, nil
	default:
		return nil, fmt.Errorf("unknown manifest media type %q", mediaType)
	}
}

// AsImage can be conveniently wrapped around a call which returns a [Ref] or
// error, when only [*Image] can be handled.
func AsImage(ref Ref, err error) (*Image, error) {
	if err != nil {
		return nil, err
	}
	image, ok := ref.(*Image)
	if !ok {
		return nil, fmt.Errorf("unexpected manifest media type %q, only image is supported", ref.MediaType())
	}
	return image, nil
}

// WalkRefs iterates over all Refs reachable from ref in DFS post-order.
// Each digest is only visited once, even if reachable multiple times.
//
// For each Ref, we also pass the digest by which it is referenced. This may be
// different from ref.Digest() if we ever support multiple digest algorithms.
func WalkRefs(digest string, ref Ref, fn func(digest string, ref Ref) error) error {
	visited := make(map[string]bool)
	return walkRefs(digest, ref, fn, visited)
}

func walkRefs(digest string, ref Ref, fn func(digest string, ref Ref) error, visited map[string]bool) error {
	if visited[digest] {
		return nil
	}
	visited[digest] = true
	switch ref := ref.(type) {
	case *Image:
	case *Index:
		for i := range ref.Manifest.Manifests {
			descriptor := &ref.Manifest.Manifests[i]
			childRef, err := ref.Ref(descriptor)
			if err != nil {
				return err
			}
			err = walkRefs(string(descriptor.Digest), childRef, fn, visited)
			if err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unknown manifest media type %q", ref.MediaType())
	}
	return fn(digest, ref)
}

// Ref reads a manifest from its descriptor and wraps it in a [Ref].
// The manifest is verified against the digest.
func (i *Index) Ref(descriptor *ocispecv1.Descriptor) (Ref, error) {
	if descriptor.Size < 0 {
		return nil, fmt.Errorf("invalid descriptor size %d", descriptor.Size)
	}
	if descriptor.Size > 50*1024*1024 {
		return nil, fmt.Errorf("refusing to read manifest of size %d into memory", descriptor.Size)
	}
	switch descriptor.MediaType {
	case ocispecv1.MediaTypeImageManifest:
	case ocispecv1.MediaTypeImageIndex:
	default:
		return nil, fmt.Errorf("unknown manifest media type %q", descriptor.MediaType)
	}
	if descriptor.Digest == "" { // NewRef treats empty digest as unknown.
		return nil, fmt.Errorf("invalid digest")
	}

	var rawManifest []byte
	if int64(len(descriptor.Data)) == descriptor.Size {
		rawManifest = descriptor.Data
	} else if len(descriptor.Data) != 0 {
		return nil, fmt.Errorf("descriptor has embedded data of wrong length")
	} else {
		var err error
		rawManifest, err = i.blobs.Manifest(descriptor)
		if err != nil {
			return nil, err
		}
	}
	if int64(len(rawManifest)) != descriptor.Size {
		return nil, fmt.Errorf("manifest has wrong length, expected %d, got %d bytes", descriptor.Size, len(rawManifest))
	}
	blobs, err := i.blobs.Blobs(descriptor)
	if err != nil {
		return nil, err
	}
	return NewRef(rawManifest, descriptor.MediaType, string(descriptor.Digest), blobs)
}

// Descriptors returns an iterator over all descriptors in the image (config and
// layers).
func (i *Image) Descriptors() iter.Seq[*ocispecv1.Descriptor] {
	return func(yield func(*ocispecv1.Descriptor) bool) {
		if !yield(&i.Manifest.Config) {
			return
		}
		for l := range i.Manifest.Layers {
			if !yield(&i.Manifest.Layers[l]) {
				return
			}
		}
	}
}

// Blob returns the contents of a blob from its descriptor.
// It does not verify the contents against the digest.
func (i *Image) Blob(descriptor *ocispecv1.Descriptor) (io.ReadCloser, error) {
	if descriptor.Size < 0 {
		return nil, fmt.Errorf("invalid descriptor size %d", descriptor.Size)
	}
	if int64(len(descriptor.Data)) == descriptor.Size {
		return structfs.Bytes(descriptor.Data).Open()
	} else if len(descriptor.Data) != 0 {
		return nil, fmt.Errorf("descriptor has embedded data of wrong length")
	}
	return i.blobs.Blob(descriptor)
}

// ReadBlobVerified reads a blob into a byte slice and verifies it against the
// digest.
func (i *Image) ReadBlobVerified(descriptor *ocispecv1.Descriptor) ([]byte, error) {
	if descriptor.Size > 50*1024*1024 {
		return nil, fmt.Errorf("refusing to read blob of size %d into memory", descriptor.Size)
	}
	expectedDigest := string(descriptor.Digest)
	if _, _, err := ParseDigest(expectedDigest); err != nil {
		return nil, err
	}
	blob, err := i.Blob(descriptor)
	if err != nil {
		return nil, err
	}
	defer blob.Close()
	content := make([]byte, descriptor.Size)
	_, err = io.ReadFull(blob, content)
	if err != nil {
		return nil, err
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(content))
	if expectedDigest != digest {
		return nil, fmt.Errorf("failed verification of blob: expected digest %q, computed %q", expectedDigest, digest)
	}
	return content, nil
}

// StructfsBlob wraps an image and descriptor into a [structfs.Blob].
func (i *Image) StructfsBlob(descriptor *ocispecv1.Descriptor) structfs.Blob {
	return &structfsBlob{
		image:      i,
		descriptor: descriptor,
	}
}

type structfsBlob struct {
	image      *Image
	descriptor *ocispecv1.Descriptor
}

func (b *structfsBlob) Open() (io.ReadCloser, error) {
	return b.image.Blob(b.descriptor)
}

func (b *structfsBlob) Size() int64 {
	return b.descriptor.Size
}

// ParseDigest splits a digest into its components. It returns an error if the
// algorithm is not supported, or if encoded is not valid for the algorithm.
func ParseDigest(digest string) (algorithm string, encoded string, err error) {
	algorithm, encoded, ok := strings.Cut(digest, ":")
	if !ok {
		return "", "", fmt.Errorf("invalid digest")
	}
	switch algorithm {
	case "sha256":
		rest := strings.TrimLeft(encoded, "0123456789abcdef")
		if len(rest) != 0 {
			return "", "", fmt.Errorf("invalid character in sha256 digest")
		}
		if len(encoded) != sha256.Size*2 {
			return "", "", fmt.Errorf("invalid sha256 digest length")
		}
	default:
		return "", "", fmt.Errorf("unknown digest algorithm %q", algorithm)
	}
	return
}
