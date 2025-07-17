// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"fmt"
	"strings"
	"testing"
)

func TestEmbeddedContent(t *testing.T) {
	manifest := `{
	"schemaVersion": 2,
	"mediaType": "application/vnd.oci.image.manifest.v1+json",
	"config": {
		"mediaType": "application/vnd.oci.empty.v1+json",
		"digest": "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a",
		"size": 2,
		"data": "e30="
	},
	"layers": [
		{
			"digest": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			"size": 0
		},
		{
			"digest": "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff80",
			"size": 2,
			"data": "e30="
		}
	]
}`
	// Pass nil for blobs, which means reading can only work if it uses the
	// embedded content.
	image, err := AsImage(NewRef([]byte(manifest), "application/vnd.oci.image.manifest.v1+json", "", nil))
	if err != nil {
		t.Fatal(err)
	}
	configBytes, err := image.ReadBlobVerified(&image.Manifest.Config)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(configBytes), "{}"; got != want {
		t.Errorf("Got config %q, expected %q", got, want)
	}
	layerBytes, err := image.ReadBlobVerified(&image.Manifest.Layers[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(layerBytes) != 0 {
		t.Errorf("Got layer %q, expected to be empty", layerBytes)
	}
	// Layer 1 has a wrong digest.
	_, err = image.ReadBlobVerified(&image.Manifest.Layers[1])
	if !strings.Contains(fmt.Sprintf("%v", err), "failed verification") {
		t.Errorf("Expected failed verification, got %v", err)
	}
}

func TestParseDigest(t *testing.T) {
	testCases := []struct {
		input     string
		algorithm string
		encoded   string
		err       string
	}{
		{input: "", err: `invalid digest`},
		{input: "1234", err: `invalid digest`},
		{input: "x:y", err: `unknown digest algorithm "x"`},
		{input: "sha256:1234", err: `invalid sha256 digest length`},
		{input: "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b8550", err: `invalid sha256 digest length`},
		{input: "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85x", err: `invalid character in sha256 digest`},
		{
			input:     "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			algorithm: "sha256",
			encoded:   "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
	}
	for _, tC := range testCases {
		algorithm, encoded, err := ParseDigest(tC.input)
		if algorithm != tC.algorithm {
			t.Errorf("ParseDigest(%q): algorithm = %q, expected %q", tC.input, algorithm, tC.algorithm)
		}
		if encoded != tC.encoded {
			t.Errorf("ParseDigest(%q): encoded = %q, expected %q", tC.input, encoded, tC.encoded)
		}
		errStr := ""
		if err != nil {
			errStr = err.Error()
		}
		if errStr != tC.err {
			t.Errorf("ParseDigest(%q): err = %q, expected %q", tC.input, errStr, tC.err)
		}
	}
}
