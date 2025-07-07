// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Package consensus implements a runnable that manages an etcd instance which
// forms part of a Metropolis etcd cluster. This cluster is a foundational
// building block of Metropolis and its startup/management sequencing needs to
// be as robust as possible.
//
// Cluster Structure
//
// Each etcd instance listens for two kinds of traffic:
//
// 1. Peer traffic over TLS on a TCP port of the node's main interface. This is
// where other etcd instances connect to to exchange peer traffic, perform
// transactions and build quorum. The TLS credentials are stored in a PKI that
// is managed internally by the consensus runnable, with its state stored in
// etcd itself.
//
// 2. Client traffic over a local domain socket, with access control based on
// standard Linux user/group permissions. Currently this allows any code running
// as root on the host namespace full access to the etcd cluster.
//
// This means that if code running on a node wishes to perform etcd
// transactions, it must also run an etcd instance. This colocation of all
// direct etcd access and the etcd intances themselves effectively delegate all
// Metropolis control plane functionality to whatever subset of nodes is running
// consensus and all codes that connects to etcd directly (the Curator).
//
// For example, if nodes foo and bar are parts of the control plane, but node
// worker is not:
//
//   .---------------------.
//   | node-foo            |
//   |---------------------|
//   | .--------------------.
//   | | etcd               |<---etcd/TLS--.   (node.ConsensusPort)
//   | '--------------------'              |
//   |     ^ Domain Socket |               |
//   |     | etcd/plain    |               |
//   | .--------------------.              |
//   | | curator            |<---gRPC/TLS----. (node.CuratorServicePort)
//   | '--------------------'              | |
//   |     ^ Domain Socket |               | |
//   |     | gRPC/plain    |               | |
//   | .-----------------. |               | |
//   | | node logic      | |               | |
//   | '-----------------' |               | |
//   '---------------------'               | |
//                                         | |
//   .---------------------.               | |
//   | node-baz            |               | |
//   |---------------------|               | |
//   | .--------------------.              | |
//   | | etcd               |<-------------' |
//   | '--------------------'                |
//   |     ^ Domain Socket |                 |
//   |     | gRPC/plain    |                 |
//   | .--------------------.                |
//   | | curator            |<---gRPC/TLS----:
//   | '--------------------'                |
//   |    ...              |                 |
//   '---------------------'                 |
//                                           |
//   .---------------------.                 |
//   | node-worker         |                 |
//   |---------------------|                 |
//   | .-----------------. |                 |
//   | | node logic      |-------------------'
//   | '-----------------' |
//   '---------------------'
//

package consensus

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"time"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/server/v3/embed"

	"source.monogon.dev/metropolis/node/core/consensus/client"
	"source.monogon.dev/osbase/event"
	"source.monogon.dev/osbase/event/memory"
	"source.monogon.dev/osbase/logtree/unraw"
	"source.monogon.dev/osbase/pki"
	"source.monogon.dev/osbase/supervisor"
)

var (
	pkiNamespace = pki.Namespaced("/pki/")
)

func pkiCA() *pki.Certificate {
	return &pki.Certificate{
		Name:      "CA",
		Namespace: &pkiNamespace,
		Issuer:    pki.SelfSigned,
		Template: x509.Certificate{
			SerialNumber: big.NewInt(1),
			Subject: pkix.Name{
				CommonName: "Metropolis etcd CA Certificate",
			},
			IsCA:        true,
			KeyUsage:    x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageOCSPSigning},
		},
	}
}

func pkiPeerCertificate(nodeID string, extraNames []string) x509.Certificate {
	return x509.Certificate{
		Subject: pkix.Name{
			CommonName: nodeID,
		},
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth,
		},
		DNSNames: append(extraNames, nodeID),
	}
}

// GetEtcdMemberNodeId returns the node ID of an etcd member. It works even for
// members which have not started, where member.Name is empty.
func GetEtcdMemberNodeId(member *etcdserverpb.Member) string {
	if member.Name != "" {
		return member.Name
	}
	if len(member.PeerURLs) == 0 {
		return ""
	}
	u, err := url.Parse(member.PeerURLs[0])
	if err != nil {
		return ""
	}
	nodeId, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		return ""
	}
	return nodeId
}

// Service is the etcd cluster member service. See package-level documentation
// for more information.
type Service struct {
	config *Config

	value memory.Value[*Status]
	ca    *pki.Certificate
}

func New(config Config) *Service {
	return &Service{
		config: &config,
	}
}

// Run is a Supervisor runnable that starts the etcd member service. It will
// become healthy once the member joins the cluster successfully.
func (s *Service) Run(ctx context.Context) error {
	// Always re-create CA to make sure we don't have PKI state from previous runs.
	//
	// TODO(q3k): make the PKI library immune to this misuse.
	s.ca = pkiCA()

	// Create log converter. This will ingest etcd logs and pipe them out to this
	// runnable's leveled logging facilities.

	// This is not where etcd will run, but where its log ingestion machinery lives.
	// This ensures that the (annoying verbose) etcd logs are contained into just
	// .etcd.
	err := supervisor.Run(ctx, "etcd", func(ctx context.Context) error {
		converter := unraw.Converter{
			Parser:            parseEtcdLogEntry,
			MaximumLineLength: 8192,
			LeveledLogger:     supervisor.Logger(ctx),
		}
		pipe, err := converter.NamedPipeReader(s.config.Ephemeral.ServerLogsFIFO.FullPath())
		if err != nil {
			return fmt.Errorf("when creating pipe reader: %w", err)
		}
		if err := supervisor.Run(ctx, "piper", pipe); err != nil {
			return fmt.Errorf("when starting log piper: %w", err)
		}
		supervisor.Signal(ctx, supervisor.SignalHealthy)
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil {
		return fmt.Errorf("when starting etcd logger: %w", err)
	}

	// Create autopromoter, which will automatically promote all learners to full
	// etcd members.
	if err := supervisor.Run(ctx, "autopromoter", s.autopromoter); err != nil {
		return fmt.Errorf("when starting autopromtoer: %w", err)
	}

	// Create selfupdater, which will perform a one-shot update of this member's
	// peer address in etcd.
	if err := supervisor.Run(ctx, "selfupdater", s.selfupdater); err != nil {
		return fmt.Errorf("when starting selfupdater: %w", err)
	}

	// Prepare cluster PKI credentials.
	ppki := s.config.Data.PeerPKI
	jc := s.config.JoinCluster
	if jc != nil {
		supervisor.Logger(ctx).Info("JoinCluster set, writing PPKI data to disk...")
		// For nodes that join an existing cluster, or re-join it, always write whatever
		// we've been given on startup.
		if err := ppki.WriteAll(jc.NodeCertificate.Raw, s.config.NodePrivateKey, jc.CACertificate.Raw); err != nil {
			return fmt.Errorf("when writing credentials for join: %w", err)
		}
		if err := s.config.Data.PeerCRL.Write(jc.InitialCRL.Raw, 0400); err != nil {
			return fmt.Errorf("when writing CRL for join: %w", err)
		}
	} else {
		// For other nodes, we should already have credentials from a previous join, or
		// a previous bootstrap. If none exist, assume we need to bootstrap these
		// credentials.
		//
		// TODO(q3k): once we have node join (ie. node restart from disk) flow, add a
		// special configuration marker to prevent spurious bootstraps.
		absent, err := ppki.AllAbsent()
		if err != nil {
			return fmt.Errorf("when checking for PKI file absence: %w", err)
		}
		if absent {
			supervisor.Logger(ctx).Info("PKI data absent, bootstrapping.")
			if err := s.bootstrap(ctx); err != nil {
				return fmt.Errorf("bootstrap failed: %w", err)
			}
		} else {
			supervisor.Logger(ctx).Info("PKI data present, not bootstrapping.")
		}
	}

	// Start etcd ...
	supervisor.Logger(ctx).Infof("Starting etcd...")
	cfg := s.config.build(true)
	server, err := embed.StartEtcd(cfg)
	if err != nil {
		return fmt.Errorf("when starting etcd: %w", err)
	}

	// ... wait for server to be ready...
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-server.Server.ReadyNotify():
	}

	// ... build a client to its' socket...
	cl, err := s.config.localClient()
	if err != nil {
		return fmt.Errorf("getting local client failed: %w", err)
	}

	// ... and wait until we're not a learner anymore.
	for {
		members, err := cl.MemberList(ctx)
		if err != nil {
			supervisor.Logger(ctx).Warningf("MemberList failed: %v", err)
			time.Sleep(time.Second)
			continue
		}

		isMember := false
		for _, member := range members.Members {
			if member.ID != uint64(server.Server.MemberID()) {
				continue
			}
			if !member.IsLearner {
				isMember = true
				break
			}
		}
		if isMember {
			break
		}
		supervisor.Logger(ctx).Warningf("Still a learner, waiting...")
		time.Sleep(time.Second)
	}

	// All done! Report status.
	supervisor.Logger(ctx).Infof("etcd server ready")

	st := &Status{
		localPeerURL:  cfg.AdvertisePeerUrls[0].String(),
		localMemberID: uint64(server.Server.MemberID()),
		cl:            cl,
		ca:            s.ca,
	}
	st2 := *st
	s.value.Set(&st2)

	// Wait until server dies for whatever reason, update status when that
	// happens.
	supervisor.Signal(ctx, supervisor.SignalHealthy)
	select {
	case err = <-server.Err():
		err = fmt.Errorf("server returned error: %w", err)
	case <-ctx.Done():
		server.Close()
		err = ctx.Err()
	}

	st.stopped = true
	st3 := *st
	s.value.Set(&st3)
	return err
}

func clientFor(kv *clientv3.Client, parts ...string) (client.Namespaced, error) {
	var err error
	namespaced := client.NewLocal(kv)
	for _, el := range parts {
		namespaced, err = namespaced.Sub(el)
		if err != nil {
			return nil, fmt.Errorf("when getting sub client: %w", err)
		}

	}
	return namespaced, nil
}

// bootstrap performs a procedure to resolve the following bootstrap problems:
// in order to start an etcd server for consensus, we need it to serve over TLS.
// However, these TLS certificates also need to be stored in etcd so that
// further certificates can be issued for new nodes.
//
// This was previously solved by a using a special PKI/TLS management system that
// could first create certificates and keys in memory, then only commit them to
// etcd. However, this ended up being somewhat brittle in the face of startup
// sequencing issues, so we're now going with a different approach.
//
// This function starts an etcd instance first without any PKI/TLS support,
// without listening on any external port for peer traffic. Once the instance is
// running, it uses the standard metropolis pki library to create all required
// data directly in the running etcd instance. It then writes all required
// startup data (node private key, member certificate, CA certificate) to disk,
// so that a 'full' etcd instance can be started.
func (s *Service) bootstrap(ctx context.Context) error {
	supervisor.Logger(ctx).Infof("Bootstrapping PKI: starting etcd...")

	cfg := s.config.build(false)
	// This will make etcd create data directories and create a fully new cluster if
	// needed. If we're restarting due to an error, the old cluster data will still
	// exist.
	cfg.ClusterState = "new"

	// Start the bootstrap etcd instance...
	server, err := embed.StartEtcd(cfg)
	if err != nil {
		return fmt.Errorf("failed to start bootstrap etcd: %w", err)
	}
	defer server.Close()

	// ... wait for it to run ...
	select {
	case <-server.Server.ReadyNotify():
	case <-ctx.Done():
		return errors.New("timed out waiting for etcd to become ready")
	}

	// ... create a client to it ...
	cl, err := s.config.localClient()
	if err != nil {
		return fmt.Errorf("when getting bootstrap client: %w", err)
	}

	// ... and build PKI there. This is idempotent, so we will never override
	// anything that's already in the cluster, instead just retrieve it.
	supervisor.Logger(ctx).Infof("Bootstrapping PKI: etcd running, building PKI...")
	clPKI, err := clientFor(cl, "namespaced", "etcd-pki")
	if err != nil {
		return fmt.Errorf("when getting pki client: %w", err)
	}
	defer clPKI.Close()
	caCert, err := s.ca.Ensure(ctx, clPKI)
	if err != nil {
		return fmt.Errorf("failed to ensure CA certificate: %w", err)
	}

	// If we're running with a test overridden external address (eg. localhost), we
	// need to also make that part of the member certificate.
	var extraNames []string
	if external := s.config.testOverrides.externalAddress; external != "" {
		extraNames = []string{external}
	}
	memberTemplate := pki.Certificate{
		Name:      s.config.NodeID,
		Namespace: &pkiNamespace,
		Issuer:    s.ca,
		Template:  pkiPeerCertificate(s.config.NodeID, extraNames),
		Mode:      pki.CertificateExternal,
		PublicKey: s.config.nodePublicKey(),
	}
	memberCert, err := memberTemplate.Ensure(ctx, clPKI)
	if err != nil {
		return fmt.Errorf("failed to ensure member certificate: %w", err)
	}

	// Retrieve CRL.
	crlW := s.ca.WatchCRL(clPKI)
	crl, err := crlW.Get(ctx)
	if err != nil {
		return fmt.Errorf("failed to retrieve initial CRL: %w", err)
	}

	// We have everything we need. Write things to disk.
	supervisor.Logger(ctx).Infof("Bootstrapping PKI: certificates issued, writing to disk...")

	if err := s.config.Data.PeerPKI.WriteAll(memberCert, s.config.NodePrivateKey, caCert); err != nil {
		return fmt.Errorf("failed to write bootstrapped certificates: %w", err)
	}
	if err := s.config.Data.PeerCRL.Write(crl.Raw, 0400); err != nil {
		return fmt.Errorf("failed tow rite CRL: %w", err)
	}

	// Stop the server synchronously (blocking until it's fully shutdown), and
	// return. The caller can now run the 'full' etcd instance with PKI.
	supervisor.Logger(ctx).Infof("Bootstrapping PKI: done, stopping server...")
	server.Close()
	return ctx.Err()
}

// autopromoter is a runnable which repeatedly attempts to promote etcd learners
// in the cluster to full followers. This is needed to bring any new cluster
// members (which are always added as learners) to full membership and make them
// part of the etcd quorum.
func (s *Service) autopromoter(ctx context.Context) error {
	autopromote := func(ctx context.Context, cl *clientv3.Client) {
		// Only autopromote if our endpoint is a leader. This is a bargain bin version
		// of leader election: it's simple and cheap, but not very reliable. The most
		// obvious failure mode is that the instance we contacted isn't a leader by the
		// time we promote a member, but that's fine - the promotion is idempotent. What
		// we really use the 'leader election' here for isn't for consistency, but to
		// prevent the cluster from being hammered by spurious leadership promotion
		// requests from every etcd member.
		status, err := cl.Status(ctx, cl.Endpoints()[0])
		if err != nil {
			supervisor.Logger(ctx).Warningf("Failed to get endpoint status: %v", err)
			return
		}
		if status.Leader != status.Header.MemberId {
			return
		}

		members, err := cl.MemberList(ctx)
		if err != nil {
			supervisor.Logger(ctx).Warningf("Failed to list members: %v", err)
			return
		}
		for _, member := range members.Members {
			if !member.IsLearner {
				continue
			}
			if member.Name == "" {
				// If the name is empty, the member has not started.
				continue
			}
			// Always call PromoteMember since the metadata necessary to decide if we should
			// is private. Luckily etcd already does consistency checks internally and will
			// refuse to promote nodes that aren't connected or are still behind on
			// transactions.
			if _, err := cl.MemberPromote(ctx, member.ID); err != nil {
				supervisor.Logger(ctx).Infof("Failed to promote consensus node %s: %v", member.Name, err)
			} else {
				supervisor.Logger(ctx).Infof("Promoted new consensus node %s", member.Name)
			}
		}
	}

	w := s.value.Watch()
	for {
		st, err := w.Get(ctx)
		if err != nil {
			return fmt.Errorf("status get failed: %w", err)
		}
		t := time.NewTicker(5 * time.Second)
		for {
			autopromote(ctx, st.cl)
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-t.C:
			}
		}
	}
}

func (s *Service) Watch() event.Watcher[*Status] {
	return s.value.Watch()
}

// selfupdater is a runnable that performs a one-shot (once per Service Run,
// thus once for each configuration) update of the node's Peer URL in etcd. This
// is currently only really needed because the first node in the cluster
// bootstraps itself without any peer URLs at first, and this allows it to then
// add the peer URLs afterwards. Instead of a runnable, this might as well have
// been part of the bootstarp logic, but making it a restartable runnable is
// more robust.
func (s *Service) selfupdater(ctx context.Context) error {
	supervisor.Signal(ctx, supervisor.SignalHealthy)
	w := s.value.Watch()
	for {
		st, err := w.Get(ctx)
		if err != nil {
			return fmt.Errorf("failed to get status: %w", err)
		}

		if st.localPeerURL != "" {
			supervisor.Logger(ctx).Infof("Updating local peer URL...")
			peerURL := st.localPeerURL
			if _, err := st.cl.MemberUpdate(ctx, st.localMemberID, []string{peerURL}); err != nil {
				supervisor.Logger(ctx).Warningf("failed to update member: %v", err)
				time.Sleep(1 * time.Second)
				continue
			}
		} else {
			supervisor.Logger(ctx).Infof("No local peer URL, not updating.")
		}

		supervisor.Signal(ctx, supervisor.SignalDone)
		return nil
	}
}
