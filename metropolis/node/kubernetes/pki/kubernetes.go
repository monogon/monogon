// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// package pki builds upon osbase/pki/ to provide an
// etcd-backed implementation of all x509 PKI Certificates/CAs required to run
// Kubernetes.
// Most elements of the PKI are 'static' long-standing certificates/credentials
// stored within etcd. However, this package also provides a method to generate
// 'volatile' (in-memory) certificates/credentials for per-node Kubelets and
// any client certificates.
package pki

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net"

	clientv3 "go.etcd.io/etcd/client/v3"
	"k8s.io/client-go/tools/clientcmd"
	configapi "k8s.io/client-go/tools/clientcmd/api"

	"source.monogon.dev/metropolis/node/allocs"
	"source.monogon.dev/metropolis/node/core/consensus"
	opki "source.monogon.dev/osbase/pki"
)

// KubeCertificateName is an enum-like unique name of a static Kubernetes
// certificate. The value of the name is used as the unique part of an etcd
// path where the certificate and key are stored.
type KubeCertificateName string

const (
	// The main Kubernetes CA, used to authenticate API consumers, and servers.
	IdCA KubeCertificateName = "id-ca"

	// Kubernetes apiserver server certificate.
	APIServer KubeCertificateName = "apiserver"

	// APIServer client certificate used to authenticate to kubelets.
	APIServerKubeletClient KubeCertificateName = "apiserver-kubelet-client"

	// Kubernetes Controller manager client certificate, used to authenticate
	// to the apiserver.
	ControllerManagerClient KubeCertificateName = "controller-manager-client"
	// Kubernetes Controller manager server certificate, used to run its HTTP
	// server.
	ControllerManager KubeCertificateName = "controller-manager"

	// Kubernetes Scheduler client certificate, used to authenticate to the apiserver.
	SchedulerClient KubeCertificateName = "scheduler-client"
	// Kubernetes scheduler server certificate, used to run its HTTP server.
	Scheduler KubeCertificateName = "scheduler"

	// Root-on-kube (system:masters) client certificate. Used to control the
	// apiserver (and resources) by Metropolis internally.
	Master KubeCertificateName = "master"

	// OpenAPI Kubernetes Aggregation CA.
	//   https://kubernetes.io/docs/tasks/extend-kubernetes/configure-aggregation-layer/#ca-reusage-and-conflicts
	AggregationCA    KubeCertificateName = "aggregation-ca"
	FrontProxyClient KubeCertificateName = "front-proxy-client"
	// The Metropolis authentication proxy needs to be able to proxy requests
	// and assert the established identity to the Kubernetes API server.
	MetropolisAuthProxyClient KubeCertificateName = "metropolis-auth-proxy-client"
)

const (
	// etcdPrefix is where all the PKI data is stored in etcd.
	etcdPrefix = "/kube-pki/"
	// serviceAccountKeyName is the etcd path part that is used to store the
	// ServiceAccount authentication secret. This is not a certificate, just an
	// RSA key.
	serviceAccountKeyName = "service-account-privkey"
)

// PKI manages all PKI resources required to run Kubernetes on Metropolis. It
// contains all static certificates, which can be retrieved, or be used to
// generate Kubeconfigs from.
type PKI struct {
	namespace    opki.Namespace
	KV           clientv3.KV
	Certificates map[KubeCertificateName]*opki.Certificate
}

func New(kv clientv3.KV, clusterDomain string) *PKI {
	pki := PKI{
		namespace:    opki.Namespaced(etcdPrefix),
		KV:           kv,
		Certificates: make(map[KubeCertificateName]*opki.Certificate),
	}

	makeCert := func(i, name KubeCertificateName, template x509.Certificate) {
		pki.Certificates[name] = &opki.Certificate{
			Namespace: &pki.namespace,
			Issuer:    pki.Certificates[i],
			Name:      string(name),
			Template:  template,
			Mode:      opki.CertificateManaged,
		}
	}

	pki.Certificates[IdCA] = &opki.Certificate{
		Namespace: &pki.namespace,
		Issuer:    opki.SelfSigned,
		Name:      string(IdCA),
		Template:  opki.CA("Metropolis Kubernetes ID CA"),
		Mode:      opki.CertificateManaged,
	}
	makeCert(IdCA, APIServer, opki.Server(
		[]string{
			"kubernetes",
			"kubernetes.default",
			"kubernetes.default.svc",
			"kubernetes.default.svc." + clusterDomain,
			"localhost",
			// Domain used to access the apiserver by Kubernetes components themselves,
			// without going over Kubernetes networking. This domain only lives as a set of
			// entries in local hostsfiles.
			"metropolis-kube-apiserver",
		},
		// TODO(q3k): add service network internal apiserver address
		[]net.IP{{10, 224, 0, 1}, {127, 0, 0, 1}},
	))
	makeCert(IdCA, APIServerKubeletClient, opki.Client("metropolis:apiserver-kubelet-client", nil))
	makeCert(IdCA, ControllerManagerClient, opki.Client("system:kube-controller-manager", nil))
	makeCert(IdCA, ControllerManager, opki.Server([]string{"kube-controller-manager.local"}, nil))
	makeCert(IdCA, SchedulerClient, opki.Client("system:kube-scheduler", nil))
	makeCert(IdCA, Scheduler, opki.Server([]string{"kube-scheduler.local"}, nil))
	makeCert(IdCA, Master, opki.Client("metropolis:master", []string{"system:masters"}))

	pki.Certificates[AggregationCA] = &opki.Certificate{
		Namespace: &pki.namespace,
		Issuer:    opki.SelfSigned,
		Name:      string(AggregationCA),
		Template:  opki.CA("Metropolis OpenAPI Aggregation CA"),
		Mode:      opki.CertificateManaged,
	}
	makeCert(AggregationCA, FrontProxyClient, opki.Client("front-proxy-client", nil))
	makeCert(AggregationCA, MetropolisAuthProxyClient, opki.Client("metropolis-auth-proxy-client", nil))

	return &pki
}

// FromLocalConsensus returns a PKI stored on the given local consensus instance,
// in the correct etcd namespace.
func FromLocalConsensus(ctx context.Context, svc consensus.ServiceHandle) (*PKI, error) {
	// TODO(q3k): make this configurable
	clusterDomain := "cluster.local"

	cstW := svc.Watch()
	defer cstW.Close()
	cst, err := cstW.Get(ctx, consensus.FilterRunning)
	if err != nil {
		return nil, fmt.Errorf("waiting for local consensus: %w", err)
	}
	kkv, err := cst.KubernetesClient()
	if err != nil {
		return nil, fmt.Errorf("retrieving kubernetes client: %w", err)
	}
	pki := New(kkv, clusterDomain)
	// Run EnsureAll ASAP to prevent race conditions between two kpki instances
	// attempting to initialize the PKI data at the same time.
	if err := pki.EnsureAll(ctx); err != nil {
		return nil, fmt.Errorf("initial ensure failed: %w", err)
	}
	return pki, nil
}

// EnsureAll ensures that all static certificates (and the serviceaccount key)
// are present on etcd.
func (k *PKI) EnsureAll(ctx context.Context) error {
	for n, v := range k.Certificates {
		_, err := v.Ensure(ctx, k.KV)
		if err != nil {
			return fmt.Errorf("could not ensure certificate %q exists: %w", n, err)
		}
	}
	_, err := k.ServiceAccountKey(ctx)
	if err != nil {
		return fmt.Errorf("could not ensure service account key exists: %w", err)
	}
	return nil
}

// Kubeconfig generates a kubeconfig blob for a given certificate name. The
// same lifetime semantics as in .Certificate apply.
func (k *PKI) Kubeconfig(ctx context.Context, name KubeCertificateName, endpoint KubernetesAPIEndpoint) ([]byte, error) {
	c, ok := k.Certificates[name]
	if !ok {
		return nil, fmt.Errorf("no certificate %q", name)
	}
	return Kubeconfig(ctx, k.KV, c, endpoint)
}

// Certificate retrieves an x509 DER-encoded (but not PEM-wrapped) key and
// certificate for a given certificate name.
// If the requested certificate is volatile, it will be created on demand.
// Otherwise it will be created on etcd (if not present), and retrieved from
// there.
func (k *PKI) Certificate(ctx context.Context, name KubeCertificateName) (cert, key []byte, err error) {
	c, ok := k.Certificates[name]
	if !ok {
		return nil, nil, fmt.Errorf("no certificate %q", name)
	}
	cert, err = c.Ensure(ctx, k.KV)
	if err != nil {
		return
	}
	key, err = c.PrivateKeyX509()
	return
}

// A KubernetesAPIEndpoint describes where a Kubeconfig will make a client
// attempt to connect to reach the Kubernetes apiservers(s).
type KubernetesAPIEndpoint string

var (
	// KubernetesAPIEndpointForWorker points Kubernetes workers to connect to a
	// locally-running apiproxy, which in turn loadbalances the connection to
	// controller nodes running in the cluster.
	KubernetesAPIEndpointForWorker = KubernetesAPIEndpoint(fmt.Sprintf("https://127.0.0.1:%d", allocs.PortKubernetesWorkerLocalAPI))
	// KubernetesAPIEndpointForController points Kubernetes controllers to connect to
	// the locally-running API server.
	KubernetesAPIEndpointForController = KubernetesAPIEndpoint(fmt.Sprintf("https://127.0.0.1:%d", allocs.PortKubernetesAPI))
)

// KubeconfigRaw emits a Kubeconfig for a given set of certificates, private key,
// and a KubernetesAPIEndpoint. This function does not rely on the rest of the
// (K)PKI infrastructure.
func KubeconfigRaw(cacert, cert []byte, priv ed25519.PrivateKey, endpoint KubernetesAPIEndpoint) ([]byte, error) {
	caX, _ := x509.ParseCertificate(cacert)
	certX, _ := x509.ParseCertificate(cert)
	if err := certX.CheckSignatureFrom(caX); err != nil {
		return nil, fmt.Errorf("given ca does not sign given cert")
	}
	pub1 := priv.Public().(ed25519.PublicKey)
	pub2 := certX.PublicKey.(ed25519.PublicKey)
	if !bytes.Equal(pub1, pub2) {
		return nil, fmt.Errorf("given private key does not match given cert (cert: %s, key: %s)", hex.EncodeToString(pub2), hex.EncodeToString(pub1))
	}

	key, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("could not marshal private key: %w", err)
	}

	kubeconfig := configapi.NewConfig()

	cluster := configapi.NewCluster()

	cluster.Server = string(endpoint)

	cluster.CertificateAuthorityData = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cacert})
	kubeconfig.Clusters["default"] = cluster

	authInfo := configapi.NewAuthInfo()
	authInfo.ClientCertificateData = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert})
	authInfo.ClientKeyData = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
	kubeconfig.AuthInfos["default"] = authInfo

	ct := configapi.NewContext()
	ct.Cluster = "default"
	ct.AuthInfo = "default"
	kubeconfig.Contexts["default"] = ct

	kubeconfig.CurrentContext = "default"
	return clientcmd.Write(*kubeconfig)
}

// Kubeconfig generates a kubeconfig blob for this certificate. The same
// lifetime semantics as in .Ensure apply.
func Kubeconfig(ctx context.Context, kv clientv3.KV, c *opki.Certificate, endpoint KubernetesAPIEndpoint) ([]byte, error) {
	cert, err := c.Ensure(ctx, kv)
	if err != nil {
		return nil, fmt.Errorf("could not ensure certificate exists: %w", err)
	}
	if len(c.PrivateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("certificate has no associated private key")
	}
	ca, err := c.Issuer.CACertificate(ctx, kv)
	if err != nil {
		return nil, fmt.Errorf("could not get CA certificate: %w", err)
	}

	return KubeconfigRaw(ca, cert, c.PrivateKey, endpoint)
}

// ServiceAccountKey retrieves (and possibly generates and stores on etcd) the
// Kubernetes service account key. The returned data is ready to be used by
// Kubernetes components (in PKIX form).
func (k *PKI) ServiceAccountKey(ctx context.Context) ([]byte, error) {
	// TODO(q3k): this should be abstracted away once we abstract away etcd
	// access into a library with try-or-create semantics.
	path := fmt.Sprintf("%s%s.der", etcdPrefix, serviceAccountKeyName)

	// Try loading  key from etcd.
	keyRes, err := k.KV.Get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("failed to get key from etcd: %w", err)
	}

	if len(keyRes.Kvs) == 1 {
		// Certificate and key exists in etcd, return that.
		return keyRes.Kvs[0].Value, nil
	}

	// No key found - generate one.
	keyRaw, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(keyRaw)
	if err != nil {
		panic(err) // Always a programmer error
	}

	// Save to etcd.
	_, err = k.KV.Put(ctx, path, string(key))
	if err != nil {
		return nil, fmt.Errorf("failed to write newly generated key: %w", err)
	}
	return key, nil
}

// Kubelet returns a pair of server/client ceritficates for the Kubelet to use.
func (k *PKI) Kubelet(ctx context.Context, name string, pubkey ed25519.PublicKey) (server *opki.Certificate, client *opki.Certificate, err error) {
	name = fmt.Sprintf("system:node:%s", name)
	err = k.EnsureAll(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("could not ensure certificates exist: %w", err)
	}
	kubeCA := k.Certificates[IdCA]
	serverName := fmt.Sprintf("kubelet-%s-server", name)
	server = &opki.Certificate{
		Name:      serverName,
		Namespace: &k.namespace,
		Issuer:    kubeCA,
		Template:  opki.Server([]string{name}, nil),
		Mode:      opki.CertificateExternal,
		PublicKey: pubkey,
	}
	clientName := fmt.Sprintf("kubelet-%s-client", name)
	client = &opki.Certificate{
		Name:      clientName,
		Namespace: &k.namespace,
		Issuer:    kubeCA,
		Template:  opki.Client(name, []string{"system:nodes"}),
		Mode:      opki.CertificateExternal,
		PublicKey: pubkey,
	}
	return server, client, nil
}

// CSIProvisioner returns a certificate to be used by the CSI provisioner running
// on a worker node.
func (k *PKI) CSIProvisioner(ctx context.Context, name string, pubkey ed25519.PublicKey) (client *opki.Certificate, err error) {
	name = fmt.Sprintf("metropolis:csi-provisioner:%s", name)
	err = k.EnsureAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not ensure certificates exist: %w", err)
	}
	kubeCA := k.Certificates[IdCA]
	clientName := fmt.Sprintf("csi-provisioner-%s", name)
	client = &opki.Certificate{
		Name:      clientName,
		Namespace: &k.namespace,
		Issuer:    kubeCA,
		Template:  opki.Client(name, []string{"metropolis:csi-provisioner"}),
		Mode:      opki.CertificateExternal,
		PublicKey: pubkey,
	}
	return client, nil
}

// NetServices returns a certificate to be used by nfproxy and clusternet running
// on a worker node.
func (k *PKI) NetServices(ctx context.Context, name string, pubkey ed25519.PublicKey) (client *opki.Certificate, err error) {
	name = fmt.Sprintf("metropolis:netservices:%s", name)
	err = k.EnsureAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not ensure certificates exist: %w", err)
	}
	kubeCA := k.Certificates[IdCA]
	clientName := fmt.Sprintf("netservices-%s", name)
	client = &opki.Certificate{
		Name:      clientName,
		Namespace: &k.namespace,
		Issuer:    kubeCA,
		Template:  opki.Client(name, []string{"metropolis:netservices"}),
		Mode:      opki.CertificateExternal,
		PublicKey: pubkey,
	}
	return client, nil
}

// VolatileClient returns a client certificate for Kubernetes clients to use.
// The generated certificate will place the user in the given groups, and with
// a given identiy as the certificate's CN.
func (k *PKI) VolatileClient(ctx context.Context, identity string, groups []string) (*opki.Certificate, error) {
	if err := k.EnsureAll(ctx); err != nil {
		return nil, fmt.Errorf("could not ensure certificates exist: %w", err)
	}
	return &opki.Certificate{
		Namespace: &k.namespace,
		Issuer:    k.Certificates[IdCA],
		Template:  opki.Client(identity, groups),
		Mode:      opki.CertificateEphemeral,
	}, nil
}
