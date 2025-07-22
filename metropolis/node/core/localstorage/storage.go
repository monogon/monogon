// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package localstorage

// Localstorage is a replacement for the old 'storage' internal library. It is
// currently unused, but will become so as the node code gets rewritten.

// The library is centered around the idea of a declarative filesystem tree
// defined as mutually recursive Go structs.  This structure is then Placed
// onto an abstract real filesystem (eg. a local POSIX filesystem at /), and a
// handle to that placed filesystem is then used by the consumers of this
// library to refer to subsets of the tree (that now correspond to locations on
// a filesystem).
//
// Every member of the storage hierarchy must either be, or inherit from
// Directory or File. In order to be placed correctly, Directory embedding
// structures must use `dir:` or `file:` tags for child Directories and files
// respectively. The content of the tag specifies the path part that this
// element will be placed at.
//
// Full placement path(available via FullPath()) format is placement
// implementation-specific. However, they're always strings.

import (
	"sync"

	"source.monogon.dev/metropolis/node/core/localstorage/declarative"
)

type Root struct {
	declarative.Directory
	// UEFI ESP partition, mounted from plaintext storage.
	ESP ESPDirectory `dir:"esp"`
	// Persistent Data partition, mounted from encrypted and authenticated storage.
	Data DataDirectory `dir:"data"`
	// FHS-standard /etc directory, contains /etc/hosts, /etc/machine-id, and
	// other compatibility files.
	Etc EtcDirectory `dir:"etc"`
	// Ephemeral data, used by runtime, stored in tmpfs. Things like sockets,
	// temporary config files, etc.
	Ephemeral EphemeralDirectory `dir:"ephemeral"`
	// FHS-standard /tmp directory, used by os.MkdirTemp.
	Tmp TmpDirectory `dir:"tmp"`
	// FHS-standard /run directory. Used by various services.
	Run RunDirectory `dir:"run"`
}

type PKIDirectory struct {
	declarative.Directory
	CACertificate declarative.File `file:"ca.pem"`
	Certificate   declarative.File `file:"cert.pem"`
	Key           declarative.File `file:"cert-key.pem"`
}

// DataDirectory is an xfs partition mounted via cryptsetup/LUKS, with a key
// derived from {global,local}Unlock keys.
type DataDirectory struct {
	declarative.Directory

	// flagLock locks canMount and mounted.
	flagLock sync.Mutex
	// canMount is set by Root when it is initialized. It is required to be set
	// for mounting the data directory.
	canMount bool
	// mounted is set by DataDirectory when it is mounted. It ensures it's only
	// mounted once.
	mounted bool

	Containerd declarative.Directory   `dir:"containerd"`
	Etcd       DataEtcdDirectory       `dir:"etcd"`
	Kubernetes DataKubernetesDirectory `dir:"kubernetes"`
	Node       DataNodeDirectory       `dir:"node"`
	Volumes    DataVolumesDirectory    `dir:"volumes"`
}

type DataNodeDirectory struct {
	declarative.Directory
	Credentials    PKIDirectory     `dir:"credentials"`
	PersistedRoles declarative.File `file:"roles.pb"`
}

type DataEtcdDirectory struct {
	declarative.Directory
	PeerPKI PKIDirectory          `dir:"peer_pki"`
	PeerCRL declarative.File      `file:"peer_crl"`
	Data    declarative.Directory `dir:"data"`
}

type DataKubernetesDirectory struct {
	declarative.Directory
	ClusterNetworking DataKubernetesClusterNetworkingDirectory `dir:"clusternet"`
	CSIProvisioner    DataKubernetesCSIProvisionerDirectory    `dir:"csiprovisioner"`
	Netservices       DataKubernetesNetservicesDirectory       `dir:"netservices"`
	Kubelet           DataKubernetesKubeletDirectory           `dir:"kubelet"`
}

type DataKubernetesClusterNetworkingDirectory struct {
	declarative.Directory
	Key declarative.File `file:"private.key"`
}

type DataKubernetesCSIProvisionerDirectory struct {
	declarative.Directory
	PKI PKIDirectory `dir:"pki"`
}

type DataKubernetesNetservicesDirectory struct {
	declarative.Directory
	PKI PKIDirectory `dir:"pki"`
}

type DataKubernetesKubeletDirectory struct {
	declarative.Directory
	PKI PKIDirectory `dir:"pki"`

	DevicePlugins struct {
		declarative.Directory
		// Used by Kubelet, hardcoded relative to
		// DataKubernetesKubeletDirectory
		Kubelet declarative.File `file:"kubelet.sock"`
	} `dir:"device-plugins"`

	// Pod logs, hardcoded to /data/kubelet/logs in
	// //metropolis/node/kubernetes:kubelet.go
	Logs declarative.Directory `dir:"logs"`

	Plugins struct {
		declarative.Directory
		VFS declarative.File `file:"dev.monogon.metropolis.vfs.sock"`
		KVM declarative.File `file:"devices.monogon.dev_kvm.sock"`
	} `dir:"plugins"`

	PluginsRegistry struct {
		declarative.Directory
		VFSReg declarative.File `file:"dev.monogon.metropolis.vfs-reg.sock"`
		KVMReg declarative.File `file:"devices.monogon.dev_kvm-reg.sock"`
	} `dir:"plugins_registry"`
}

type DataVolumesDirectory struct {
	declarative.Directory
}

type EtcDirectory struct {
	declarative.Directory
	// Symlinked to /ephemeral/machine-id, baked into the erofs system image
	MachineID declarative.File `file:"machine-id"`
}

type EphemeralDirectory struct {
	declarative.Directory
	Consensus         EphemeralConsensusDirectory  `dir:"consensus"`
	Containerd        EphemeralContainerdDirectory `dir:"containerd"`
	FlexvolumePlugins declarative.Directory        `dir:"flexvolume_plugins"`
	MachineID         declarative.File             `file:"machine-id"`
}

type EphemeralConsensusDirectory struct {
	declarative.Directory
	ClientSocket   declarative.File `file:"client.sock"`
	ServerLogsFIFO declarative.File `file:"server-logs.fifo"`
}

type EphemeralContainerdDirectory struct {
	declarative.Directory
	ClientSocket  declarative.File      `file:"client.sock"`
	RunSCLogsFIFO declarative.File      `file:"runsc-logs.fifo"`
	Tmp           declarative.Directory `dir:"tmp"`
	RunSC         declarative.Directory `dir:"runsc"`
}

type TmpDirectory struct {
	declarative.Directory
}

type RunDirectory struct {
	declarative.Directory
	// Hardcoded in @com_github_containerd_containerd_v2//
	// cmd/containerd-shim-runc-v2/process/utils.go and
	// @com_github_containerd_containerd_v2//defaults:defaults_unix.go
	Containerd declarative.Directory `dir:"containerd"`
}
