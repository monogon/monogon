// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package logtree

import (
	"errors"
	"sync/atomic"

	"source.monogon.dev/go/logging"
)

// LogReadOption describes options for the LogTree.Read call.
type LogReadOption func(*logReaderOptions)

type logReaderOptions struct {
	withChildren               bool
	withStream                 bool
	withBacklog                int
	onlyLeveled                bool
	onlyRaw                    bool
	leveledWithMinimumSeverity logging.Severity
}

// WithChildren makes Read return/stream data for both a given DN and all its
// children.
func WithChildren() LogReadOption {
	return func(lro *logReaderOptions) {
		lro.withChildren = true
	}
}

// WithStream makes Read return a stream of data. This works alongside WithBacklog
// to create a read-and-stream construct.
func WithStream() LogReadOption {
	return func(lro *logReaderOptions) {
		lro.withStream = true
	}
}

// WithBacklog makes Read return already recorded log entries, up to count
// elements.
func WithBacklog(count int) LogReadOption {
	return func(lro *logReaderOptions) { lro.withBacklog = count }
}

// BacklogAllAvailable makes WithBacklog return all backlogged log data that
// logtree possesses.
const BacklogAllAvailable int = -1

func OnlyRaw() LogReadOption { return func(lro *logReaderOptions) { lro.onlyRaw = true } }

func OnlyLeveled() LogReadOption { return func(lro *logReaderOptions) { lro.onlyLeveled = true } }

// LeveledWithMinimumSeverity makes Read return only log entries that are at least
// at a given Severity. If only leveled entries are needed, OnlyLeveled must be
// used. This is a no-op when OnlyRaw is used.
func LeveledWithMinimumSeverity(s logging.Severity) LogReadOption {
	return func(lro *logReaderOptions) { lro.leveledWithMinimumSeverity = s }
}

// LogReader permits reading an already existing backlog of log entries and to
// stream further ones.
type LogReader struct {
	// Backlog are the log entries already logged by LogTree. This will only be set if
	// WithBacklog has been passed to Read.
	Backlog []*LogEntry
	// Stream is a channel of new entries as received live by LogTree. This will only
	// be set if WithStream has been passed to Read. In this case, entries from this
	// channel must be read as fast as possible by the consumer in order to prevent
	// missing entries.
	Stream <-chan *LogEntry
	// done is channel used to signal (by closing) that the log consumer is not
	// interested in more Stream data.
	done chan<- struct{}
	// missed is an atomic integer pointer that tells the subscriber how many messages
	// in Stream they missed. This pointer is nil if no streaming has been requested.
	missed *uint64
}

// Missed returns the amount of entries that were missed from Stream (as the
// channel was not drained fast enough).
func (l *LogReader) Missed() uint64 {
	// No Stream.
	if l.missed == nil {
		return 0
	}
	return atomic.LoadUint64(l.missed)
}

// Close closes the LogReader's Stream. This must be called once the Reader does
// not wish to receive streaming messages anymore.
func (l *LogReader) Close() {
	if l.done != nil {
		close(l.done)
	}
}

var (
	ErrRawAndLeveled = errors.New("cannot return logs that are simultaneously OnlyRaw and OnlyLeveled")
)

// Read and/or stream entries from a LogTree. The returned LogReader is influenced
// by the LogReadOptions passed, which influence whether the Read will return
// existing entries, a stream, or both. In addition the options also dictate
// whether only entries for that particular DN are returned, or for all sub-DNs as
// well.
func (l *LogTree) Read(dn DN, opts ...LogReadOption) (*LogReader, error) {
	l.journal.mu.RLock()
	defer l.journal.mu.RUnlock()

	var lro logReaderOptions

	for _, opt := range opts {
		opt(&lro)
	}

	if lro.onlyLeveled && lro.onlyRaw {
		return nil, ErrRawAndLeveled
	}

	var filters []filter
	if lro.onlyLeveled {
		filters = append(filters, filterOnlyLeveled)
	}
	if lro.onlyRaw {
		filters = append(filters, filterOnlyRaw)
	}
	if lro.withChildren {
		filters = append(filters, filterSubtree(dn))
	} else {
		filters = append(filters, filterExact(dn))
	}
	if lro.leveledWithMinimumSeverity != "" {
		filters = append(filters, filterSeverity(lro.leveledWithMinimumSeverity))
	}

	var entries []*entry
	if lro.withBacklog > 0 || lro.withBacklog == BacklogAllAvailable {
		if lro.withChildren {
			entries = l.journal.scanEntries(lro.withBacklog, filters...)
		} else {
			entries = l.journal.getEntries(lro.withBacklog, dn, filters...)
		}
	}

	var sub *subscriber
	if lro.withStream {
		sub = &subscriber{
			// TODO(q3k): make buffer size configurable
			dataC:   make(chan *LogEntry, 128),
			doneC:   make(chan struct{}),
			filters: filters,
		}
		l.journal.subscribe(sub)
	}

	lr := &LogReader{}
	lr.Backlog = make([]*LogEntry, len(entries))
	for i, entry := range entries {
		lr.Backlog[i] = entry.external()
	}
	if lro.withStream {
		lr.Stream = sub.dataC
		lr.done = sub.doneC
		lr.missed = &sub.missed
	}
	return lr, nil
}
