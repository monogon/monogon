// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package socksproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"testing"

	"golang.org/x/net/proxy"
)

// TestE2E implements a happy path test by chaining together an HTTP server, a
// proxy server, a proxy client (from golang.org/x/net) and an HTTP client into
// an end-to-end test. It uses HostHandler and the actual host network stack for
// the test HTTP server and test proxy server.
func TestE2E(t *testing.T) {
	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()

	// Start test HTTP server.
	lisSrv, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not bind http listener: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(rw http.ResponseWriter, req *http.Request) {
		fmt.Fprintf(rw, "foo")
	})
	go func() {
		err := http.Serve(lisSrv, mux)
		if err != nil {
			t.Errorf("http.Serve: %v", err)
			return
		}
	}()

	// Start proxy server.
	lisPrx, err := net.Listen("tcp", ":")
	if err != nil {
		t.Fatalf("could not bind proxy listener: %v", err)
	}
	go func() {
		err := Serve(ctx, HostHandler, lisPrx)
		if err != nil && !errors.Is(err, ctx.Err()) {
			t.Errorf("proxy.Serve: %v", err)
			return
		}
	}()

	// Start proxy client.
	dialer, err := proxy.SOCKS5("tcp", lisPrx.Addr().String(), nil, proxy.Direct)
	if err != nil {
		t.Fatalf("creating SOCKS dialer failed: %v", err)
	}

	// Create http client.
	tr := &http.Transport{
		Dial: dialer.Dial,
	}
	cl := &http.Client{
		Transport: tr,
	}

	// Perform request and expect 'foo' in response.
	url := fmt.Sprintf("http://%s/", lisSrv.Addr().String())
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatalf("creating test request failed: %v", err)
	}
	res, err := cl.Do(req)
	if err != nil {
		t.Fatalf("test http request failed: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if want, got := "foo", string(body); want != got {
		t.Errorf("wrong response from HTTP, wanted %q, got %q", want, got)
	}
}

// testHandler is a handler which serves /dev/zero and keeps count of the
// current number of live connections. It's used in TestCancellation to ensure
// contexts are canceled appropriately.
type testHandler struct {
	live int64
}

func (t *testHandler) Connect(ctx context.Context, req *ConnectRequest) *ConnectResponse {
	f, _ := os.Open("/dev/zero")

	atomic.AddInt64(&t.live, 1)
	go func() {
		<-ctx.Done()
		atomic.AddInt64(&t.live, -1)
		f.Close()
	}()

	return &ConnectResponse{
		Backend:      f,
		LocalAddress: net.ParseIP("127.0.0.1"),
		LocalPort:    42123,
	}
}

// TestCancellation ensures request contexts are canceled correctly - when an
// incoming connection is closed and when the entire server is stopped.
func TestCancellation(t *testing.T) {
	handler := &testHandler{}

	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()

	// Start proxy server.
	lisPrx, err := net.Listen("tcp", ":")
	if err != nil {
		t.Fatalf("could not bind proxy listener: %v", err)
	}
	go func() {
		err := Serve(ctx, handler, lisPrx)
		if err != nil && !errors.Is(err, ctx.Err()) {
			t.Errorf("proxy.Serve: %v", err)
			return
		}
	}()

	// Start proxy client.
	dialer, err := proxy.SOCKS5("tcp", lisPrx.Addr().String(), nil, proxy.Direct)
	if err != nil {
		t.Fatalf("creating SOCKS dialer failed: %v", err)
	}

	// Open two connections.
	con1, err := dialer.Dial("tcp", "192.2.0.10:1234")
	if err != nil {
		t.Fatalf("Dialing first client failed: %v", err)
	}
	con2, err := dialer.Dial("tcp", "192.2.0.10:1234")
	if err != nil {
		t.Fatalf("Dialing first client failed: %v", err)
	}

	// Read some data. This makes sure we're ready to check for the liveness of
	// currently running connections.
	io.ReadFull(con1, make([]byte, 3))
	io.ReadFull(con2, make([]byte, 3))

	// Ensure we have two connections.
	if want, got := int64(2), atomic.LoadInt64(&handler.live); want != got {
		t.Errorf("wanted %d connections at first, got %d", want, got)
	}

	// Close one connection. Wait for its context to be canceled.
	con2.Close()
	for {
		if atomic.LoadInt64(&handler.live) == 1 {
			break
		}
	}

	// Cancel the entire server context. Wait for the other connection's context to
	// be canceled as well.
	ctxC()
	for {
		if atomic.LoadInt64(&handler.live) == 0 {
			break
		}
	}
}
