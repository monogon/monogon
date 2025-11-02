// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logtree

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"source.monogon.dev/go/logging"
)

func TestParseKmsg(t *testing.T) {
	now := time.Unix(1691593045, 128027944)
	nowMonotonic := time.Duration(1501096434537722)

	for i, te := range []struct {
		line string
		want *LeveledPayload
	}{
		// Empty line
		{"", nil},
		// Unknown format
		{"Not a valid line", nil},
		// Normal entry
		{"6,30962,1501094342185,-;test\n", &LeveledPayload{
			messages:  []string{"test"},
			timestamp: time.Date(2023, 8, 9, 14, 57, 23, 35675222, time.UTC),
			severity:  logging.INFO,
		}},
		// With metadata and different severity
		{"4,30951,1486884175312,-;nvme nvme2: starting error recovery\n SUBSYSTEM=nvme\n DEVICE=c239:2\n", &LeveledPayload{
			messages:  []string{"nvme nvme2: starting error recovery"},
			timestamp: time.Date(2023, 8, 9, 11, 00, 32, 868802222, time.UTC),
			severity:  logging.WARNING,
		}},
	} {
		got := parseKmsg(now, nowMonotonic, []byte(te.line))
		if diff := cmp.Diff(te.want, got, cmp.AllowUnexported(LeveledPayload{})); diff != "" {
			t.Errorf("%d: mismatch (-want +got):\n%s", i, diff)
		}
	}
}
