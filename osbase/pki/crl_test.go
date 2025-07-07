// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"context"
	"crypto/x509"
	"testing"

	"go.etcd.io/etcd/client/pkg/v3/testutil"
	"go.etcd.io/etcd/tests/v3/framework/integration"
)

// TestRevoke exercises the CRL revocation and watching functionality of a CA
// certificate.
func TestRevoke(t *testing.T) {
	tb, cancel := testutil.NewTestingTBProthesis("pki-revoke")
	defer cancel()
	cluster := integration.NewCluster(tb, &integration.ClusterConfig{
		Size: 1,
	})
	cl := cluster.Client(0)
	defer cluster.Terminate(tb)
	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()
	ns := Namespaced("/test-managed/")

	ca := &Certificate{
		Namespace: &ns,
		Issuer:    SelfSigned,
		Name:      "ca",
		Template:  CA("Test CA"),
	}
	sub := &Certificate{
		Namespace: &ns,
		Issuer:    ca,
		Name:      "sub",
		Template:  Server([]string{"server"}, nil),
	}

	caCertBytes, err := ca.Ensure(ctx, cl)
	if err != nil {
		t.Fatalf("Ensuring ca certificate failed: %v", err)
	}
	caCert, err := x509.ParseCertificate(caCertBytes)
	if err != nil {
		t.Fatalf("Loading newly emitted CA certificate failed: %v", err)
	}

	subCertBytes, err := sub.Ensure(ctx, cl)
	if err != nil {
		t.Fatalf("Ensuring sub certificate failed: %v", err)
	}
	subCert, err := x509.ParseCertificate(subCertBytes)
	if err != nil {
		t.Fatalf("Loading newly emitted sub certificate failed: %v", err)
	}

	// Ensure CRL is correctly signed and that subCert is not yet on it.
	crlW := ca.WatchCRL(cl)
	crl, err := crlW.Get(ctx)
	if err != nil {
		t.Fatalf("Retrieving initial CRL failed: %v", err)
	}
	if err := caCert.CheckCRLSignature(crl.List); err != nil {
		t.Fatalf("Initial CRL not signed by CA: %v", err)
	}
	for _, el := range crl.List.TBSCertList.RevokedCertificates {
		if el.SerialNumber.Cmp(subCert.SerialNumber) == 0 {
			t.Fatalf("Newly emitted certificate is already on CRL.")
		}
	}

	// Emit yet another certificate. Also shouldn't be on CRL.
	bad := &Certificate{
		Namespace: &ns,
		Issuer:    ca,
		Name:      "bad",
		Template:  Server([]string{"badserver"}, nil),
	}
	badCertBytes, err := bad.Ensure(ctx, cl)
	if err != nil {
		t.Fatalf("Ensuring bad certificate failed: %v", err)
	}
	badCert, err := x509.ParseCertificate(badCertBytes)
	if err != nil {
		t.Fatalf("Loading newly emitted bad certificate failed: %v", err)
	}
	for _, el := range crl.List.TBSCertList.RevokedCertificates {
		if el.SerialNumber.Cmp(badCert.SerialNumber) == 0 {
			t.Fatalf("Newly emitted bad certificate is already on CRL.")
		}
	}

	// Revoke bad certificate. Should now be present in CRL.
	if err := ca.Revoke(ctx, cl, "badserver"); err != nil {
		t.Fatalf("Revoke failed: %v", err)
	}
	// Get in a loop until found.
	for {
		crl, err = crlW.Get(ctx)
		if err != nil {
			t.Fatalf("Get failed: %v", err)
		}
		found := false
		for _, el := range crl.List.TBSCertList.RevokedCertificates {
			if el.SerialNumber.Cmp(badCert.SerialNumber) == 0 {
				found = true
			}
			if el.SerialNumber.Cmp(subCert.SerialNumber) == 0 {
				t.Errorf("Found non-revoked cert in CRL")
			}
		}
		if found {
			break
		}
	}
	// Now revoke first certificate. Both should be now present in CRL.
	if err := ca.Revoke(ctx, cl, "server"); err != nil {
		t.Fatalf("Revoke failed: %v", err)
	}
	// Get in a loop until found.
	for {
		crl, err = crlW.Get(ctx)
		if err != nil {
			t.Fatalf("Get failed: %v", err)
		}
		foundSub := false
		foundBad := false
		for _, el := range crl.List.TBSCertList.RevokedCertificates {
			if el.SerialNumber.Cmp(badCert.SerialNumber) == 0 {
				foundBad = true
			}
			if el.SerialNumber.Cmp(subCert.SerialNumber) == 0 {
				foundSub = true
			}
		}
		if foundBad && foundSub {
			break
		}
	}
}
