// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package staticcheck

import (
	"golang.org/x/tools/go/analysis"
	"honnef.co/go/tools/analysis/lint"
	"honnef.co/go/tools/quickfix"
	"honnef.co/go/tools/simple"
	"honnef.co/go/tools/staticcheck"
	"honnef.co/go/tools/stylecheck"
	"honnef.co/go/tools/unused"
)

// Analyzers contains all staticcheck analyzer passes
// Copied from https://github.com/sluongng/nogo-analyzer/ under Apache 2.0
var Analyzers = func() map[string]*analysis.Analyzer {
	m := make(map[string]*analysis.Analyzer)
	for _, analyzers := range [][]*lint.Analyzer{
		quickfix.Analyzers,
		simple.Analyzers,
		staticcheck.Analyzers,
		stylecheck.Analyzers,
		{unused.Analyzer},
	} {
		for _, a := range analyzers {
			m[a.Analyzer.Name] = a.Analyzer
		}
	}
	return m
}()
