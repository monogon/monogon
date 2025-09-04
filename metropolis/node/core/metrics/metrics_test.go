// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	apb "source.monogon.dev/metropolis/node/core/curator/proto/api"

	"source.monogon.dev/metropolis/node/allocs"
	"source.monogon.dev/metropolis/test/util"
	"source.monogon.dev/osbase/supervisor"
	"source.monogon.dev/osbase/test/freeport"
)

var (
	// These are filled by bazel at linking time with the canonical path of
	// their corresponding file. Inside the init function we resolve it
	// with the rules_go runfiles package to the real path.
	xFakeExporterPath string
)

func init() {
	var err error
	for _, path := range []*string{
		&xFakeExporterPath,
	} {
		*path, err = runfiles.Rlocation(*path)
		if err != nil {
			panic(err)
		}
	}
}

func fakeExporter(name, value string) *Exporter {
	p, closer, err := freeport.AllocateTCPPort()
	if err != nil {
		panic(err)
	}
	defer closer.Close()
	port := allocs.Port(p)

	return &Exporter{
		Name:       name,
		Port:       port,
		Executable: xFakeExporterPath,
		Arguments: []string{
			"-listen", "127.0.0.1:" + port.PortString(),
			"-value", value,
		},
	}
}

// TestMetricsForwarder exercises the metrics forwarding functionality of the
// metrics service. That is, it makes sure that the service starts some fake
// exporters and then forwards HTTP traffic to them.
func TestMetricsForwarder(t *testing.T) {
	exporters := []*Exporter{
		fakeExporter("test1", "100"),
		fakeExporter("test2", "200"),
	}

	eph := util.NewEphemeralClusterCredentials(t, 1)

	svc := Service{
		Credentials: eph.Nodes[0],
		Exporters:   exporters,

		enableDynamicAddr: true,
		dynamicAddr:       make(chan string),
	}

	supervisor.TestHarness(t, svc.Run)
	addr := <-svc.dynamicAddr

	pool := x509.NewCertPool()
	pool.AddCert(eph.CA)

	cl := http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				ServerName: eph.Nodes[0].ID(),
				RootCAs:    pool,

				Certificates: []tls.Certificate{eph.Manager},
			},
		},
	}

	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()

	util.TestEventual(t, "retrieve-test1", ctx, 10*time.Second, func(ctx context.Context) error {
		url := (&url.URL{
			Scheme: "https",
			Host:   addr,
			Path:   "/metrics/test1",
		}).String()
		req, _ := http.NewRequest("GET", url, nil)
		res, err := cl.Do(req)
		if err != nil {
			return fmt.Errorf("Get(%q): %w", url, err)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			return fmt.Errorf("Get(%q): code %d", url, res.StatusCode)
		}
		body, _ := io.ReadAll(res.Body)
		want := "test 100"
		if !strings.Contains(string(body), want) {
			return util.Permanent(fmt.Errorf("did not find expected value %q in %q", want, string(body)))
		}
		return nil
	})
	util.TestEventual(t, "retrieve-test2", ctx, 10*time.Second, func(ctx context.Context) error {
		url := (&url.URL{
			Scheme: "https",
			Host:   addr,
			Path:   "/metrics/test2",
		}).String()
		req, _ := http.NewRequest("GET", url, nil)
		res, err := cl.Do(req)
		if err != nil {
			return fmt.Errorf("Get(%q): %w", url, err)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			return fmt.Errorf("Get(%q): code %d", url, res.StatusCode)
		}
		body, _ := io.ReadAll(res.Body)
		want := "test 200"
		if !strings.Contains(string(body), want) {
			return util.Permanent(fmt.Errorf("did not find expected value %q in %q", want, string(body)))
		}
		return nil
	})
}

func TestDiscovery(t *testing.T) {
	eph := util.NewEphemeralClusterCredentials(t, 1)

	curator, ccl := util.MakeTestCurator(t)
	defer ccl.Close()

	svc := Service{
		Credentials:       eph.Nodes[0],
		Discovery:         Discovery{Curator: apb.NewCuratorClient(ccl)},
		enableDynamicAddr: true,
		dynamicAddr:       make(chan string),
	}

	enableDiscovery := make(chan bool)
	supervisor.TestHarness(t, func(ctx context.Context) error {
		if err := supervisor.Run(ctx, "metrics", svc.Run); err != nil {
			return err
		}

		err := supervisor.Run(ctx, "discovery", func(ctx context.Context) error {
			<-enableDiscovery
			return svc.Discovery.Run(ctx)
		})
		if err != nil {
			return err
		}

		supervisor.Signal(ctx, supervisor.SignalHealthy)
		supervisor.Signal(ctx, supervisor.SignalDone)
		return nil
	})

	addr := <-svc.dynamicAddr

	pool := x509.NewCertPool()
	pool.AddCert(eph.CA)

	cl := http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				ServerName: eph.Nodes[0].ID(),
				RootCAs:    pool,

				Certificates: []tls.Certificate{eph.Manager},
			},
		},
	}

	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()

	util.TestEventual(t, "inactive-discovery", ctx, 10*time.Second, func(ctx context.Context) error {
		url := (&url.URL{
			Scheme: "https",
			Host:   addr,
			Path:   "/discovery",
		}).String()
		req, _ := http.NewRequest("GET", url, nil)
		res, err := cl.Do(req)
		if err != nil {
			return fmt.Errorf("Get(%q): %w", url, err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusServiceUnavailable {
			return fmt.Errorf("Get(%q): code %d", url, res.StatusCode)
		}
		return nil
	})

	// First start the watcher, create a fake node after that
	enableDiscovery <- true
	curator.NodeWithPrefixes(wgtypes.Key{}, "metropolis-fake-1", "1.2.3.4")

	util.TestEventual(t, "active-discovery", ctx, 10*time.Second, func(ctx context.Context) error {
		url := (&url.URL{
			Scheme: "https",
			Host:   addr,
			Path:   "/discovery",
		}).String()
		req, _ := http.NewRequest("GET", url, nil)
		res, err := cl.Do(req)
		if err != nil {
			return fmt.Errorf("Get(%q): %w", url, err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("Get(%q): code %d", url, res.StatusCode)
		}
		body, _ := io.ReadAll(res.Body)
		want := `[{"targets":["1.2.3.4"],"labels":{"__meta_metropolis_node":"metropolis-fake-1","__meta_metropolis_role_consensus_member":"true","__meta_metropolis_role_kubernetes_controller":"false","__meta_metropolis_role_kubernetes_worker":"false"}}]`
		if !strings.Contains(string(body), want) {
			return util.Permanent(fmt.Errorf("did not find expected value %q in %q", want, string(body)))
		}
		return nil
	})
}
