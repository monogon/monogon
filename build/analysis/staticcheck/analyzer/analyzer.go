// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package staticcheck

import (
	"fmt"

	"source.monogon.dev/build/analysis/staticcheck"
)

var (
	name     string
	Analyzer = staticcheck.Analyzers[name]
)

func init() {
	if Analyzer == nil {
		panic(fmt.Sprintf("staticcheck analyzer %q not found", name))
	}
}
