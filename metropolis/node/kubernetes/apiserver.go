// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"os/exec"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/apis/apiserver"
	"k8s.io/kubernetes/plugin/pkg/admission/security/podsecurity"
	podsecurityadmissionv1 "k8s.io/pod-security-admission/admission/api/v1"

	"source.monogon.dev/metropolis/node/allocs"
	"source.monogon.dev/metropolis/node/core/localstorage"
	"source.monogon.dev/metropolis/node/kubernetes/pki"
	"source.monogon.dev/osbase/fileargs"
	"source.monogon.dev/osbase/supervisor"
)

type apiserverService struct {
	KPKI                        *pki.PKI
	AdvertiseAddress            net.IP
	ServiceIPRange              net.IPNet
	EphemeralConsensusDirectory *localstorage.EphemeralConsensusDirectory

	// All PKI-related things are in DER
	idCA                  []byte
	kubeletClientCert     []byte
	kubeletClientKey      []byte
	aggregationCA         []byte
	aggregationClientCert []byte
	aggregationClientKey  []byte
	serviceAccountPrivKey []byte // In PKIX form
	serverCert            []byte
	serverKey             []byte
}

func mustWrapUnknownJSON(o schema.ObjectKind) *runtime.Unknown {
	oRaw, err := json.Marshal(o)
	if err != nil {
		panic("While marshaling object into runtime.Unknown: " + err.Error())
	}
	var typ runtime.TypeMeta
	typ.SetGroupVersionKind(o.GroupVersionKind())
	return &runtime.Unknown{
		TypeMeta:    typ,
		Raw:         oRaw,
		ContentType: runtime.ContentTypeJSON,
	}
}

func mustMarshalJSON(o any) []byte {
	out, err := json.Marshal(o)
	if err != nil {
		panic("mustMarshalJSON failed: " + err.Error())
	}
	return out
}

var (
	podsecurityadmission = &podsecurityadmissionv1.PodSecurityConfiguration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: podsecurityadmissionv1.SchemeGroupVersion.String(),
			Kind:       "PodSecurityConfiguration",
		},
		Defaults: podsecurityadmissionv1.PodSecurityDefaults{
			Enforce: "baseline",
			Warn:    "baseline",
			Audit:   "baseline",
		},
		Exemptions: podsecurityadmissionv1.PodSecurityExemptions{},
	}

	admissionConfig = apiserver.AdmissionConfiguration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiserver.SchemeGroupVersion.String(),
			Kind:       "AdmissionConfiguration",
		},
		Plugins: []apiserver.AdmissionPluginConfiguration{{
			Name:          podsecurity.PluginName,
			Configuration: mustWrapUnknownJSON(podsecurityadmission),
		}},
	}

	admissionConfigRaw = mustMarshalJSON(admissionConfig)
)

func (s *apiserverService) loadPKI(ctx context.Context) error {
	for _, el := range []struct {
		targetCert *[]byte
		targetKey  *[]byte
		name       pki.KubeCertificateName
	}{
		{&s.idCA, nil, pki.IdCA},
		{&s.kubeletClientCert, &s.kubeletClientKey, pki.APIServerKubeletClient},
		{&s.aggregationCA, nil, pki.AggregationCA},
		{&s.aggregationClientCert, &s.aggregationClientKey, pki.FrontProxyClient},
		{&s.serverCert, &s.serverKey, pki.APIServer},
	} {
		cert, key, err := s.KPKI.Certificate(ctx, el.name)
		if err != nil {
			return fmt.Errorf("could not load certificate %q from PKI: %w", el.name, err)
		}
		if el.targetCert != nil {
			*el.targetCert = cert
		}
		if el.targetKey != nil {
			*el.targetKey = key
		}
	}

	var err error
	s.serviceAccountPrivKey, err = s.KPKI.ServiceAccountKey(ctx)
	if err != nil {
		return fmt.Errorf("could not load serviceaccount privkey: %w", err)
	}
	return nil
}

func (s *apiserverService) Run(ctx context.Context) error {
	if err := s.loadPKI(ctx); err != nil {
		return fmt.Errorf("loading PKI data failed: %w", err)
	}
	args, err := fileargs.New()
	if err != nil {
		panic(err) // If this fails, something is very wrong. Just crash.
	}
	defer args.Close()

	cmd := exec.CommandContext(ctx, "/kubernetes/bin/kube", "kube-apiserver",
		fmt.Sprintf("--advertise-address=%v", s.AdvertiseAddress.String()),
		"--authorization-mode=Node,RBAC",
		args.FileOpt("--client-ca-file", "client-ca.pem",
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.idCA})),
		"--enable-admission-plugins=NodeRestriction",
		"--enable-aggregator-routing=true",
		fmt.Sprintf("--secure-port=%d", allocs.PortKubernetesAPI),
		fmt.Sprintf("--etcd-servers=unix:///%s:0", s.EphemeralConsensusDirectory.ClientSocket.FullPath()),
		args.FileOpt("--kubelet-client-certificate", "kubelet-client-cert.pem",
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.kubeletClientCert})),
		args.FileOpt("--kubelet-client-key", "kubelet-client-key.pem",
			pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: s.kubeletClientKey})),
		"--kubelet-preferred-address-types=InternalIP",
		args.FileOpt("--proxy-client-cert-file", "aggregation-client-cert.pem",
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.aggregationClientCert})),
		args.FileOpt("--proxy-client-key-file", "aggregation-client-key.pem",
			pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: s.aggregationClientKey})),
		"--requestheader-allowed-names=front-proxy-client,metropolis-auth-proxy-client",
		args.FileOpt("--requestheader-client-ca-file", "aggregation-ca.pem",
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.aggregationCA})),
		"--requestheader-extra-headers-prefix=X-Remote-Extra-",
		"--requestheader-group-headers=X-Remote-Group",
		"--requestheader-username-headers=X-Remote-User",
		args.FileOpt("--service-account-key-file", "service-account-pubkey.pem",
			pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: s.serviceAccountPrivKey})),
		args.FileOpt("--service-account-signing-key-file", "service-account-signing-key.pem",
			pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: s.serviceAccountPrivKey})),
		"--service-account-issuer", "https://metropolis.internal", // TODO: Figure out federation
		fmt.Sprintf("--service-cluster-ip-range=%v", s.ServiceIPRange.String()),
		// We use a patch for the allocator that prevents usage of system ports.
		"--service-node-port-range=1-65535",
		args.FileOpt("--tls-cert-file", "server-cert.pem",
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.serverCert})),
		args.FileOpt("--tls-private-key-file", "server-key.pem",
			pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: s.serverKey})),
		args.FileOpt("--admission-control-config-file", "admission-control.json", admissionConfigRaw),
		"--allow-privileged=true",
		extraFeatureGates.AsFlag(),
	)
	if args.Error() != nil {
		return err
	}
	return supervisor.RunCommand(ctx, cmd, supervisor.ParseKLog())
}
