// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package consensus

import (
	"crypto/ed25519"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/server/v3/embed"

	"source.monogon.dev/metropolis/node"
	"source.monogon.dev/metropolis/node/core/localstorage"
	"source.monogon.dev/osbase/pki"
)

// Config describes the startup configuration of a consensus instance.
type Config struct {
	// Data directory (persistent, encrypted storage) for etcd.
	Data *localstorage.DataEtcdDirectory
	// Ephemeral directory for etcd.
	Ephemeral *localstorage.EphemeralConsensusDirectory

	// JoinCluster is set if this instance is to join an existing cluster for the
	// first time. If not set, it's assumed this instance has ran before and has all
	// the state on disk required to become part of whatever cluster it was before.
	// If that data is not present, a new cluster will be bootstrapped.
	JoinCluster *JoinCluster

	// NodeID is the node ID, which is also used to identify consensus nodes.
	NodeID string

	// NodePrivateKey is the node's main private key which is also used for
	// Metropolis PKI. The same key will be used for consensus nodes, but
	// different certificates will be used.
	NodePrivateKey ed25519.PrivateKey

	testOverrides testOverrides
}

// JoinCluster is all the data required for a node to join (for the first time)
// an already running cluster. This data is available from an already running
// consensus member by performing AddNode, which is called by the Curator when
// new etcd nodes are added to the cluster.
type JoinCluster struct {
	CACertificate   *x509.Certificate
	NodeCertificate *x509.Certificate
	// ExistingNodes are an arbitrarily ordered list of other consensus members that
	// the node should attempt to contact.
	ExistingNodes []ExistingNode
	// InitialCRL is a certificate revocation list for this cluster. After the node
	// starts, a CRL on disk will be maintained reflecting the PKI state within etcd.
	InitialCRL *pki.CRL
}

// ExistingNode is the peer URL and name of an already running consensus instance.
type ExistingNode struct {
	Name string
	URL  string
}

func (e *ExistingNode) connectionString() string {
	return fmt.Sprintf("%s=%s", e.Name, e.URL)
}

func (c *Config) nodePublicKey() ed25519.PublicKey {
	return c.NodePrivateKey.Public().(ed25519.PublicKey)
}

// testOverrides are available to test code to make some things easier in a test
// environment.
type testOverrides struct {
	// externalPort overrides the default port used by the node.
	externalPort int
	// externalAddress overrides the address of the node, which is usually its ID.
	externalAddress string
	// etcdMetricsPort overrides the default etcd metrics port used by the node.
	etcdMetricsPort int
}

// build takes a Config and returns an etcd embed.Config.
//
// enablePeers selects whether the etcd instance will listen for peer traffic
// over TLS. This requires TLS credentials to be present on disk, and will be
// disabled for bootstrapping the instance.
func (c *Config) build(enablePeers bool) *embed.Config {
	port := int(node.ConsensusPort)
	if p := c.testOverrides.externalPort; p != 0 {
		port = p
	}
	host := c.NodeID
	if c.testOverrides.externalAddress != "" {
		host = c.testOverrides.externalAddress
	}
	etcdPort := int(node.MetricsEtcdListenerPort)
	if p := c.testOverrides.etcdMetricsPort; p != 0 {
		etcdPort = p
	}

	cfg := embed.NewConfig()

	cfg.Name = c.NodeID
	cfg.ClusterState = "existing"
	cfg.InitialClusterToken = "METROPOLIS"
	cfg.Logger = "zap"
	cfg.LogOutputs = []string{c.Ephemeral.ServerLogsFIFO.FullPath()}
	cfg.EnableGRPCGateway = false
	cfg.GRPCOnly = true
	cfg.ListenMetricsUrls = []url.URL{
		{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", etcdPort))},
	}

	cfg.Dir = c.Data.Data.FullPath()

	// Client URL, ie. local UNIX socket to listen on for trusted, unauthenticated
	// traffic.
	cfg.ListenClientUrls = []url.URL{{
		Scheme: "unix",
		Path:   c.Ephemeral.ClientSocket.FullPath() + ":0",
	}}

	if enablePeers {
		cfg.PeerTLSInfo.CertFile = c.Data.PeerPKI.Certificate.FullPath()
		cfg.PeerTLSInfo.KeyFile = c.Data.PeerPKI.Key.FullPath()
		cfg.PeerTLSInfo.TrustedCAFile = c.Data.PeerPKI.CACertificate.FullPath()
		cfg.PeerTLSInfo.ClientCertAuth = true
		cfg.PeerTLSInfo.CRLFile = c.Data.PeerCRL.FullPath()

		cfg.ListenPeerUrls = []url.URL{{
			Scheme: "https",
			Host:   fmt.Sprintf("[::]:%d", port),
		}}
		cfg.AdvertisePeerUrls = []url.URL{{
			Scheme: "https",
			Host:   net.JoinHostPort(host, strconv.Itoa(port)),
		}}
	} else {
		// When not enabling peer traffic, listen on loopback. We would not listen at
		// all, but etcd seems to prevent us from doing that.
		cfg.ListenPeerUrls = []url.URL{{
			Scheme: "http",
			Host:   fmt.Sprintf("127.0.0.1:%d", port),
		}}
		cfg.AdvertisePeerUrls = []url.URL{{
			Scheme: "http",
			Host:   fmt.Sprintf("127.0.0.1:%d", port),
		}}
	}

	cfg.InitialCluster = cfg.InitialClusterFromName(c.NodeID)
	if c.JoinCluster != nil {
		for _, n := range c.JoinCluster.ExistingNodes {
			cfg.InitialCluster += "," + n.connectionString()
		}
	}
	return cfg
}

// localClient returns an etcd client connected to the socket as configured in
// Config.
func (c *Config) localClient() (*clientv3.Client, error) {
	socket := c.Ephemeral.ClientSocket.FullPath()
	return clientv3.New(clientv3.Config{
		Endpoints:   []string{fmt.Sprintf("unix://%s:0", socket)},
		DialTimeout: 2 * time.Second,
	})
}
