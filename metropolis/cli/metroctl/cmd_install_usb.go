// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	_ "embed"
	"fmt"
	"log"

	"github.com/spf13/cobra"

	"source.monogon.dev/metropolis/cli/metroctl/core"
	"source.monogon.dev/osbase/oci"
)

var genusbCmd = &cobra.Command{
	Use:     "genusb target",
	Short:   "Generates a Metropolis installer disk or image.",
	Example: "metroctl install --image=metropolis-v0.1 genusb /dev/sdx",
	Args:    PrintUsageOnWrongArgs(cobra.ExactArgs(1)), // One positional argument: the target
	RunE: func(cmd *cobra.Command, args []string) error {
		params, err := makeNodeParams()
		if err != nil {
			return err
		}

		installerPath, err := cmd.Flags().GetString("installer")
		if err != nil {
			return err
		}

		installer, err := externalFile("installer", "_main/metropolis/installer/kernel.efi", &installerPath)
		if err != nil {
			return err
		}
		imagePathResolved, err := external("image", "_main/metropolis/node/oci_image", imagePath)
		if err != nil {
			return err
		}
		image, err := oci.AsImage(oci.ReadLayout(imagePathResolved))
		if err != nil {
			return fmt.Errorf("failed to read OS image: %w", err)
		}

		installerImageArgs := core.MakeInstallerImageArgs{
			TargetPath: args[0],
			Installer:  installer,
			NodeParams: params,
			Image:      image,
		}

		log.Printf("Generating installer image (this can take a while, see issues/92).")
		if err := core.MakeInstallerImage(installerImageArgs); err != nil {
			return fmt.Errorf("failed to create installer: %w", err)
		}
		return nil
	},
}

func init() {
	genusbCmd.Flags().StringP("installer", "i", "", "Path to the Metropolis installer to use when installing")
	installCmd.AddCommand(genusbCmd)
}
