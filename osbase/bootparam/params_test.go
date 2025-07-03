// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package bootparam_test

import (
	"testing"

	"source.monogon.dev/osbase/bootparam"
)

func TestConsoles(t *testing.T) {
	cases := []struct {
		name     string
		cmdline  string
		consoles []string
	}{
		{"Empty", "", []string{}},
		{"None", "notconsole=test", []string{}},
		{"Single", "asdf=ttyS1 console=ttyS0,115200", []string{"ttyS0"}},
		{"MultipleSame", "console=ttyS0 noop console=ttyS0", []string{"ttyS0"}},
		{"MultipleDiff", "console=tty27 console=ttyACM0", []string{"tty27", "ttyACM0"}},
		{"WithDev", "console=/dev/ttyXYZ0", []string{"ttyXYZ0"}},
		{"BrokenBadDev", "console=/etc/passwd", []string{}},
		{"BrokenNoValue", "console=", []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, _, err := bootparam.Unmarshal(c.cmdline)
			if err != nil {
				t.Fatalf("Failed to parse cmdline %q: %v", c.cmdline, err)
			}
			consoles := p.Consoles()
			wantConsoles := make(map[string]bool)
			for _, con := range c.consoles {
				wantConsoles[con] = true
			}
			for con := range wantConsoles {
				if !consoles[con] {
					t.Errorf("Expected console %s to be returned but it wasn't", con)
				}
			}
			for con := range consoles {
				if !wantConsoles[con] {
					t.Errorf("Didn't expect console %s to be returned but it was", con)
				}
			}
		})
	}
}
