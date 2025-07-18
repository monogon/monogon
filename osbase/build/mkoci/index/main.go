// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go"
	ocispecv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"source.monogon.dev/osbase/oci"
)

var (
	outPath = flag.String("out", "", "Output OCI Image Layout directory path")
)

func addImage(outPath string, path string, haveBlob map[digest.Digest]bool) (*ocispecv1.Descriptor, error) {
	index, err := oci.ReadLayoutIndex(path)
	if err != nil {
		return nil, err
	}
	if len(index.Manifest.Manifests) == 0 {
		return nil, fmt.Errorf("index.json contains no manifests")
	}
	if len(index.Manifest.Manifests) != 1 {
		return nil, fmt.Errorf("index.json files containing multiple manifests are not supported")
	}
	manifestDescriptor := &index.Manifest.Manifests[0]

	image, err := oci.AsImage(index.Ref(manifestDescriptor))
	if err != nil {
		return nil, err
	}

	// Create symlinks to blobs
	descriptors := []ocispecv1.Descriptor{*manifestDescriptor, image.Manifest.Config}
	descriptors = append(descriptors, image.Manifest.Layers...)
	for _, descriptor := range descriptors {
		if haveBlob[descriptor.Digest] {
			continue
		}
		haveBlob[descriptor.Digest] = true

		algorithm, encoded, err := oci.ParseDigest(string(descriptor.Digest))
		if err != nil {
			return nil, fmt.Errorf("failed to parse digest: %w", err)
		}
		srcPath := filepath.Join(path, "blobs", algorithm, encoded)
		destDir := filepath.Join(outPath, "blobs", algorithm)
		destPath := filepath.Join(outPath, "blobs", algorithm, encoded)
		relPath, err := filepath.Rel(destDir, srcPath)
		if err != nil {
			return nil, err
		}
		err = os.Symlink(relPath, destPath)
		if err != nil {
			return nil, err
		}
	}

	return manifestDescriptor, nil
}

func main() {
	var images []string
	flag.Func("image", "OCI image path", func(path string) error {
		images = append(images, path)
		return nil
	})
	flag.Parse()

	// Create blobs directory.
	blobsPath := filepath.Join(*outPath, "blobs", "sha256")
	err := os.MkdirAll(blobsPath, 0755)
	if err != nil {
		log.Fatal(err)
	}

	haveBlob := make(map[digest.Digest]bool)
	index := ocispecv1.Index{
		Versioned: ocispec.Versioned{SchemaVersion: 2},
		MediaType: ocispecv1.MediaTypeImageIndex,
		Manifests: []ocispecv1.Descriptor{},
	}
	for _, path := range images {
		descriptor, err := addImage(*outPath, path, haveBlob)
		if err != nil {
			log.Fatalf("Failed to add image %q: %v", path, err)
		}
		index.Manifests = append(index.Manifests, *descriptor)
	}

	// Write the index manifest.
	indexBytes, err := json.MarshalIndent(index, "", "\t")
	if err != nil {
		log.Fatalf("Failed to marshal index manifest: %v", err)
	}
	indexBytes = append(indexBytes, '\n')
	indexHash := fmt.Sprintf("%x", sha256.Sum256(indexBytes))
	err = os.WriteFile(filepath.Join(blobsPath, indexHash), indexBytes, 0644)
	if err != nil {
		log.Fatalf("Failed to write index manifest: %v", err)
	}

	// Write the entry-point index.
	topIndex := ocispecv1.Index{
		Versioned: ocispec.Versioned{SchemaVersion: 2},
		MediaType: ocispecv1.MediaTypeImageIndex,
		Manifests: []ocispecv1.Descriptor{{
			MediaType: ocispecv1.MediaTypeImageIndex,
			Digest:    digest.NewDigestFromEncoded(digest.SHA256, indexHash),
			Size:      int64(len(indexBytes)),
		}},
	}
	topIndexBytes, err := json.MarshalIndent(topIndex, "", "\t")
	if err != nil {
		log.Fatalf("Failed to marshal entry-point index: %v", err)
	}
	topIndexBytes = append(topIndexBytes, '\n')
	err = os.WriteFile(filepath.Join(*outPath, "index.json"), topIndexBytes, 0644)
	if err != nil {
		log.Fatalf("Failed to write entry-point index: %v", err)
	}

	// Write the oci-layout marker file.
	err = os.WriteFile(
		filepath.Join(*outPath, "oci-layout"),
		[]byte(`{"imageLayoutVersion": "1.0.0"}`+"\n"),
		0644,
	)
	if err != nil {
		log.Fatalf("Failed to write oci-layout file: %v", err)
	}
}
