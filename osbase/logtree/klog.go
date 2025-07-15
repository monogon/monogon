// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package logtree

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog/v2"

	"source.monogon.dev/go/logging"
	"source.monogon.dev/osbase/logbuffer"
)

// KLogParser returns an io.WriteCloser to which raw logging from a klog emitter
// can be piped. It will attempt to parse all lines from this log as
// glog/klog-style entries, and pass them over to a LeveledLogger as if they were
// emitted locally.
//
// This allows for piping in external processes that emit klog logging into a
// logtree, leading to niceties like transparently exposing the log severity or
// source file/line.
//
// One caveat, however, is that V-leveled logs will not be translated
// appropriately - anything that the klog-emitter pumps out as Info will be
// directly ingested as Info logging. There is no way to work around this.
//
// Another important limitation is that any written line is interpreted as having
// happened recently (ie. within one hour of the time of execution of this
// function). This is important as klog/glog-formatted loglines don't have a year
// attached, so we have to infer it based on the current timestamp (note: parsed
// lines do not necessarily have their year aleays equal to the current year, as
// the code handles the edge case of parsing a line from the end of a previous
// year at the beginning of the next).
func KLogParser(logger logging.Leveled) io.WriteCloser {
	p, ok := logger.(*leveledPublisher)
	if !ok {
		// Fail fast, as this is a programming error.
		panic("Expected *leveledPublisher in LeveledLogger from supervisor")
	}

	k := &klogParser{
		publisher: p,
	}
	// klog seems to have no line length limit. Let's assume some sane sort of default.
	k.buffer = logbuffer.NewLineBuffer(1024, k.consumeLine)
	return k
}

type klogParser struct {
	publisher *leveledPublisher
	buffer    *logbuffer.LineBuffer
}

func (k *klogParser) Write(p []byte) (n int, err error) {
	return k.buffer.Write(p)
}

// Close must be called exactly once after the parser is done being used. It will
// pipe any leftover data in its write buffer as one last line to parse.
func (k *klogParser) Close() error {
	return k.buffer.Close()
}

// consumeLine is called by the internal LineBuffer any time a new line is fully
// written.
func (k *klogParser) consumeLine(l *logbuffer.Line) {
	p := parse(time.Now(), l.Data)
	if p == nil {
		// We could instead emit that line as a raw log - however, this would lead to
		// interleaving raw logging and leveled logging.
		k.publisher.Errorf("Invalid klog line: %s", l.Data)
		return
	}
	// TODO(q3k): should this be exposed as an API on LeveledLogger? How much should
	// we permit library users to 'fake' logs? This would also permit us to get rid
	// of the type assertion in KLogParser().
	e := &entry{
		origin:  k.publisher.node.dn,
		leveled: p,
	}
	k.publisher.node.tree.journal.append(e)
	k.publisher.node.tree.journal.notify(e)
}

var (
	// reKLog matches and parses klog/glog-formatted log lines. Format: I0312
	// 14:20:04.240540     204 shared_informer.go:247] Caches are synced for attach
	// detach
	reKLog = regexp.MustCompile(`^([IEWF])(\d{4})\s+(\d{2}:\d{2}:\d{2}(\.\d+)?)\s+(\d+)\s+([^:]+):(\d+)]\s+(.+)$`)
)

// parse attempts to parse a klog-formatted line. Returns nil if the line
// couldn't have been parsed successfully.
func parse(now time.Time, s string) *LeveledPayload {
	parts := reKLog.FindStringSubmatch(s)
	if parts == nil {
		return nil
	}

	severityS := parts[1]
	date := parts[2]
	timestamp := parts[3]
	pid := parts[5]
	file := parts[6]
	lineS := parts[7]
	message := parts[8]

	var severity logging.Severity
	switch severityS {
	case "I":
		severity = logging.INFO
	case "W":
		severity = logging.WARNING
	case "E":
		severity = logging.ERROR
	case "F":
		severity = logging.FATAL
	default:
		return nil
	}

	// Possible race due to klog's/glog's format not containing a year.
	// On 2020/12/31 at 23:59:59.99999 a klog logger emits this line:
	//
	//   I1231 23:59:59.99999 1 example.go:10] It's almost 2021! Hooray.
	//
	// Then, if this library parses that line at 2021/01/01 00:00:00.00001, the
	// time will be interpreted as:
	//
	//   2021/12/31 23:59:59
	//
	// So around one year in the future. We attempt to fix this case further down in
	// this function.
	year := now.Year()
	ts, err := parseKLogTime(year, date, timestamp)
	if err != nil {
		return nil
	}

	// Attempt to fix the aforementioned year-in-the-future issue.
	if ts.After(now) && ts.Sub(now) > time.Hour {
		// Parsed timestamp is in the future. How close is it to One-Year-From-Now?
		oyfn := now.Add(time.Hour * 24 * 365)
		dOyfn := ts.Sub(oyfn)
		// Let's make sure Duration-To-One-Year-From-Now is always positive. This
		// simplifies the rest of the checks and papers over some possible edge cases.
		if dOyfn < 0 {
			dOyfn = -dOyfn
		}

		// Okay, is that very close? Then the issue above happened and we should
		// attempt to reparse it with last year. We can't just manipulate the date we
		// already have, as it's difficult to 'subtract one year'.
		if dOyfn < (time.Hour * 24 * 2) {
			ts, err = parseKLogTime(year-1, date, timestamp)
			if err != nil {
				return nil
			}
		} else {
			// Otherwise, we received some seriously time traveling log entry. Abort.
			return nil
		}
	}

	line, err := strconv.Atoi(lineS)
	if err != nil {
		return nil
	}

	// The PID is discarded.
	_ = pid

	// Finally we have extracted all the data from the line. Inject into the log
	// publisher.
	return &LeveledPayload{
		timestamp: ts,
		severity:  severity,
		messages:  []string{message},
		file:      file,
		line:      line,
	}
}

// parseKLogTime parses a klog date and time (eg. "0314", "12:13:14.12345") into
// a time.Time happening at a given year.
func parseKLogTime(year int, d, t string) (time.Time, error) {
	var layout string
	if strings.Contains(t, ".") {
		layout = "2006 0102 15:04:05.000000"
	} else {
		layout = "2006 0102 15:04:05"
	}
	// Make up a string that contains the current year. This permits us to parse
	// fully into an actual timestamp.
	// TODO(q3k): add a timezone? This currently behaves as UTC, which is probably
	// what we want, but we should formalize this.
	return time.Parse(layout, fmt.Sprintf("%d %s %s", year, d, t))
}

// klogSink implements klog.LogSink on a logging.Leveld logger.
type klogSink struct {
	logging.Leveled
}

func (k *klogSink) Enabled(level int) bool {
	return k.V(logging.VerbosityLevel(level)).Enabled()
}

func (k *klogSink) Error(err error, msg string, keysAndValues ...any) {
	if err != nil {
		k.Errorf("%s: %w - %v", msg, err, keysAndValues)
	} else {
		k.Errorf("%s: %v", msg, keysAndValues)
	}
}

func (k *klogSink) Info(level int, msg string, keysAndValues ...any) {
	k.Infof("%s: %v", msg, keysAndValues)
}

func (k *klogSink) Init(info klog.RuntimeInfo) {
}

// WithName is only need if we would want to wrap the logger with a new
// name, but we don't need that.
func (k *klogSink) WithName(name string) klog.LogSink {
	k.Warning("logtree.klogSink: WithName() called but not implemented. This is a bug!")
	return k
}

// WithValues is only need if we would want to wrap the logger with a list
// of static keys/values, but we don't need that.
func (k *klogSink) WithValues(keysAndValues ...any) klog.LogSink {
	k.Warning("logtree.klogSink: WithValues() called but not implemented. This is a bug!")
	return k
}

func NewKlogLogger(logger logging.Leveled) klog.Logger {
	return klog.New(&klogSink{logger})
}
