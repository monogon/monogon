// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package gofmt

import (
	"bytes"
	"os"
	"strings"

	"github.com/golangci/gofmt/gofmt"
	"golang.org/x/tools/go/analysis"

	alib "source.monogon.dev/build/analysis/lib"
)

var Analyzer = &analysis.Analyzer{
	Name: "gofmt",
	Doc:  "checks if files have been run through `gofmt -s`",
	Run: func(pass *analysis.Pass) (any, error) {
		for _, f := range pass.Files {
			if alib.IsGeneratedFile(f) {
				continue
			}

			fileName := pass.Fset.PositionFor(f.Pos(), true).Filename

			// Unlike our other analyzers, this one is special.
			// It operates on the raw file, so we need to skip
			// non-Go files that would otherwise be a no-op
			// with an empty AST.
			if !strings.HasSuffix(fileName, ".go") {
				continue
			}

			src, err := os.ReadFile(fileName)
			if err != nil {
				return nil, err
			}

			res, err := gofmt.Source(fileName, src, gofmt.Options{NeedSimplify: true})
			if err != nil {
				return nil, err
			}

			if bytes.Equal(src, res) {
				continue
			}

			pass.Report(analysis.Diagnostic{
				Pos:     f.Pos(),
				Message: "not formatted with gofmt -s",
				SuggestedFixes: []analysis.SuggestedFix{
					{
						TextEdits: []analysis.TextEdit{
							{
								Pos:     f.FileStart,
								End:     f.FileEnd,
								NewText: res,
							},
						},
					},
				},
			})
		}
		return nil, nil
	},
}
