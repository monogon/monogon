// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Package osimage allows reading OS images represented as OCI artifacts, and
// contains the types for the OS image config.
package osimage

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"

	"source.monogon.dev/osbase/oci"
	"source.monogon.dev/osbase/structfs"
)

// Image represents an OS image.
type Image struct {
	// Config contains the parsed config.
	Config *Config
	// RawConfig contains the bytes of the config.
	RawConfig []byte

	image *oci.Image
}

// Read reads the config from an OCI image and returns an [Image].
func Read(image *oci.Image) (*Image, error) {
	manifest := image.Manifest
	if manifest.ArtifactType != ArtifactTypeOSImage {
		return nil, fmt.Errorf("unexpected manifest artifact type %q", manifest.ArtifactType)
	}
	if manifest.Config.MediaType != MediaTypeOSImageConfig {
		return nil, fmt.Errorf("unexpected config media type %q", manifest.Config.MediaType)
	}

	// Read the config.
	rawConfig, err := image.ReadBlobVerified(&manifest.Config)
	if err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}
	config := &Config{}
	err = json.Unmarshal(rawConfig, config)
	if err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}
	if config.FormatVersion != ConfigVersion {
		return nil, fmt.Errorf("unsupported config version %q", config.FormatVersion)
	}
	if len(config.Payloads) != len(manifest.Layers) {
		return nil, fmt.Errorf("number of layers %d does not match number of payloads %d", len(manifest.Layers), len(config.Payloads))
	}
	for i := range config.Payloads {
		payload := &config.Payloads[i]
		if payload.Size < 0 {
			return nil, fmt.Errorf("payload %q has negative size", payload.Name)
		}
		if payload.HashChunkSize <= 0 {
			return nil, fmt.Errorf("payload %q has invalid chunk size %d", payload.Name, payload.HashChunkSize)
		}
		if payload.HashChunkSize > 16*1024*1024 {
			return nil, fmt.Errorf("payload %q has too large chunk size %d", payload.Name, payload.HashChunkSize)
		}
		chunks := payload.Size / payload.HashChunkSize
		if chunks*payload.HashChunkSize < payload.Size {
			chunks++
		}
		if int64(len(payload.ChunkHashesSHA256)) != chunks {
			return nil, fmt.Errorf("payload %q has %d chunks but %d chunk hashes", payload.Name, chunks, len(payload.ChunkHashesSHA256))
		}
	}

	osImage := &Image{
		Config:    config,
		RawConfig: rawConfig,
		image:     image,
	}
	return osImage, nil
}

// Payload returns the contents of the payload of the given name.
// All data is verified against hashes in the config before it is returned.
func (i *Image) Payload(name string) (structfs.Blob, error) {
	for pi := range i.Config.Payloads {
		info := &i.Config.Payloads[pi]
		if info.Name == name {
			layer := &i.image.Manifest.Layers[pi]
			blob := &payloadBlob{
				raw:       i.image.StructfsBlob(layer),
				mediaType: layer.MediaType,
				info:      info,
			}
			return blob, nil
		}
	}
	return nil, fmt.Errorf("payload %q not found", name)
}

// PayloadUnverified returns the contents of the payload of the given name.
// Data is not verified against hashes. This only works for uncompressed images.
func (i *Image) PayloadUnverified(name string) (structfs.Blob, error) {
	for pi, info := range i.Config.Payloads {
		if info.Name == name {
			layer := &i.image.Manifest.Layers[pi]
			if layer.MediaType != MediaTypePayloadUncompressed {
				return nil, fmt.Errorf("unsupported media type %q for unverified payload", layer.MediaType)
			}
			return i.image.StructfsBlob(layer), nil
		}
	}
	return nil, fmt.Errorf("payload %q not found", name)
}

type payloadBlob struct {
	raw       structfs.Blob
	mediaType string
	info      *PayloadInfo
}

func (b *payloadBlob) Open() (io.ReadCloser, error) {
	blobReader, err := b.raw.Open()
	if err != nil {
		return nil, err
	}
	reader := &payloadReader{
		chunkHashes: b.info.ChunkHashesSHA256,
		blobReader:  blobReader,
		remaining:   b.info.Size,
		buf:         make([]byte, b.info.HashChunkSize),
	}
	switch b.mediaType {
	case MediaTypePayloadUncompressed:
		reader.uncompressed = blobReader
	case MediaTypePayloadZstd:
		reader.zstdDecoder, err = zstd.NewReader(blobReader)
		if err != nil {
			blobReader.Close()
			return nil, fmt.Errorf("failed to create zstd decoder: %w", err)
		}
		reader.uncompressed = reader.zstdDecoder
	default:
		blobReader.Close()
		return nil, fmt.Errorf("unsupported media type %q", b.mediaType)
	}
	return reader, nil
}

func (b *payloadBlob) Size() int64 {
	return b.info.Size
}

type payloadReader struct {
	chunkHashes  []string
	blobReader   io.ReadCloser
	zstdDecoder  *zstd.Decoder
	uncompressed io.Reader
	remaining    int64  // number of bytes remaining in uncompressed
	buf          []byte // buffer of chunk size
	available    []byte // bytes available for reading in the last read chunk
}

func (r *payloadReader) Read(p []byte) (n int, err error) {
	if len(r.available) != 0 {
		n = copy(p, r.available)
		r.available = r.available[n:]
		return
	}
	if r.remaining == 0 {
		err = io.EOF
		return
	}
	chunkLen := min(r.remaining, int64(len(r.buf)))
	chunk := r.buf[:chunkLen]
	_, err = io.ReadFull(r.uncompressed, chunk)
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		r.remaining = 0
		return
	}
	chunkHashBytes := sha256.Sum256(chunk)
	chunkHash := base64.RawStdEncoding.EncodeToString(chunkHashBytes[:])
	if chunkHash != r.chunkHashes[0] {
		err = fmt.Errorf("payload failed verification against chunk hash, expected %q, got %q", r.chunkHashes[0], chunkHash)
		r.remaining = 0
		return
	}
	r.chunkHashes = r.chunkHashes[1:]
	r.remaining -= chunkLen
	n = copy(p, chunk)
	r.available = chunk[n:]
	return
}

func (r *payloadReader) Close() error {
	if r.zstdDecoder != nil {
		r.zstdDecoder.Close()
	}
	return r.blobReader.Close()
}
