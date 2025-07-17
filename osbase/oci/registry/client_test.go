// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/cenkalti/backoff/v4"

	"source.monogon.dev/osbase/oci"
)

var (
	// These are filled by bazel at linking time with the canonical path of
	// their corresponding file. Inside the init function we resolve it
	// with the rules_go runfiles package to the real path.
	xImagePath string
)

func init() {
	var err error
	for _, path := range []*string{
		&xImagePath,
	} {
		*path, err = runfiles.Rlocation(*path)
		if err != nil {
			panic(err)
		}
	}
}

func TestRetries(t *testing.T) {
	srcImage, err := oci.AsImage(oci.ReadLayout(xImagePath))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer()
	server.AddRef("test/repo", "test-tag", srcImage)
	wrapper := &unreliableServer{
		handler:   server,
		blobLimit: srcImage.Manifest.Config.Size / 2,
		seen:      make(map[string]bool),
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go http.Serve(listener, wrapper)
	wrapper.host = listener.Addr().String()

	client := &Client{
		GetBackOff: func() backoff.BackOff {
			return backoff.NewExponentialBackOff(backoff.WithInitialInterval(time.Millisecond))
		},
		RetryNotify: func(err error, d time.Duration) {
			fmt.Printf("Retrying in %v: %v\n", d, err)
		},
		Scheme:     "http",
		Host:       listener.Addr().String(),
		Repository: "test/repo",
	}

	image, err := oci.AsImage(client.Read(context.Background(), "test-tag", srcImage.Digest()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = image.ReadBlobVerified(&image.Manifest.Config)
	if err != nil {
		t.Error(err)
	}
}

type unreliableServer struct {
	handler   http.Handler
	host      string
	blobLimit int64
	mu        sync.Mutex
	seen      map[string]bool
}

func (s *unreliableServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	fmt.Printf("%s %s %s\n", req.Method, req.URL.String(), req.Header.Get("Range"))

	// Every path returns a temporary error the first time it is hit. This
	// includes the redirected and token paths.
	s.mu.Lock()
	if !s.seen[req.URL.Path] {
		s.seen[req.URL.Path] = true
		s.mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	s.mu.Unlock()

	// Every path is redirected.
	var ok bool
	req.URL.Path, ok = strings.CutPrefix(req.URL.Path, "/redirected")
	if !ok {
		req.URL.Path = "/redirected" + req.URL.Path
		w.Header().Set("Location", req.URL.String())
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}

	// Each request requires a token.
	if req.URL.Path == "/token" {
		query := req.URL.Query()
		if query.Get("service") != "myregistry.test" || query.Get("scope") != "repository:test/repo:pull" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"token": "the_token"}`))
		return
	} else if req.Header.Get("Authorization") != "Bearer the_token" {
		w.Header().Set("Www-Authenticate", fmt.Sprintf(`Bearer realm="http://%s/token",service="myregistry.test",scope="repository:test/repo:pull"`, s.host))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Blob requests fail after returning part of the response, requiring retries
	// with Range header.
	if strings.Contains(req.URL.Path, "/blobs/") {
		w = &limitResponseWriter{ResponseWriter: w, remaining: s.blobLimit}
	}

	s.handler.ServeHTTP(w, req)
}

type limitResponseWriter struct {
	http.ResponseWriter
	remaining int64
}

func (w *limitResponseWriter) Write(b []byte) (n int, err error) {
	if w.remaining <= 0 {
		return 0, fmt.Errorf("limit reached")
	}
	if int64(len(b)) > w.remaining {
		n, _ = w.ResponseWriter.Write(b[:w.remaining])
		err = fmt.Errorf("limit reached")
		w.remaining = 0
		return
	}
	w.remaining -= int64(len(b))
	return w.ResponseWriter.Write(b)
}
