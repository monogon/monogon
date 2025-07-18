// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package osimage

import "strings"

// Media types which appear in the OCI image manifest.
const (
	ArtifactTypeOSImage          = "application/vnd.monogon.os.image.v1"
	MediaTypeOSImageConfig       = "application/vnd.monogon.os.image.config.v1+json"
	MediaTypePayloadUncompressed = "application/octet-stream"
	MediaTypePayloadZstd         = "application/zstd"
)

// ConfigVersion is the current version in [Config.FormatVersion].
const ConfigVersion = "1"

// Config contains metadata of an OS image.
type Config struct {
	// FormatVersion should be incremented when making breaking changes to the
	// image format. Readers must stop when they see an unknown version.
	FormatVersion string `json:"format_version"`
	// ProductInfo contains information about the content of the image.
	ProductInfo ProductInfo `json:"product_info"`
	// Payloads describes the payloads contained in the image. It has the same
	// length and order as the layers list in the image manifest.
	Payloads []PayloadInfo `json:"payloads"`
}

type ProductInfo struct {
	// ID of the product in the image. Recommended to be the same as the ID
	// property in os-release, and should follow the same syntax restrictions.
	// Example: "metropolis-node"
	// See: https://www.freedesktop.org/software/systemd/man/latest/os-release.html#ID=
	ID string `json:"id"`
	// Name of the product in the image. Recommended to be the same as the NAME
	// property in os-release. Example: "Metropolis Node"
	Name string `json:"name"`
	// Version of the product in the image.
	Version string `json:"version"`
	// Variant of the product build. This contains the values of relevant flags
	// passed to the build command. Currently, this is the architecture, and the
	// debug and race flag if set. Additional flags may be added in the future.
	// Examples: "x86_64-debug", "aarch64"
	//
	// The first "-"-separated component will always be the architecture.
	// See //build/platforms/BUILD.bazel for available architectures.
	//
	// This must contain only characters in the set [a-zA-Z0-9._-], such that it
	// can be part of a tag in an OCI registry.
	Variant string `json:"variant"`

	// CommitHash is the hex-encoded, full hash of the commit from which the image
	// was built.
	CommitHash string `json:"commit_hash"`
	// CommitDate of the commit from which the image was built.
	// This gives an indication of how old the image is, as it does not contain
	// changes made after this date.
	CommitDate string `json:"commit_date"`
	// BuildTreeDirty indicates that the tree from which the image was built
	// differs from the tree of the commit referenced by commit_hash.
	BuildTreeDirty bool `json:"build_tree_dirty"`

	// PlatformOS on top of which the product executes. This is put into the
	// platform os field when building an OCI image index. Example: "uefi"
	PlatformOS string `json:"platform_os"`
	// Components contains versions of the most important components. These are
	// mostly intended for human consumption, but could also be used for certain
	// automations, e.g. automatically deriving Kubernetes compatibility
	// constraints.
	Components []Component `json:"components,omitzero"`
}

// Architecture returns the CPU architecture, extracted from Variant.
func (p *ProductInfo) Architecture() string {
	architecture, _, _ := strings.Cut(p.Variant, "-")
	return architecture
}

type Component struct {
	// ID of the component. Example: "linux"
	ID string `json:"id"`
	// Version of the component. Example: "6.6.50"
	Version string `json:"version"`
}

type PayloadInfo struct {
	// Name of this payload, for example "system" or "kernel.efi". Must consist of
	// at least one and at most 80 characters in the set [0-9A-Za-z._-], and may
	// not start with [._] or end with [.]. Each name must be unique.
	Name string `json:"name"`
	// Size is the uncompressed size in bytes of the payload.
	Size int64 `json:"size"`
	// HashChunkSize is the size of each hash chunk, except for the last chunk
	// which may be smaller.
	HashChunkSize int64 `json:"hash_chunk_size"`
	// ChunkHashesSHA256 contains the sha256 hash of each chunk. Chunks are
	// obtained by dividing the uncompressed payload into chunks of size
	// HashChunkSize, with the last chunk containing the remainder.
	// Hashes are encoded as base64 without padding.
	ChunkHashesSHA256 []string `json:"chunk_hashes_sha256"`
}
