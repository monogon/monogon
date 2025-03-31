// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package launch

import (
	"github.com/bazelbuild/rules_go/go/runfiles"
)

var (
	// These are filled by bazel at linking time with the canonical path of
	// their corresponding file. Inside the init function we resolve it
	// with the rules_go runfiles package to the real path.
	xSwtpmPath        string
	xSwtpmSetupPath   string
	xSwtpmLocalCAPath string
	xSwtpmCertPath    string
	xCerttoolPath     string
	xMetroctlPath     string
	xOvmfCodePath     string
	xOvmfVarsPath     string
	xKernelPath       string
	xInitramfsPath    string
	xNodeImagePath    string
	xAbloaderPath     string
	xQEMUPath         string
)

func init() {
	var err error
	for _, path := range []*string{
		&xSwtpmPath, &xSwtpmSetupPath, &xSwtpmLocalCAPath,
		&xSwtpmCertPath, &xCerttoolPath, &xMetroctlPath,
		&xOvmfCodePath, &xOvmfVarsPath, &xKernelPath,
		&xInitramfsPath, &xNodeImagePath, &xAbloaderPath,
		&xQEMUPath,
	} {
		*path, err = runfiles.Rlocation(*path)
		if err != nil {
			panic(err)
		}
	}
}
