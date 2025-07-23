// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package logtree

import (
	"errors"
	"slices"
	"strings"
	"sync"

	"source.monogon.dev/go/logging"
)

// DN is the Distinguished Name, a dot-delimited path used to address loggers
// within a LogTree. For example, "foo.bar" designates the 'bar' logger node
// under the 'foo' logger node under the root node of the logger. An empty
// string is the root node of the tree.
type DN string

var (
	ErrInvalidDN = errors.New("invalid DN")
)

// Path return the parts of a DN, ie. all the elements of the dot-delimited DN
// path.  For the root node, an empty list will be returned. An error will be
// returned if the DN is invalid (contains empty parts, eg. `foo..bar`, `.foo`
// or `foo.`.
func (d DN) Path() ([]string, error) {
	if d == "" {
		return nil, nil
	}
	parts := strings.Split(string(d), ".")
	for _, p := range parts {
		if p == "" {
			return nil, ErrInvalidDN
		}
	}
	return parts, nil
}

// journal is the main log recording structure of logtree. It manages linked lists
// containing the actual log entries, and implements scans across them. It does not
// understand the hierarchical nature of logtree, and instead sees all entries as
// part of a global linked list and a local linked list for a given DN.
//
// The global linked list is represented by the head/tail pointers in journal and
// nextGlobal/prevGlobal pointers in entries. The local linked lists are
// represented by heads[DN]/tails[DN] pointers in journal and nextLocal/prevLocal
// pointers in entries:
//
//	      .------------.        .------------.        .------------.
//	      | dn: A.B    |        | dn: Z      |        | dn: A.B    |
//	      | time: 1    |        | time: 2    |        | time: 3    |
//	      |------------|        |------------|        |------------|
//	      | nextGlobal :------->| nextGlobal :------->| nextGlobal :--> nil
//	nil <-: prevGlobal |<-------: prevGlobal |<-------| prevGlobal |
//	      |------------|        |------------|  n     |------------|
//	      | nextLocal  :---. n  | nextLocal  :->i .-->| nextLocal  :--> nil
//	nil <-: prevLocal  |<--: i<-: prevLocal  |  l :---| prevLocal  |
//	      '------------'   | l  '------------'    |   '------------'
//	           ^           '----------------------'         ^
//	           |                      ^                     |
//	           |                      |                     |
//	        ( head )             ( tails[Z] )            ( tail )
//	     ( heads[A.B] )          ( heads[Z] )         ( tails[A.B] )
type journal struct {
	// mu locks the rest of the structure. It must be taken during any operation on the
	// journal.
	mu sync.RWMutex

	// tail is the side of the global linked list that contains the newest log entry,
	// ie. the one that has been pushed the most recently. It can be nil when no log
	// entry has yet been pushed. The global linked list contains all log entries
	// pushed to the journal.
	tail *entry
	// head is the side of the global linked list that contains the oldest log entry.
	// It can be nil when no log entry has yet been pushed.
	head *entry

	// tails are the tail sides of a local linked list for a given DN, ie. the sides
	// that contain the newest entry. They are nil if there are no log entries for that
	// DN.
	tails map[DN]*entry
	// heads are the head sides of a local linked list for a given DN, ie. the sides
	// that contain the oldest entry. They are nil if there are no log entries for that
	// DN.
	heads map[DN]*entry

	// quota is a map from DN to quota structure, representing the quota policy of a
	// particular DN-designated logger.
	quota map[DN]*quota

	// subscribers are observer to logs. New log entries get emitted to channels
	// present in the subscriber structure, after filtering them through subscriber-
	// provided filters (eg. to limit events to subtrees that interest that particular
	// subscriber).
	subscribers []*subscriber

	// seq is a counter tracking the total amount of log entries appended since
	// creation.
	seq uint64
}

// newJournal creates a new empty journal. All journals are independent from
// eachother, and as such, all LogTrees are also independent.
func newJournal() *journal {
	return &journal{
		tails: make(map[DN]*entry),
		heads: make(map[DN]*entry),

		quota: make(map[DN]*quota),
	}
}

// filter is a predicate that returns true if a log subscriber or reader is
// interested in a given log entry.
type filter func(*entry) bool

// filterAll returns a filter that accepts all log entries.
func filterAll() filter {
	return func(*entry) bool { return true }
}

// filterExact returns a filter that accepts only log entries at a given exact
// DN.  This filter should not be used in conjunction with journal.scanEntries
// - instead, journal.getEntries should be used, as it is much faster.
func filterExact(dn DN) filter {
	return func(e *entry) bool {
		return e.origin == dn
	}
}

// filterSubtree returns a filter that accepts all log entries at a given DN and
// sub-DNs. For example, filterSubtree at "foo.bar" would allow entries at
// "foo.bar", "foo.bar.baz", but not "foo" or "foo.barr".
func filterSubtree(root DN) filter {
	if root == "" {
		return filterAll()
	}

	rootParts := strings.Split(string(root), ".")
	return func(e *entry) bool {
		parts := strings.Split(string(e.origin), ".")
		if len(parts) < len(rootParts) {
			return false
		}

		for i, p := range rootParts {
			if parts[i] != p {
				return false
			}
		}

		return true
	}
}

// filterSeverity returns a filter that accepts log entries at a given severity
// level or above. See the Severity type for more information about severity
// levels.
func filterSeverity(atLeast logging.Severity) filter {
	return func(e *entry) bool {
		return e.leveled != nil && e.leveled.severity.AtLeast(atLeast)
	}
}

func filterOnlyRaw(e *entry) bool {
	return e.raw != nil
}

func filterOnlyLeveled(e *entry) bool {
	return e.leveled != nil
}

func filterStartPosition(count int, pos int, direction ReadDirection) filter {
	return func(e *entry) bool {
		switch direction {
		case ReadDirectionAfter:
			return e.seqGlobal >= uint64(pos) && (count == BacklogAllAvailable || e.seqGlobal < uint64(pos+count))
		case ReadDirectionBefore:
			return e.seqGlobal < uint64(pos) && (count == BacklogAllAvailable || e.seqGlobal >= uint64(max(0, pos-count)))
		default:
			panic("unreachable")
		}
	}
}

// scanEntries does a linear scan through the global entry list and returns all
// entries that match the given filters. If retrieving entries for an exact event,
// getEntries should be used instead, as it will leverage DN-local linked lists to
// retrieve them faster. journal.mu must be taken at R or RW level when calling
// this function.
func (j *journal) scanEntries(count int, filters ...filter) (res []*entry) {
	cur := j.tail
	for {
		if cur == nil {
			break
		}

		passed := true
		for _, filter := range filters {
			if !filter(cur) {
				passed = false
				break
			}
		}
		if passed {
			res = append(res, cur)
		}
		if count != BacklogAllAvailable && len(res) >= count {
			break
		}
		cur = cur.prevGlobal
	}

	// Reverse entries back into chronological order.
	slices.Reverse(res)

	return
}

// getEntries returns all entries at a given DN. This is faster than a
// scanEntries(filterExact), as it uses the special local linked list pointers to
// traverse the journal. Additional filters can be passed to further limit the
// entries returned, but a scan through this DN's local linked list will be
// performed regardless. journal.mu must be taken at R or RW level when calling
// this function.
func (j *journal) getEntries(count int, exact DN, filters ...filter) (res []*entry) {
	cur := j.tails[exact]
	for {
		if cur == nil {
			break
		}

		passed := true
		for _, filter := range filters {
			if !filter(cur) {
				passed = false
				break
			}
		}
		if passed {
			res = append(res, cur)
		}
		if count != BacklogAllAvailable && len(res) >= count {
			break
		}
		cur = cur.prevLocal
	}

	// Reverse entries back into chronological order.
	slices.Reverse(res)

	return
}

// Shorten returns a shortened version of this DN for constrained logging
// environments like tty0 logging.
//
// If ShortenDictionary is given, it will be used to replace DN parts with
// shorter equivalents. For example, with the dictionary:
//
// { "foobar": "foo", "manager": "mgr" }
//
// The DN some.foobar.logger will be turned into some.foo.logger before further
// being processed by the shortening mechanism.
//
// The shortening rules applied are Metropolis-specific.
func (d DN) Shorten(dict ShortenDictionary, maxLen int) string {
	path, _ := d.Path()
	// Apply DN part shortening rules.
	if dict != nil {
		for i, p := range path {
			if sh, ok := dict[p]; ok {
				path[i] = sh
			}
		}
	}

	// This generally shouldn't happen.
	if len(path) == 0 {
		return "?"
	}

	// Strip 'root.' prefix.
	if len(path) > 1 && path[0] == "root" {
		path = path[1:]
	}

	// Replace role.xxx.yyy.zzz with xxx.zzz - stripping everything between the role
	// name and the last element of the path.
	if path[0] == "role" && len(path) > 1 {
		if len(path) == 2 {
			path = path[1:]
		} else {
			path = []string{
				path[1],
				path[len(path)-1],
			}
		}
	}

	// Join back to be ' '-delimited, and ellipsize if too long.
	s := strings.Join(path, " ")
	if overflow := len(s) - maxLen; overflow > 0 {
		s = "..." + s[overflow+3:]
	}
	return s
}

type ShortenDictionary map[string]string

var MetropolisShortenDict = ShortenDictionary{
	"controlplane":           "cplane",
	"map-cluster-membership": "map-membership",
	"cluster-membership":     "cluster",
	"controller-manager":     "controllers",
	"networking":             "net",
	"network":                "net",
	"interfaces":             "ifaces",
	"kubernetes":             "k8s",
}
