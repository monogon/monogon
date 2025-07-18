// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"

	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go"
	ocispecv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"source.monogon.dev/osbase/oci/osimage"
)

const hashChunkSize = 1024 * 1024

var payloadNameRegexp = regexp.MustCompile(`^[0-9A-Za-z-](?:[0-9A-Za-z._-]{0,78}[0-9A-Za-z_-])?$`)

var (
	productInfoPath  = flag.String("product_info", "", "Path to the product info JSON file")
	payloadName      = flag.String("payload_name", "", "Payload name for the next payload_file flag")
	compressionLevel = flag.Int("compression_level", int(zstd.SpeedDefault), "Compression level")
	outPath          = flag.String("out", "", "Output OCI Image Layout directory path")
)

var architectureToOCI = map[string]string{
	"x86_64":  "amd64",
	"aarch64": "arm64",
}

type payload struct {
	name string
	path string
}

type countWriter struct {
	size int64
}

func (c *countWriter) Write(p []byte) (n int, err error) {
	c.size += int64(len(p))
	return len(p), nil
}

func processPayloadsUncompressed(payloads []payload, blobsPath string) ([]osimage.PayloadInfo, []ocispecv1.Descriptor, error) {
	payloadInfos := []osimage.PayloadInfo{}
	payloadDescriptors := []ocispecv1.Descriptor{}
	buf := make([]byte, hashChunkSize)
	for _, payload := range payloads {
		payloadFile, err := os.Open(payload.path)
		if err != nil {
			return nil, nil, err
		}
		payloadStat, err := payloadFile.Stat()
		if err != nil {
			return nil, nil, err
		}
		remaining := payloadStat.Size()
		fullHash := sha256.New()
		var chunkHashes []string
		for remaining != 0 {
			chunk := buf[:min(remaining, int64(len(buf)))]
			remaining -= int64(len(chunk))
			_, err := io.ReadFull(payloadFile, chunk)
			if err != nil {
				return nil, nil, err
			}
			fullHash.Write(chunk)
			chunkHashBytes := sha256.Sum256(chunk)
			chunkHash := base64.RawStdEncoding.EncodeToString(chunkHashBytes[:])
			chunkHashes = append(chunkHashes, chunkHash)
		}
		payloadFile.Close()

		fullHashSum := fmt.Sprintf("%x", fullHash.Sum(nil))

		payloadInfos = append(payloadInfos, osimage.PayloadInfo{
			Name:              payload.name,
			Size:              payloadStat.Size(),
			HashChunkSize:     hashChunkSize,
			ChunkHashesSHA256: chunkHashes,
		})
		payloadDescriptors = append(payloadDescriptors, ocispecv1.Descriptor{
			MediaType: osimage.MediaTypePayloadUncompressed,
			Digest:    digest.NewDigestFromEncoded(digest.SHA256, fullHashSum),
			Size:      payloadStat.Size(),
		})

		relPath, err := filepath.Rel(blobsPath, payload.path)
		if err != nil {
			return nil, nil, err
		}
		err = os.Symlink(relPath, filepath.Join(blobsPath, fullHashSum))
		if err != nil {
			return nil, nil, err
		}
	}
	return payloadInfos, payloadDescriptors, nil
}

func processPayloadsZstd(payloads []payload, blobsPath string) ([]osimage.PayloadInfo, []ocispecv1.Descriptor, error) {
	payloadInfos := []osimage.PayloadInfo{}
	payloadDescriptors := []ocispecv1.Descriptor{}
	buf := make([]byte, hashChunkSize)
	tmpPath := filepath.Join(blobsPath, "tmp")
	zstdWriter, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevel(*compressionLevel)))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create zstd writer: %w", err)
	}
	for _, payload := range payloads {
		payloadFile, err := os.Open(payload.path)
		if err != nil {
			return nil, nil, err
		}
		payloadStat, err := payloadFile.Stat()
		if err != nil {
			return nil, nil, err
		}

		compressedFile, err := os.Create(tmpPath)
		if err != nil {
			return nil, nil, err
		}
		compressedSize := &countWriter{}
		compressedHash := sha256.New()
		compressedWriter := io.MultiWriter(compressedFile, compressedSize, compressedHash)
		zstdWriter.ResetContentSize(compressedWriter, payloadStat.Size())
		remaining := payloadStat.Size()
		var chunkHashes []string
		for remaining != 0 {
			chunk := buf[:min(remaining, int64(len(buf)))]
			remaining -= int64(len(chunk))
			_, err := io.ReadFull(payloadFile, chunk)
			if err != nil {
				return nil, nil, err
			}
			_, err = zstdWriter.Write(chunk)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to write compressed data: %w", err)
			}
			chunkHashBytes := sha256.Sum256(chunk)
			chunkHash := base64.RawStdEncoding.EncodeToString(chunkHashBytes[:])
			chunkHashes = append(chunkHashes, chunkHash)
		}
		err = zstdWriter.Close()
		if err != nil {
			return nil, nil, fmt.Errorf("failed to close zstd writer: %w", err)
		}
		err = compressedFile.Close()
		if err != nil {
			return nil, nil, err
		}
		payloadFile.Close()

		compressedHashSum := fmt.Sprintf("%x", compressedHash.Sum(nil))

		payloadInfos = append(payloadInfos, osimage.PayloadInfo{
			Name:              payload.name,
			Size:              payloadStat.Size(),
			HashChunkSize:     hashChunkSize,
			ChunkHashesSHA256: chunkHashes,
		})
		payloadDescriptors = append(payloadDescriptors, ocispecv1.Descriptor{
			MediaType: osimage.MediaTypePayloadZstd,
			Digest:    digest.NewDigestFromEncoded(digest.SHA256, compressedHashSum),
			Size:      compressedSize.size,
		})

		err = os.Rename(tmpPath, filepath.Join(blobsPath, compressedHashSum))
		if err != nil {
			return nil, nil, err
		}
	}
	return payloadInfos, payloadDescriptors, nil
}

func main() {
	var payloads []payload
	seenNames := make(map[string]bool)
	flag.Func("payload_file", "Payload file path", func(payloadPath string) error {
		if *payloadName == "" {
			return fmt.Errorf("payload_name not set")
		}
		if !payloadNameRegexp.MatchString(*payloadName) {
			return fmt.Errorf("invalid payload name %q", *payloadName)
		}
		if seenNames[*payloadName] {
			return fmt.Errorf("duplicate payload name %q", *payloadName)
		}
		seenNames[*payloadName] = true
		payloads = append(payloads, payload{
			name: *payloadName,
			path: payloadPath,
		})
		return nil
	})
	flag.Parse()

	rawProductInfo, err := os.ReadFile(*productInfoPath)
	if err != nil {
		log.Fatalf("Failed to read product info file: %v", err)
	}
	var productInfo osimage.ProductInfo
	err = json.Unmarshal(rawProductInfo, &productInfo)
	if err != nil {
		log.Fatal(err)
	}

	// Create blobs directory.
	blobsPath := filepath.Join(*outPath, "blobs", "sha256")
	err = os.MkdirAll(blobsPath, 0755)
	if err != nil {
		log.Fatal(err)
	}

	// Process payloads.
	var payloadInfos []osimage.PayloadInfo
	var payloadDescriptors []ocispecv1.Descriptor
	if *compressionLevel == 0 {
		payloadInfos, payloadDescriptors, err = processPayloadsUncompressed(payloads, blobsPath)
	} else {
		payloadInfos, payloadDescriptors, err = processPayloadsZstd(payloads, blobsPath)
	}
	if err != nil {
		log.Fatalf("Failed to process payloads: %v", err)
	}

	// Write the OS image config.
	imageConfig := osimage.Config{
		FormatVersion: osimage.ConfigVersion,
		ProductInfo:   productInfo,
		Payloads:      payloadInfos,
	}
	imageConfigBytes, err := json.MarshalIndent(imageConfig, "", "\t")
	if err != nil {
		log.Fatalf("Failed to marshal OS image config: %v", err)
	}
	imageConfigBytes = append(imageConfigBytes, '\n')
	imageConfigHash := fmt.Sprintf("%x", sha256.Sum256(imageConfigBytes))
	err = os.WriteFile(filepath.Join(blobsPath, imageConfigHash), imageConfigBytes, 0644)
	if err != nil {
		log.Fatalf("Failed to write OS image config: %v", err)
	}

	// Write the image manifest.
	imageManifest := ocispecv1.Manifest{
		Versioned:    ocispec.Versioned{SchemaVersion: 2},
		MediaType:    ocispecv1.MediaTypeImageManifest,
		ArtifactType: osimage.ArtifactTypeOSImage,
		Config: ocispecv1.Descriptor{
			MediaType: osimage.MediaTypeOSImageConfig,
			Digest:    digest.NewDigestFromEncoded(digest.SHA256, imageConfigHash),
			Size:      int64(len(imageConfigBytes)),
		},
		Layers: payloadDescriptors,
	}
	imageManifestBytes, err := json.MarshalIndent(imageManifest, "", "\t")
	if err != nil {
		log.Fatalf("Failed to marshal image manifest: %v", err)
	}
	imageManifestBytes = append(imageManifestBytes, '\n')
	imageManifestHash := fmt.Sprintf("%x", sha256.Sum256(imageManifestBytes))
	err = os.WriteFile(filepath.Join(blobsPath, imageManifestHash), imageManifestBytes, 0644)
	if err != nil {
		log.Fatalf("Failed to write image manifest: %v", err)
	}

	// Write the index.
	platformArchitecture, ok := architectureToOCI[productInfo.Architecture()]
	if !ok {
		log.Fatalf("Missing architectureToOCI entry for %q", productInfo.Architecture())
	}
	platformOS := productInfo.PlatformOS
	if platformOS == "" {
		platformOS = "unknown"
	}
	imageIndex := ocispecv1.Index{
		Versioned: ocispec.Versioned{SchemaVersion: 2},
		MediaType: ocispecv1.MediaTypeImageIndex,
		Manifests: []ocispecv1.Descriptor{{
			MediaType:    ocispecv1.MediaTypeImageManifest,
			ArtifactType: osimage.MediaTypeOSImageConfig,
			// The platform set here is used when building an OCI index with Bazel.
			// Other consumers cannot rely on it being present because it is not
			// preserved when pushing to a registry. We don't use the OS field, but
			// it's required by the OCI spec.
			Platform: &ocispecv1.Platform{
				Architecture: platformArchitecture,
				OS:           platformOS,
			},
			Digest: digest.NewDigestFromEncoded(digest.SHA256, imageManifestHash),
			Size:   int64(len(imageManifestBytes)),
		}},
	}
	imageIndexBytes, err := json.MarshalIndent(imageIndex, "", "\t")
	if err != nil {
		log.Fatalf("Failed to marshal image index: %v", err)
	}
	imageIndexBytes = append(imageIndexBytes, '\n')
	err = os.WriteFile(filepath.Join(*outPath, "index.json"), imageIndexBytes, 0644)
	if err != nil {
		log.Fatalf("Failed to write image index: %v", err)
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
