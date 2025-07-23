// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package logtree

import (
	"fmt"
	"strings"

	"github.com/mitchellh/go-wordwrap"

	"source.monogon.dev/osbase/logbuffer"
	lpb "source.monogon.dev/osbase/logtree/proto"
)

// LogEntry contains a log entry, combining both leveled and raw logging into a
// single stream of events. A LogEntry will contain exactly one of either
// LeveledPayload or RawPayload.
type LogEntry struct {
	// If non-nil, this is a leveled logging entry.
	Leveled *LeveledPayload
	// If non-nil, this is a raw logging entry line.
	Raw *logbuffer.Line
	// DN from which this entry was logged.
	DN DN
	// Position of this entry in the global journal. This is only available
	// locally and is not set if the entry was obtained via protobuf.
	Position int
}

// String returns a canonical representation of this payload as a single string
// prefixed with metadata. If the entry is a leveled log entry that originally was
// logged with newlines this representation will also contain newlines, with each
// original message part prefixed by the metadata. For an alternative call that
// will instead return a canonical prefix and a list of lines in the message, see
// Strings().
func (l *LogEntry) String() string {
	if l.Leveled != nil {
		prefix, messages := l.Leveled.Strings()
		res := make([]string, len(messages))
		for i, m := range messages {
			res[i] = fmt.Sprintf("%-32s %s%s", l.DN, prefix, m)
		}
		return strings.Join(res, "\n")
	}
	if l.Raw != nil {
		return fmt.Sprintf("%-32s R %s", l.DN, l.Raw)
	}
	return "INVALID"
}

// ConciseString returns a concise representation of this log entry for
// constrained environments, like TTY consoles.
//
// The output format is as follows:
//
//	  shortened dn I Hello there
//	some component W Something went wrong
//	  shortened dn I Goodbye there
//	external stuff R I am en external process using raw logging.
//
// The above output is the result of calling ConciseString on three different
// LogEntries.
//
// If maxWidth is greater than zero, word wrapping will be applied. For example,
// with maxWidth set to 40:
//
//	     shortened I Hello there
//	some component W Something went wrong and here are the very long details that
//	               | describe this particular issue: according to all known laws of
//	               | aviation, there is no way a bee should be able to fly.
//	  shortened dn I Goodbye there
//	external stuff R I am en external process using raw logging.
//
// The above output is also the result of calling ConciseString on three
// different LogEntries.
//
// Multi-line log entries will emit 'continuation' lines (with '|') in the same
// way as word wrapping does. That means that even with word wrapping disabled,
// the result of this function might be multiline.
//
// The width of the first column (the 'shortened DN' column) is automatically
// selected based on maxWidth. If maxWidth is less than 60, the column will be
// omitted. For example, with maxWidth set to 20:
//
//	I Hello there
//	W Something went wrong and here are the very long details that
//	| describe this particular issue: according to all known laws of
//	| aviation, there is no way a bee should be able to fly.
//	I Goodbye there
//	R I am en external process using raw logging.
//
// The given `dict` implements simple replacement rules for shortening the DN
// parts of a log entry's DN. Some rules are hardcoded for Metropolis' DN tree.
// If no extra shortening rules should be applied, dict can be set to ni// The
// given `dict` implements simple replacement rules for shortening the DN parts
// of a log entry's DN. Some rules are hardcoded for Metropolis' DN tree. If no
// extra shortening rules should be applied, dict can be set to nil.
func (l *LogEntry) ConciseString(dict ShortenDictionary, maxWidth int) string {
	// Decide on a dnWidth.
	dnWidth := 0
	switch {
	case maxWidth >= 80:
		dnWidth = 20
	case maxWidth >= 60:
		dnWidth = 16
	case maxWidth <= 0:
		// No word wrapping.
		dnWidth = 20
	}

	// Compute shortened DN, if needed.
	sh := ""
	if dnWidth > 0 {
		sh = l.DN.Shorten(dict, dnWidth)
		sh = fmt.Sprintf("%*s ", dnWidth, sh)
	}

	// Prefix of the first line emitted.
	var prefix string
	switch {
	case l.Leveled != nil:
		prefix = sh + string(l.Leveled.Severity()) + " "
	case l.Raw != nil:
		prefix = sh + "R "
	}
	// Prefix of rest of lines emitted.
	continuationPrefix := strings.Repeat(" ", len(sh)) + "| "

	// Collect lines based on the type of LogEntry.
	var lines []string
	collect := func(message string) {
		if maxWidth > 0 {
			message = wordwrap.WrapString(message, uint(maxWidth-len(prefix)))
		}
		for _, m2 := range strings.Split(message, "\n") {
			if len(m2) == 0 {
				continue
			}
			if len(lines) == 0 {
				lines = append(lines, prefix+m2)
			} else {
				lines = append(lines, continuationPrefix+m2)
			}
		}
	}
	switch {
	case l.Leveled != nil:
		_, messages := l.Leveled.Strings()
		for _, m := range messages {
			collect(m)
		}
	case l.Raw != nil:
		collect(l.Raw.String())
	default:
		return ""
	}

	return strings.Join(lines, "\n")
}

// Strings returns the canonical representation of this payload split into a
// prefix and all lines that were contained in the original message. This is
// meant to be displayed to the user by showing the prefix before each line,
// concatenated together - possibly in a table form with the prefixes all
// unified with a rowspan- like mechanism.
//
// For example, this function can return:
//   prefix = "root.foo.bar                    I1102 17:20:06.921395     0 foo.go:42] "
//   lines = []string{"current tags:", " - one", " - two"}
//
// With this data, the result should be presented to users this way in text form:
// root.foo.bar                    I1102 17:20:06.921395 foo.go:42] current tags:
// root.foo.bar                    I1102 17:20:06.921395 foo.go:42]  - one
// root.foo.bar                    I1102 17:20:06.921395 foo.go:42]  - two
//
// Or, in a table layout:
// .----------------------------------------------------------------------.
// | root.foo.bar     I1102 17:20:06.921395 foo.go:42] : current tags:    |
// |                                                   :------------------|
// |                                                   :  - one           |
// |                                                   :------------------|
// |                                                   :  - two           |
// '----------------------------------------------------------------------'

func (l *LogEntry) Strings() (prefix string, lines []string) {
	if l.Leveled != nil {
		prefix, messages := l.Leveled.Strings()
		prefix = fmt.Sprintf("%-32s %s", l.DN, prefix)
		return prefix, messages
	}
	if l.Raw != nil {
		return fmt.Sprintf("%-32s R ", l.DN), []string{l.Raw.Data}
	}
	return "INVALID ", []string{"INVALID"}
}

// Proto converts this LogEntry to proto. Returned value may be nil if given
// LogEntry is invalid, eg. contains neither a Raw nor Leveled entry.
func (l *LogEntry) Proto() *lpb.LogEntry {
	p := &lpb.LogEntry{
		Dn: string(l.DN),
	}
	switch {
	case l.Leveled != nil:
		leveled := l.Leveled
		p.Kind = &lpb.LogEntry_Leveled_{
			Leveled: leveled.Proto(),
		}
	case l.Raw != nil:
		raw := l.Raw
		p.Kind = &lpb.LogEntry_Raw_{
			Raw: raw.ProtoLog(),
		}
	default:
		return nil
	}
	return p
}

// LogEntryFromProto parses a proto LogEntry back into internal structure.
// This can be used in log proto API consumers to easily print received log
// entries.
func LogEntryFromProto(l *lpb.LogEntry) (*LogEntry, error) {
	dn := DN(l.Dn)
	if _, err := dn.Path(); err != nil {
		return nil, fmt.Errorf("could not convert DN: %w", err)
	}
	res := &LogEntry{
		DN: dn,
	}
	switch inner := l.Kind.(type) {
	case *lpb.LogEntry_Leveled_:
		leveled, err := LeveledPayloadFromProto(inner.Leveled)
		if err != nil {
			return nil, fmt.Errorf("could not convert leveled entry: %w", err)
		}
		res.Leveled = leveled
	case *lpb.LogEntry_Raw_:
		line, err := logbuffer.LineFromLogProto(inner.Raw)
		if err != nil {
			return nil, fmt.Errorf("could not convert raw entry: %w", err)
		}
		res.Raw = line
	default:
		return nil, fmt.Errorf("proto has neither Leveled nor Raw set")
	}
	return res, nil
}
