// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"testing"

	"go.etcd.io/etcd/client/pkg/v3/testutil"
	"go.etcd.io/etcd/tests/v3/framework/integration"
	"go.uber.org/zap"

	"source.monogon.dev/osbase/logtree"
)

// TestManaged ensures Managed Certificates work, including re-ensuring
// certificates with the same data and issuing subordinate certificates.
func TestManaged(t *testing.T) {
	lt := logtree.New()
	logtree.PipeAllToTest(t, lt)
	tb, cancel := testutil.NewTestingTBProthesis("pki-managed")
	defer cancel()
	cluster := integration.NewCluster(tb, &integration.ClusterConfig{
		Size: 1,
		LoggerBuilder: func(memberName string) *zap.Logger {
			dn := logtree.DN("etcd." + memberName)
			return logtree.Zapify(lt.MustLeveledFor(dn), zap.WarnLevel)
		},
	})
	cl := cluster.Client(0)
	defer cluster.Terminate(tb)
	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()
	ns := Namespaced("/test-managed/")

	// Test CA certificate issuance.
	ca := &Certificate{
		Namespace: &ns,
		Issuer:    SelfSigned,
		Name:      "ca",
		Template:  CA("Test CA"),
	}
	caBytes, err := ca.Ensure(ctx, cl)
	if err != nil {
		t.Fatalf("Failed to Ensure CA: %v", err)
	}
	caCert, err := x509.ParseCertificate(caBytes)
	if err != nil {
		t.Fatalf("Failed to parse newly emited CA cert: %v", err)
	}
	if !caCert.IsCA {
		t.Errorf("Newly emitted CA cert is not CA")
	}
	if ca.PublicKey == nil {
		t.Errorf("Newly emitted CA cert has no public key")
	}
	if ca.PrivateKey == nil {
		t.Errorf("Newly emitted CA cert has no public key")
	}

	// Re-emitting CA certificate with same parameters should return exact same
	// data.
	ca2 := &Certificate{
		Namespace: &ns,
		Issuer:    SelfSigned,
		Name:      "ca",
		Template:  CA("Test CA"),
	}
	caBytes2, err := ca2.Ensure(ctx, cl)
	if err != nil {
		t.Fatalf("Failed to re-Ensure CA: %v", err)
	}
	if !bytes.Equal(caBytes, caBytes2) {
		t.Errorf("New CA has different x509 certificate")
	}
	if !bytes.Equal(ca.PublicKey, ca2.PublicKey) {
		t.Errorf("New CA has different public key")
	}
	if !bytes.Equal(ca.PrivateKey, ca2.PrivateKey) {
		t.Errorf("New CA has different private key")
	}

	// Emitting a subordinate certificate should work.
	client := &Certificate{
		Namespace: &ns,
		Issuer:    ca2,
		Name:      "client",
		Template:  Client("foo", nil),
	}
	clientBytes, err := client.Ensure(ctx, cl)
	if err != nil {
		t.Fatalf("Failed to ensure client certificate: %v", err)
	}
	clientCert, err := x509.ParseCertificate(clientBytes)
	if err != nil {
		t.Fatalf("Failed to parse newly emitted client certificate: %v", err)
	}
	if clientCert.IsCA {
		t.Errorf("New client cert is CA")
	}
	if want, got := "foo", clientCert.Subject.CommonName; want != got {
		t.Errorf("New client CN should be %q, got %q", want, got)
	}
	if want, got := caCert.Subject.String(), clientCert.Issuer.String(); want != got {
		t.Errorf("New client issuer should be %q, got %q", want, got)
	}
}

// TestExternal ensures External certificates work correctly, including
// re-Ensuring certificates with the same public key, and attempting to re-issue
// the same certificate with a different public key (which should fail).
func TestExternal(t *testing.T) {
	lt := logtree.New()
	logtree.PipeAllToTest(t, lt)
	tb, cancel := testutil.NewTestingTBProthesis("pki-managed")
	defer cancel()
	cluster := integration.NewCluster(tb, &integration.ClusterConfig{
		Size: 1,
		LoggerBuilder: func(memberName string) *zap.Logger {
			dn := logtree.DN("etcd." + memberName)
			return logtree.Zapify(lt.MustLeveledFor(dn), zap.WarnLevel)
		},
	})
	cl := cluster.Client(0)
	defer cluster.Terminate(tb)
	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()
	ns := Namespaced("/test-external/")

	ca := &Certificate{
		Namespace: &ns,
		Issuer:    SelfSigned,
		Name:      "ca",
		Template:  CA("Test CA"),
	}

	// Issuing an external certificate should work.
	pk, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	server := &Certificate{
		Namespace: &ns,
		Issuer:    ca,
		Name:      "server",
		Template:  Server([]string{"server"}, nil),
		Mode:      CertificateExternal,
		PublicKey: pk,
	}
	serverBytes, err := server.Ensure(ctx, cl)
	if err != nil {
		t.Fatalf("Failed to Ensure server certificate: %v", err)
	}

	// Issuing an external certificate with the same name but different public key
	// should fail.
	pk2, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	server2 := &Certificate{
		Namespace: &ns,
		Issuer:    ca,
		Name:      "server",
		Template:  Server([]string{"server"}, nil),
		Mode:      CertificateExternal,
		PublicKey: pk2,
	}
	if _, err := server2.Ensure(ctx, cl); err == nil {
		t.Fatalf("Issuing server certificate with different public key should have failed")
	}

	// Issuing the external certificate with the same name and same public key
	// should work and yield the same x509 bytes.
	server3 := &Certificate{
		Namespace: &ns,
		Issuer:    ca,
		Name:      "server",
		Template:  Server([]string{"server"}, nil),
		Mode:      CertificateExternal,
		PublicKey: pk,
	}
	serverBytes3, err := server3.Ensure(ctx, cl)
	if err != nil {
		t.Fatalf("Failed to re-Ensure server certificate: %v", err)
	}
	if !bytes.Equal(serverBytes, serverBytes3) {
		t.Errorf("New server certificate has different x509 certificate")
	}
}
