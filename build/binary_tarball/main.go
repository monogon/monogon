// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"flag"
	"io"
	"log"
	"os"
	"path"
	"strings"

	"google.golang.org/protobuf/encoding/prototext"

	"source.monogon.dev/build/binary_tarball/spec"
)

var (
	specPath = flag.String("spec", "", "Path to the layer specification (spec.Spec)")
	outPath  = flag.String("out", "", "Output file path")
)

func main() {
	flag.Parse()
	var spec spec.Spec
	specRaw, err := os.ReadFile(*specPath)
	if err != nil {
		log.Fatalf("failed to open spec file: %v", err)
	}
	if err := prototext.Unmarshal(specRaw, &spec); err != nil {
		log.Fatalf("failed to unmarshal spec: %v", err)
	}
	outFile, err := os.Create(*outPath)
	if err != nil {
		log.Fatalf("failed to open output: %v", err)
	}
	defer outFile.Close()
	outTar := tar.NewWriter(outFile)
	defer outTar.Close()
	createdDirs := make(map[string]bool)
	for _, file := range spec.File {
		srcFile, err := os.Open(file.Src)
		if err != nil {
			log.Fatalf("failed to open input file: %v", err)
		}
		info, err := srcFile.Stat()
		if err != nil {
			log.Fatalf("cannot stat input file: %v", err)
		}
		var mode int64 = 0644
		if info.Mode()&0111 != 0 {
			mode = 0755
		}
		targetPath := path.Join("app", file.Path)
		targetDirParts := strings.Split(path.Dir(targetPath), "/")
		var partialDir string
		for _, part := range targetDirParts {
			partialDir = path.Join(partialDir, part)
			if !createdDirs[partialDir] {
				if err := outTar.WriteHeader(&tar.Header{
					Typeflag: tar.TypeDir,
					Name:     partialDir,
					Mode:     0755,
				}); err != nil {
					log.Fatalf("failed to write directory: %v", err)
				}
				createdDirs[partialDir] = true
			}
		}
		if err := outTar.WriteHeader(&tar.Header{
			Name: targetPath,
			Size: info.Size(),
			Mode: mode,
		}); err != nil {
			log.Fatalf("failed to write header: %v", err)
		}
		if _, err := io.Copy(outTar, srcFile); err != nil {
			log.Fatalf("failed to copy file into tar: %v", err)
		}
	}
}
