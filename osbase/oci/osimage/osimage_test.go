// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package osimage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/cenkalti/backoff/v4"
	ocispecv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"source.monogon.dev/osbase/oci"
	"source.monogon.dev/osbase/oci/registry"
)

var (
	// These are filled by bazel at linking time with the canonical path of
	// their corresponding file. Inside the init function we resolve it
	// with the rules_go runfiles package to the real path.
	xImagePath             string
	xImageUncompressedPath string
	xTestPayloadPath       string
)

func init() {
	var err error
	for _, path := range []*string{
		&xImagePath, &xImageUncompressedPath, &xTestPayloadPath,
	} {
		*path, err = runfiles.Rlocation(*path)
		if err != nil {
			panic(err)
		}
	}
}

var expectedPayloadHash [32]byte
var expectedPayloadLen int64

func init() {
	expectedPayload, err := os.ReadFile(xTestPayloadPath)
	if err != nil {
		panic(err)
	}
	expectedPayloadHash = sha256.Sum256(expectedPayload)
	expectedPayloadLen = int64(len(expectedPayload))
}

func TestRead(t *testing.T) {
	testCases := []struct {
		desc string
		path string
	}{
		{"compressed", xImagePath},
		{"uncompressed", xImageUncompressedPath},
	}
	for _, tC := range testCases {
		t.Run(tC.desc, func(t *testing.T) {
			image, err := oci.AsImage(oci.ReadLayout(tC.path))
			if err != nil {
				t.Fatal(err)
			}
			osImage, err := Read(image)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := osImage.Payload("test")
			if err != nil {
				t.Fatal(err)
			}
			if got, want := payload.Size(), expectedPayloadLen; got != want {
				t.Errorf("payload has size %d, expected %d", got, want)
			}
			reader, err := payload.Open()
			if err != nil {
				t.Fatal(err)
			}
			content, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			contentHash := sha256.Sum256(content)
			if contentHash != expectedPayloadHash {
				t.Errorf("Payload read through Image does not match expected content, expected %x, got %x", expectedPayloadHash, contentHash)
			}
			if err := reader.Close(); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestVerification(t *testing.T) {
	server := registry.NewServer()
	srcImage, err := oci.AsImage(oci.ReadLayout(xImageUncompressedPath))
	if err != nil {
		t.Fatal(err)
	}
	server.AddRef("test/repo", "test-tag", srcImage)
	corrupter := &corruptingServer{handler: server}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go http.Serve(listener, corrupter)

	client := &registry.Client{
		GetBackOff: func() backoff.BackOff {
			return backoff.NewExponentialBackOff()
		},
		RetryNotify: func(err error, _ time.Duration) {
			t.Errorf("Unexpected retry; verification errors should not trigger retries: %v", err)
		},
		Scheme:     "http",
		Host:       listener.Addr().String(),
		Repository: "test/repo",
	}

	// Test manifest verification
	corrupter.affectedPath = "/v2/test/repo/manifests/test-tag"
	_, err = client.Read(context.Background(), "test-tag", "")
	if err != nil {
		t.Errorf("Expected reading manifest to succeed when digest not given: %v", err)
	}
	_, err = client.Read(context.Background(), "test-tag", srcImage.Digest())
	if !strings.Contains(fmt.Sprintf("%v", err), "failed verification") {
		t.Errorf("Expected failed verification, got %v", err)
	}

	// Test config verification
	corrupter.affectedPath = fmt.Sprintf("/v2/test/repo/blobs/%s", srcImage.Manifest.Config.Digest)
	image, err := oci.AsImage(client.Read(context.Background(), "test-tag", srcImage.Digest()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Read(image)
	if !strings.Contains(fmt.Sprintf("%v", err), "failed verification") {
		t.Errorf("Expected failed verification, got %v", err)
	}

	// Test payload verification
	corrupter.affectedPath = fmt.Sprintf("/v2/test/repo/blobs/%s", srcImage.Manifest.Layers[0].Digest)
	image, err = oci.AsImage(client.Read(context.Background(), "test-tag", srcImage.Digest()))
	if err != nil {
		t.Fatal(err)
	}
	osImage, err := Read(image)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := osImage.Payload("test")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := payload.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	content, err := io.ReadAll(reader)
	if !strings.Contains(fmt.Sprintf("%v", err), "payload failed verification") {
		t.Errorf("Expected failed verification, got %v", err)
	}
	if len(content) != 0 {
		t.Errorf("Did not expect to read any content, got %d bytes", len(content))
	}
}

type corruptingServer struct {
	affectedPath string
	handler      http.Handler
}

func (s *corruptingServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == s.affectedPath {
		w = &corruptingResponseWriter{ResponseWriter: w}
	}
	s.handler.ServeHTTP(w, req)
}

// corruptingResponseWriter replaces the first newline in the response with a
// space. This means that JSON parsing will still succeed, but digest
// verification should fail.
type corruptingResponseWriter struct {
	http.ResponseWriter
	corrupted bool
}

func (w *corruptingResponseWriter) Write(b []byte) (n int, err error) {
	index := bytes.IndexByte(b, '\n')
	if w.corrupted || index == -1 {
		return w.ResponseWriter.Write(b)
	}
	b = bytes.Clone(b)
	b[index] = ' '
	n, err = w.ResponseWriter.Write(b)
	if n > index {
		w.corrupted = true
	}
	return
}

func TestTruncation(t *testing.T) {
	srcImage, err := oci.AsImage(oci.ReadLayout(xImageUncompressedPath))
	if err != nil {
		t.Fatal(err)
	}
	blobs := &truncatedBlobs{
		image:  srcImage,
		length: srcImage.Manifest.Config.Size,
	}
	truncatedImage, err := oci.AsImage(oci.NewRef(srcImage.RawManifest(), ocispecv1.MediaTypeImageManifest, "", blobs))
	if err != nil {
		t.Fatal(err)
	}

	osImage, err := Read(truncatedImage)
	if err != nil {
		t.Fatal(err)
	}
	blobs.length = osImage.Config.Payloads[0].HashChunkSize
	payload, err := osImage.Payload("test")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := payload.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	_, err = io.ReadAll(reader)
	if err == nil {
		t.Error("Expected to get an error, got nil")
	}
}

type truncatedBlobs struct {
	image  *oci.Image
	length int64
}

func (b *truncatedBlobs) Blob(d *ocispecv1.Descriptor) (io.ReadCloser, error) {
	reader, err := b.image.Blob(d)
	if err != nil {
		return nil, err
	}
	reader = &readCloser{
		Reader: io.LimitReader(reader, b.length),
		Closer: reader,
	}
	return reader, nil
}

func (b *truncatedBlobs) Manifest(_ *ocispecv1.Descriptor) ([]byte, error) {
	return nil, fmt.Errorf("not implemented")
}

func (b *truncatedBlobs) Blobs(_ *ocispecv1.Descriptor) (oci.Blobs, error) {
	return b, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}
