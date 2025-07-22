// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/cenkalti/backoff/v4"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"

	apb "source.monogon.dev/cloud/agent/api"

	"source.monogon.dev/metropolis/node/core/devmgr"
	"source.monogon.dev/metropolis/node/core/network"
	"source.monogon.dev/osbase/bringup"
	"source.monogon.dev/osbase/pki"
	"source.monogon.dev/osbase/supervisor"
)

func main() {
	bringup.Runnable(agentRunnable).Run()
}

// This is similar to rpc.NewEphemeralCredentials, but that only deals with
// Metropolis-style certificate verification.
func newEphemeralCert(private ed25519.PrivateKey) (*tls.Certificate, error) {
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now(),
		NotAfter:     pki.UnknownNotAfter,

		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	certificateBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, private.Public(), private)
	if err != nil {
		return nil, fmt.Errorf("when generating self-signed certificate: %w", err)
	}
	return &tls.Certificate{
		Certificate: [][]byte{certificateBytes},
		PrivateKey:  private,
	}, nil
}

// Main runnable for the agent.
func agentRunnable(ctx context.Context) error {
	l := supervisor.Logger(ctx)
	agentInitRaw, err := os.ReadFile("/init.pb")
	if err != nil {
		return fmt.Errorf("unable to read spec file from takeover: %w", err)
	}

	var agentInit apb.AgentInit
	if err := proto.Unmarshal(agentInitRaw, &agentInit); err != nil {
		return fmt.Errorf("unable to parse spec file from takeover: %w", err)
	}
	l.Info("Monogon BMaaS Agent started")
	if agentInit.TakeoverInit == nil {
		return errors.New("AgentInit takeover_init field is unset, this is not allowed")
	}

	devmgrSvc := devmgr.New()
	supervisor.Run(ctx, "devmgr", devmgrSvc.Run)

	networkSvc := network.New(agentInit.NetworkConfig, nil, nil)
	networkSvc.DHCPVendorClassID = "dev.monogon.cloud.agent.v1"
	supervisor.Run(ctx, "networking", networkSvc.Run)
	l.Info("Started networking")

	ephemeralCert, err := newEphemeralCert(agentInit.PrivateKey)
	if err != nil {
		return fmt.Errorf("could not generate ephemeral credentials: %w", err)
	}
	var rootCAs *x509.CertPool
	if len(agentInit.TakeoverInit.CaCertificate) != 0 {
		caCert, err := x509.ParseCertificate(agentInit.TakeoverInit.CaCertificate)
		if err != nil {
			return fmt.Errorf("unable to parse supplied ca_certificate, is it in DER format?")
		}
		rootCAs = x509.NewCertPool()
		rootCAs.AddCert(caCert)
	}

	conn, err := grpc.NewClient(agentInit.TakeoverInit.BmaasEndpoint, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{*ephemeralCert},
		RootCAs:      rootCAs,
	})))
	if err != nil {
		return fmt.Errorf("error creating BMaaS gRPC client: %w", err)
	}
	c := apb.NewAgentCallbackClient(conn)

	supervisor.Signal(ctx, supervisor.SignalHealthy)

	assembleHWReport := func() *apb.AgentHardwareReport {
		report, warnings := gatherHWReport()
		var warningStrings []string
		for _, w := range warnings {
			l.Warningf("Hardware Report Warning: %v", w)
			warningStrings = append(warningStrings, w.Error())
		}
		return &apb.AgentHardwareReport{
			Report:  report,
			Warning: warningStrings,
		}
	}

	var sentFirstHeartBeat, hwReportSent bool
	var installationReport *apb.OSInstallationReport
	var installationGeneration int64
	b := backoff.NewExponentialBackOff()
	// Never stop retrying, there is nothing else to do
	b.MaxElapsedTime = 0
	// Main heartbeat loop
	for {
		req := apb.HeartbeatRequest{
			MachineId: agentInit.TakeoverInit.MachineId,
		}
		if sentFirstHeartBeat && !hwReportSent {
			req.HardwareReport = assembleHWReport()
		}
		if installationReport != nil {
			req.InstallationReport = installationReport
		}
		reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		res, err := c.Heartbeat(reqCtx, &req)
		cancel()
		if err != nil {
			l.Infof("Heartbeat failed: %v", err)
			time.Sleep(b.NextBackOff())
			continue
		}
		b.Reset()
		sentFirstHeartBeat = true
		if req.HardwareReport != nil {
			hwReportSent = true
		}
		if installationReport != nil {
			l.Infof("Installation report sent successfully, rebooting")
			// Close connection and wait 1s to make sure that the RST
			// can be sent. Important for QEMU/slirp where not doing this
			// triggers bugs in the connection state management, but also
			// nice for reducing the number of stale connections in the API
			// server.
			conn.Close()
			time.Sleep(1 * time.Second)
			unix.Sync()
			unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
		}
		if res.InstallationRequest != nil {
			if res.InstallationRequest.Generation == installationGeneration {
				// This installation request has already been attempted
				continue
			}
			installationReport = &apb.OSInstallationReport{
				Generation: res.InstallationRequest.Generation,
			}
			installCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
			if err := install(installCtx, res.InstallationRequest, agentInit.NetworkConfig); err != nil {
				l.Errorf("Installation failed: %v", err)
				installationReport.Result = &apb.OSInstallationReport_Error_{
					Error: &apb.OSInstallationReport_Error{
						Error: err.Error(),
					},
				}
			} else {
				l.Info("Installation succeeded")
				installationReport.Result = &apb.OSInstallationReport_Success_{
					Success: &apb.OSInstallationReport_Success{},
				}
			}
			cancel()
		} else {
			time.Sleep(30 * time.Second)
		}
	}
}
