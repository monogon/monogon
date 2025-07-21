// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package logtree

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"source.monogon.dev/go/logging"
)

func testPayload(msg string) *LeveledPayload {
	return &LeveledPayload{
		messages:  []string{msg},
		timestamp: time.Now(),
		severity:  logging.INFO,
		file:      "main.go",
		line:      1337,
	}
}

func TestJournalRetention(t *testing.T) {
	j := newJournal()

	for i := 0; i < 9000; i += 1 {
		e := &entry{
			origin:  "main",
			leveled: testPayload(fmt.Sprintf("test %d", i)),
		}
		j.append(e)
	}

	entries := j.getEntries(BacklogAllAvailable, "main")
	if want, got := defaultDNQuota, len(entries); want != got {
		t.Fatalf("wanted %d entries, got %d", want, got)
	}
	for i, entry := range entries {
		want := fmt.Sprintf("test %d", (9000-defaultDNQuota)+i)
		got := strings.Join(entry.leveled.messages, "\n")
		if want != got {
			t.Fatalf("wanted entry %q, got %q", want, got)
		}
	}
}

func TestJournalQuota(t *testing.T) {
	j := newJournal()

	for i := 0; i < 9000; i += 1 {
		j.append(&entry{
			origin:  "chatty",
			leveled: testPayload(fmt.Sprintf("chatty %d", i)),
		})
		if i%10 == 0 {
			j.append(&entry{
				origin:  "solemn",
				leveled: testPayload(fmt.Sprintf("solemn %d", i)),
			})
		}
	}

	entries := j.getEntries(BacklogAllAvailable, "chatty")
	if want, got := defaultDNQuota, len(entries); want != got {
		t.Fatalf("wanted %d chatty entries, got %d", want, got)
	}
	entries = j.getEntries(BacklogAllAvailable, "solemn")
	if want, got := 900, len(entries); want != got {
		t.Fatalf("wanted %d solemn entries, got %d", want, got)
	}
	entries = j.getEntries(BacklogAllAvailable, "absent")
	if want, got := 0, len(entries); want != got {
		t.Fatalf("wanted %d absent entries, got %d", want, got)
	}

	entries = j.scanEntries(BacklogAllAvailable, filterAll())
	if want, got := defaultDNQuota+900, len(entries); want != got {
		t.Fatalf("wanted %d total entries, got %d", want, got)
	}
	setMessages := make(map[string]bool)
	for _, entry := range entries {
		setMessages[strings.Join(entry.leveled.messages, "\n")] = true
	}

	for i := 0; i < 900; i += 1 {
		want := fmt.Sprintf("solemn %d", i*10)
		if !setMessages[want] {
			t.Fatalf("could not find entry %q in journal", want)
		}
	}
	for i := 0; i < defaultDNQuota; i += 1 {
		want := fmt.Sprintf("chatty %d", i+(9000-defaultDNQuota))
		if !setMessages[want] {
			t.Fatalf("could not find entry %q in journal", want)
		}
	}
}

func TestJournalSubtree(t *testing.T) {
	j := newJournal()
	j.append(&entry{origin: "a", leveled: testPayload("a")})
	j.append(&entry{origin: "a.b", leveled: testPayload("a.b")})
	j.append(&entry{origin: "a.b.c", leveled: testPayload("a.b.c")})
	j.append(&entry{origin: "a.b.d", leveled: testPayload("a.b.d")})
	j.append(&entry{origin: "e.f", leveled: testPayload("e.f")})
	j.append(&entry{origin: "e.g", leveled: testPayload("e.g")})

	expect := func(f filter, msgs ...string) string {
		res := j.scanEntries(BacklogAllAvailable, f)
		set := make(map[string]bool)
		for _, entry := range res {
			set[strings.Join(entry.leveled.messages, "\n")] = true
		}

		for _, want := range msgs {
			if !set[want] {
				return fmt.Sprintf("missing entry %q", want)
			}
		}
		return ""
	}

	if res := expect(filterAll(), "a", "a.b", "a.b.c", "a.b.d", "e.f", "e.g"); res != "" {
		t.Fatalf("All: %s", res)
	}
	if res := expect(filterSubtree("a"), "a", "a.b", "a.b.c", "a.b.d"); res != "" {
		t.Fatalf("Subtree(a): %s", res)
	}
	if res := expect(filterSubtree("a.b"), "a.b", "a.b.c", "a.b.d"); res != "" {
		t.Fatalf("Subtree(a.b): %s", res)
	}
	if res := expect(filterSubtree("e"), "e.f", "e.g"); res != "" {
		t.Fatalf("Subtree(a.b): %s", res)
	}
}

func TestDN_Shorten(t *testing.T) {
	for i, te := range []struct {
		input  string
		maxLen int
		want   string
	}{
		{"root.role.controlplane.launcher.consensus.autopromoter", 20, "cplane autopromoter"},
		{"networking.interfaces", 20, "net ifaces"},
		{"hostsfile", 20, "hostsfile"},
		{"root.dhcp-server", 20, "dhcp-server"},
		{"root.role.kubernetes.run.kubernetes.apiserver", 20, "k8s apiserver"},
		{"some.very.long.dn.that.cant.be.shortened", 20, "...cant be shortened"},
		{"network.interfaces.dhcp", 20, "net ifaces dhcp"},
	} {
		got := DN(te.input).Shorten(MetropolisShortenDict, te.maxLen)
		if len(got) > te.maxLen {
			t.Errorf("case %d: output %q too long, got %d bytes, wanted %d", i, got, len(got), te.maxLen)
		} else {
			if te.want != got {
				t.Errorf("case %d: wanted %q, got %q", i, te.want, got)
			}
		}
	}
}
