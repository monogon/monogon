// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// genproductinfo generates a product info JSON file from arguments and
// stamping. Additionally, it generates an os-release file following the
// freedesktop spec from the same information.
//
// https://www.freedesktop.org/software/systemd/man/os-release.html
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"

	"github.com/joho/godotenv"

	"source.monogon.dev/osbase/oci/osimage"
)

var (
	flagStatusFile      = flag.String("status_file", "", "path to bazel workspace status file")
	flagProductInfoFile = flag.String("product_info_file", "product-info", "path to product info output file")
	flagOSReleaseFile   = flag.String("os_release_file", "os-release", "path to os-release output file")
	flagStampVar        = flag.String("stamp_var", "", "variable to use as version from the workspace status file")
	flagName            = flag.String("name", "", "name parameter (see freedesktop spec)")
	flagID              = flag.String("id", "", "id parameter (see freedesktop spec)")
	flagArchitecture    = flag.String("architecture", "", "CPU architecture")
	flagBuildFlags      = flag.String("build_flags", "", "build flags joined by '-'")
	flagPlatformOS      = flag.String("platform_os", "", "platform OS")
)

var (
	rePrereleaseGitHash = regexp.MustCompile(`^g[0-9a-f]+$`)
)

func versionWithoutGitInfo(version string) string {
	version, metadata, hasMetadata := strings.Cut(version, "+")
	version, prerelease, hasPrerelease := strings.Cut(version, "-")
	if hasPrerelease {
		var filteredParts []string
		for part := range strings.SplitSeq(prerelease, ".") {
			switch {
			case part == "dirty":
				// Ignore field.
			case rePrereleaseGitHash.FindStringSubmatch(part) != nil:
				// Ignore field.
			default:
				filteredParts = append(filteredParts, part)
			}
		}
		if len(filteredParts) != 0 {
			version = version + "-" + strings.Join(filteredParts, ".")
		}
	}
	if hasMetadata {
		version = version + "+" + metadata
	}
	return version
}

func main() {
	var componentIDs []string
	flag.Func("component", "ID of a component", func(component string) error {
		componentIDs = append(componentIDs, component)
		return nil
	})
	flag.Parse()

	statusFileContent, err := os.ReadFile(*flagStatusFile)
	if err != nil {
		log.Fatalf("Failed to open bazel workspace status file: %v", err)
	}
	statusVars := make(map[string]string)
	for line := range strings.SplitSeq(string(statusFileContent), "\n") {
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			log.Fatalf("Invalid line in status file: %q", line)
		}
		statusVars[key] = value
	}

	version, ok := statusVars[*flagStampVar]
	if !ok {
		log.Fatalf("%s key not set in bazel workspace status file", *flagStampVar)
	}

	var components []osimage.Component
	for _, id := range componentIDs {
		versionKey := fmt.Sprintf("STABLE_MONOGON_componentVersion_%s", id)
		version, ok := statusVars[versionKey]
		if !ok {
			log.Fatalf("%s key not set in bazel workspace status file", versionKey)
		}
		components = append(components, osimage.Component{
			ID:      id,
			Version: version,
		})
	}

	variant := *flagArchitecture
	if *flagBuildFlags != "" {
		variant = variant + "-" + *flagBuildFlags
	}

	productInfo := osimage.ProductInfo{
		ID:             *flagID,
		Name:           *flagName,
		Version:        versionWithoutGitInfo(version),
		Variant:        variant,
		CommitHash:     statusVars["STABLE_MONOGON_gitCommit"],
		CommitDate:     statusVars["STABLE_MONOGON_gitCommitDate"],
		BuildTreeDirty: statusVars["STABLE_MONOGON_gitTreeState"] == "dirty",
		PlatformOS:     *flagPlatformOS,
		Components:     components,
	}
	productInfoBytes, err := json.MarshalIndent(productInfo, "", "\t")
	if err != nil {
		log.Fatalf("Failed to marshal OS image config: %v", err)
	}
	productInfoBytes = append(productInfoBytes, '\n')
	if err := os.WriteFile(*flagProductInfoFile, productInfoBytes, 0644); err != nil {
		log.Fatalf("Failed to write product info file: %v", err)
	}

	// As specified by https://www.freedesktop.org/software/systemd/man/os-release.html
	osReleaseVars := map[string]string{
		"NAME":        *flagName,
		"ID":          *flagID,
		"VERSION":     version,
		"VERSION_ID":  version,
		"PRETTY_NAME": *flagName + " " + version,
	}
	osReleaseContent, err := godotenv.Marshal(osReleaseVars)
	if err != nil {
		log.Fatalf("Failed to encode os-release file: %v", err)
	}
	if err := os.WriteFile(*flagOSReleaseFile, []byte(osReleaseContent+"\n"), 0644); err != nil {
		log.Fatalf("Failed to write os-release file: %v", err)
	}
}
