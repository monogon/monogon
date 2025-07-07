// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	_ "embed"
	"fmt"
	"os"
	"os/signal"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/prototext"

	"source.monogon.dev/metropolis/proto/api"
	cpb "source.monogon.dev/metropolis/proto/common"

	"source.monogon.dev/metropolis/cli/flagdefs"
	"source.monogon.dev/metropolis/cli/metroctl/core"
	common "source.monogon.dev/metropolis/node"
	"source.monogon.dev/osbase/structfs"
)

var installCmd = &cobra.Command{
	Short: "Contains subcommands to install Metropolis via different media.",
	Use:   "install",
}

// bootstrap is a flag controlling node parameters included in the installer
// image. If set, the installed node will bootstrap a new cluster. Otherwise,
// it will try to connect to the cluster which endpoints were provided with
// the --endpoints flag.
var bootstrap = installCmd.PersistentFlags().Bool("bootstrap", false, "Create a bootstrap installer image.")
var bootstrapTPMMode = flagdefs.TPMModePflag(installCmd.PersistentFlags(), "bootstrap-tpm-mode", cpb.ClusterConfiguration_TPM_MODE_REQUIRED, "TPM mode to set on cluster")
var bootstrapStorageSecurityPolicy = flagdefs.StorageSecurityPolicyPflag(installCmd.PersistentFlags(), "bootstrap-storage-security", cpb.ClusterConfiguration_STORAGE_SECURITY_POLICY_NEEDS_ENCRYPTION_AND_AUTHENTICATION, "Storage security policy to set on cluster")
var imagePath = installCmd.PersistentFlags().StringP("image", "", "", "Path to the OCI layout directory containing the Metropolis OS image to be installed")
var nodeParamPath = installCmd.PersistentFlags().String("node-params", "", "Path to the metropolis.proto.api.NodeParameters prototext file (advanced usage only)")

func makeNodeParams() (*api.NodeParameters, error) {
	ctx, _ := signal.NotifyContext(context.Background(), os.Interrupt)

	if err := os.MkdirAll(flags.configPath, 0700); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("failed to create config directory: %w", err)
	}

	var params api.NodeParameters
	if *nodeParamPath != "" {
		nodeParamsRaw, err := os.ReadFile(*nodeParamPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read node-params file: %w", err)
		}
		if err := prototext.Unmarshal(nodeParamsRaw, &params); err != nil {
			return nil, fmt.Errorf("failed to parse node-params: %w", err)
		}
	} else {
		params = api.NodeParameters{}
	}

	if *bootstrap {
		if flags.cluster == "" {
			return nil, fmt.Errorf("when bootstrapping a cluster, the --cluster parameter is required")
		}
		if err := common.ValidateClusterDomain(flags.cluster); err != nil {
			return nil, fmt.Errorf("invalid cluster domain: %w", err)
		}

		// TODO(lorenz): Have a key management story for this
		priv, err := core.GetOrMakeOwnerKey(flags.configPath)
		if err != nil {
			return nil, fmt.Errorf("failed to generate or get owner key: %w", err)
		}
		pub := priv.Public().(ed25519.PublicKey)
		params.Cluster = &api.NodeParameters_ClusterBootstrap_{
			ClusterBootstrap: &api.NodeParameters_ClusterBootstrap{
				OwnerPublicKey: pub,
				InitialClusterConfiguration: &cpb.ClusterConfiguration{
					ClusterDomain:         flags.cluster,
					StorageSecurityPolicy: *bootstrapStorageSecurityPolicy,
					TpmMode:               *bootstrapTPMMode,
				},
			},
		}
	} else {
		cc, err := newAuthenticatedClient(ctx)
		if err != nil {
			return nil, fmt.Errorf("while creating client: %w", err)
		}
		mgmt := api.NewManagementClient(cc)
		resT, err := mgmt.GetRegisterTicket(ctx, &api.GetRegisterTicketRequest{})
		if err != nil {
			return nil, fmt.Errorf("while receiving register ticket: %w", err)
		}
		resI, err := mgmt.GetClusterInfo(ctx, &api.GetClusterInfoRequest{})
		if err != nil {
			return nil, fmt.Errorf("while receiving cluster directory: %w", err)
		}

		params.Cluster = &api.NodeParameters_ClusterRegister_{
			ClusterRegister: &api.NodeParameters_ClusterRegister{
				RegisterTicket:   resT.Ticket,
				ClusterDirectory: resI.ClusterDirectory,
				CaCertificate:    resI.CaCertificate,
			},
		}
	}
	return &params, nil
}

func external(name, datafilePath string, flag *string) (string, error) {
	if flag == nil || *flag == "" {
		rPath, err := runfiles.Rlocation(datafilePath)
		if err != nil {
			return "", fmt.Errorf("no %s specified", name)
		}
		return rPath, nil
	}
	return *flag, nil
}

func externalFile(name, datafilePath string, flag *string) (structfs.Blob, error) {
	path, err := external(name, datafilePath, flag)
	if err != nil {
		return nil, err
	}
	f, err := structfs.OSPathBlob(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open specified %s: %w", name, err)
	}
	return f, nil
}

func init() {
	rootCmd.AddCommand(installCmd)
}
