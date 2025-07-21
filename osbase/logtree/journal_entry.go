// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package logtree

import "source.monogon.dev/osbase/logbuffer"

// entry is a journal entry, representing a single log event (encompassed in a
// Payload) at a given DN. See the journal struct for more information about the
// global/local linked lists.
type entry struct {
	// origin is the DN at which the log entry was recorded, or conversely, in which DN
	// it will be available at.
	origin DN
	// journal is the parent journal of this entry. An entry can belong only to a
	// single journal. This pointer is used to mutate the journal's head/tail pointers
	// when unlinking an entry.
	journal *journal
	// leveled is the leveled log entry for this entry, if this log entry was emitted
	// by leveled logging. Otherwise it is nil.
	leveled *LeveledPayload
	// raw is the raw log entry for this entry, if this log entry was emitted by raw
	// logging. Otherwise it is nil.
	raw *logbuffer.Line

	// prevGlobal is the previous entry in the global linked list, or nil if this entry
	// is the oldest entry in the global linked list.
	prevGlobal *entry
	// nextGlobal is the next entry in the global linked list, or nil if this entry is
	// the newest entry in the global linked list.
	nextGlobal *entry

	// prevLocal is the previous entry in this entry DN's local linked list, or nil if
	// this entry is the oldest entry in this local linked list.
	prevLocal *entry
	// prevLocal is the next entry in this entry DN's local linked list, or nil if this
	// entry is the newest entry in this local linked list.
	nextLocal *entry

	// seqLocal is a counter within a local linked list that increases by one each time
	// a new log entry is added. It is used to quickly establish local linked list
	// sizes (by subtracting seqLocal from both ends). This setup allows for O(1)
	// length calculation for local linked lists as long as entries are only unlinked
	// from the head or tail (which is the case in the current implementation).
	seqLocal uint64
}

// defaultDNQuota defines how many messages should be stored per DN.
const defaultDNQuota = 8192

// external returns a LogEntry object for this entry, ie. the public version of
// this object, without fields relating to the parent journal, linked lists,
// sequences, etc. These objects are visible to library consumers.
func (e *entry) external() *LogEntry {
	return &LogEntry{
		DN:      e.origin,
		Leveled: e.leveled,
		Raw:     e.raw,
	}
}

// unlink removes this entry from both global and local linked lists, updating the
// journal's head/tail pointers if needed. journal.mu must be taken as RW
func (e *entry) unlink() {
	// Unlink from the global linked list.
	if e.prevGlobal != nil {
		e.prevGlobal.nextGlobal = e.nextGlobal
	}
	if e.nextGlobal != nil {
		e.nextGlobal.prevGlobal = e.prevGlobal
	}
	// Update journal head/tail pointers.
	if e.journal.head == e {
		e.journal.head = e.nextGlobal
	}
	if e.journal.tail == e {
		e.journal.tail = e.prevGlobal
	}

	// Unlink from the local linked list.
	if e.prevLocal != nil {
		e.prevLocal.nextLocal = e.nextLocal
	}
	if e.nextLocal != nil {
		e.nextLocal.prevLocal = e.prevLocal
	}
	// Update journal head/tail pointers.
	if e.journal.heads[e.origin] == e {
		e.journal.heads[e.origin] = e.nextLocal
	}
	if e.journal.tails[e.origin] == e {
		e.journal.tails[e.origin] = e.prevLocal
	}
}

// quota describes the quota policy for logging at a given DN.
type quota struct {
	// origin is the exact DN that this quota applies to.
	origin DN
	// max is the maximum count of log entries permitted for this DN - ie, the maximum
	// size of the local linked list.
	max uint64
}

// append adds an entry at the head of the global and local linked lists.
func (j *journal) append(e *entry) {
	j.mu.Lock()
	defer j.mu.Unlock()

	e.journal = j

	// Insert at head in global linked list, set pointers.
	e.nextGlobal = nil
	e.prevGlobal = j.tail
	if j.tail != nil {
		j.tail.nextGlobal = e
	}
	j.tail = e
	if j.head == nil {
		j.head = e
	}

	// Create quota if necessary.
	if _, ok := j.quota[e.origin]; !ok {
		j.quota[e.origin] = &quota{origin: e.origin, max: defaultDNQuota}
	}

	// Insert at head in local linked list, calculate seqLocal, set pointers.
	e.nextLocal = nil
	e.prevLocal = j.tails[e.origin]
	if j.tails[e.origin] != nil {
		j.tails[e.origin].nextLocal = e
		e.seqLocal = e.prevLocal.seqLocal + 1
	} else {
		e.seqLocal = 0
	}
	j.tails[e.origin] = e
	if j.heads[e.origin] == nil {
		j.heads[e.origin] = e
	}

	// Apply quota to the local linked list that this entry got inserted to, ie. remove
	// elements in excess of the quota.max count.
	quota := j.quota[e.origin]
	count := (j.tails[e.origin].seqLocal - j.heads[e.origin].seqLocal) + 1
	if count > quota.max {
		// Keep popping elements off the head of the local linked list until quota is not
		// violated.
		left := count - quota.max
		cur := j.heads[e.origin]
		for {
			// This shouldn't happen if quota.max >= 1.
			if cur == nil {
				break
			}
			if left == 0 {
				break
			}
			el := cur
			cur = el.nextLocal
			// Unlinking the entry unlinks it from both the global and local linked lists.
			el.unlink()
			left -= 1
		}
	}
}
