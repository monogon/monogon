// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package logtree

import (
	"fmt"
	"strings"
	"testing"
)

func TestJournalStartPosition(t *testing.T) {
	lt := New()

	for i := 0; i < 100; i += 1 {
		e := &entry{
			origin:  "main",
			leveled: testPayload(fmt.Sprintf("test %d", i)),
		}
		lt.journal.append(e)
	}

	type tCase struct {
		name string

		count     int
		direction ReadDirection
		pos       int

		expectedCount int
		expectedFirst string
		expectedLast  string
	}

	for _, tc := range []tCase{
		{
			name:      "fetch all before id 0",
			count:     BacklogAllAvailable,
			direction: ReadDirectionBefore,
			pos:       0,

			expectedCount: 0,
			expectedFirst: "UNREACHABLE",
			expectedLast:  "UNREACHABLE",
		},
		{
			name:      "fetch all after id 0",
			count:     BacklogAllAvailable,
			direction: ReadDirectionAfter,
			pos:       0,

			expectedCount: 100,
			expectedFirst: "test 0",
			expectedLast:  "test 99",
		},

		{
			name:      "fetch all before id 10",
			count:     BacklogAllAvailable,
			direction: ReadDirectionBefore,
			pos:       10,

			expectedCount: 10,
			expectedFirst: "test 0",
			expectedLast:  "test 9",
		},
		{
			name:      "fetch all after id 10",
			count:     BacklogAllAvailable,
			direction: ReadDirectionAfter,
			pos:       10,

			expectedCount: 90,
			expectedFirst: "test 10",
			expectedLast:  "test 99",
		},

		{
			name:      "fetch 10 before id 0",
			count:     10,
			direction: ReadDirectionBefore,
			pos:       0,

			expectedCount: 0,
			expectedFirst: "UNREACHABLE",
			expectedLast:  "UNREACHABLE",
		},
		{
			name:      "fetch 10 after id 0",
			count:     10,
			direction: ReadDirectionAfter,
			pos:       0,

			expectedCount: 10,
			expectedFirst: "test 0",
			expectedLast:  "test 9",
		},

		{
			name:      "fetch 10 before id 3",
			count:     10,
			direction: ReadDirectionBefore,
			pos:       3,

			expectedCount: 3,
			expectedFirst: "test 0",
			expectedLast:  "test 2",
		},
		{
			name:      "fetch 10 after id 3",
			count:     10,
			direction: ReadDirectionAfter,
			pos:       3,

			expectedCount: 10,
			expectedFirst: "test 3",
			expectedLast:  "test 12",
		},
		{
			name:      "fetch 43 before id 47",
			count:     43,
			direction: ReadDirectionBefore,
			pos:       47,

			expectedCount: 43,
			expectedFirst: "test 4",
			expectedLast:  "test 46",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lr, _ := lt.Read("main",
				WithBacklog(tc.count),
				WithStartPosition(tc.pos, tc.direction),
			)
			if l := len(lr.Backlog); l != tc.expectedCount {
				t.Fatalf("expected %d entries, got %d", tc.expectedCount, l)
			}
			if len(lr.Backlog) == 0 {
				// If there is nothing to test against, skip to next test.
				return
			}
			if first := strings.Join(lr.Backlog[0].Leveled.messages, "\n"); first != tc.expectedFirst {
				t.Errorf("wanted first entry %q, got %q", tc.expectedFirst, first)
			}
			if last := strings.Join(lr.Backlog[len(lr.Backlog)-1].Leveled.messages, "\n"); last != tc.expectedLast {
				t.Errorf("wanted last entry %q, got %q", tc.expectedLast, last)
			}
			for i, entry := range lr.Backlog {
				// If we skip messages and are reading oldest first, adapt the
				// id to the expected position
				if tc.pos != 0 && tc.direction == ReadDirectionAfter {
					i = tc.pos + i
				}
				if tc.count != BacklogAllAvailable && tc.pos != 0 && tc.direction == ReadDirectionBefore {
					// Limit the negative offset to 0
					i = max(0, tc.pos-tc.count) + i
				}
				want := fmt.Sprintf("test %d", i)
				got := strings.Join(entry.Leveled.messages, "\n")
				if want != got {
					t.Errorf("wanted entry %q, got %q", want, got)
				}
			}
		})
	}
}
