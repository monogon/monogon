// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logtree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"source.monogon.dev/go/logging"
)

const (
	loglevelEmergency = 0
	loglevelAlert     = 1
	loglevelCritical  = 2
	loglevelError     = 3
	loglevelWarning   = 4
	loglevelNotice    = 5
	loglevelInfo      = 6
	loglevelDebug     = 7
)

// KmsgPipe pipes logs from the kernel kmsg interface at /dev/kmsg into the
// given logger.
func KmsgPipe(ctx context.Context, lt logging.Leveled) error {
	publisher, ok := lt.(*leveledPublisher)
	if !ok {
		// Fail fast, as this is a programming error.
		panic("Expected *leveledPublisher in LeveledLogger from supervisor")
	}
	kmsgFile, err := os.Open("/dev/kmsg")
	if err != nil {
		return err
	}
	defer kmsgFile.Close()
	var lastOverflow time.Time
	// PRINTK_MESSAGE_MAX in @linux//kernel/printk:internal.h
	linebuf := make([]byte, 2048)
	for {
		n, err := kmsgFile.Read(linebuf)
		// Best-effort, in Go it is not possible to cancel a Read on-demand.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if errors.Is(err, unix.EPIPE) {
			now := time.Now()
			// Rate-limit to 1 per second
			if lastOverflow.Add(1 * time.Second).Before(now) {
				lt.Warning("Lost messages due to kernel ring buffer overflow")
				lastOverflow = now
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("while reading from kmsg: %w", err)
		}
		var monotonicRaw unix.Timespec
		if err := unix.ClockGettime(unix.CLOCK_MONOTONIC_RAW, &monotonicRaw); err != nil {
			return fmt.Errorf("while getting monotonic timestamp: %w", err)
		}
		p := parseKmsg(time.Now(), time.Duration(monotonicRaw.Nano())*time.Nanosecond, linebuf[:n])
		if p == nil {
			continue
		}
		e := &entry{
			origin:  publisher.node.dn,
			leveled: p,
		}
		publisher.node.tree.journal.append(e)
		publisher.node.tree.journal.notify(e)
	}
}

// See https://www.kernel.org/doc/Documentation/ABI/testing/dev-kmsg for format.
func parseKmsg(now time.Time, monotonicSinceBoot time.Duration, data []byte) *LeveledPayload {
	meta, message, ok := bytes.Cut(data, []byte(";"))
	if !ok {
		// Unknown message format
		return nil
	}
	endOfMsgIdx := bytes.IndexByte(message, '\n')
	if endOfMsgIdx == -1 {
		return nil
	}
	message = message[:endOfMsgIdx]
	metaFields := strings.FieldsFunc(string(meta), func(r rune) bool { return r == ',' })
	if len(metaFields) < 4 {
		return nil
	}
	loglevel, err := strconv.ParseUint(metaFields[0], 10, 64)
	if err != nil {
		return nil
	}

	monotonicMicro, err := strconv.ParseUint(metaFields[2], 10, 64)
	if err != nil {
		return nil
	}

	// Kmsg entries are timestamped with CLOCK_MONOTONIC_RAW, a clock which does
	// not have a direct correspondence with civil time (UTC). To assign best-
	// effort timestamps, use the current monotonic clock reading to determine
	// the elapsed time between the kmsg entry and now on the monotonic clock.
	// This does not correspond well to elapsed UTC time on longer timescales as
	// CLOCK_MONOTONIC_RAW is not trimmed to run true to UTC, but up to in the
	// order of hours this is close. As the pipe generally processes messages
	// very close to their creation date, the elapsed time and thus the accrued
	// error is extremely small.
	monotonic := time.Duration(monotonicMicro) * time.Microsecond

	monotonicFromNow := monotonic - monotonicSinceBoot

	var severity logging.Severity
	switch loglevel {
	case loglevelEmergency, loglevelAlert:
		severity = logging.FATAL
	case loglevelCritical, loglevelError:
		severity = logging.ERROR
	case loglevelWarning:
		severity = logging.WARNING
	case loglevelNotice, loglevelInfo, loglevelDebug:
		severity = logging.INFO
	default:
		severity = logging.INFO
	}

	return &LeveledPayload{
		timestamp: now.Add(monotonicFromNow),
		severity:  severity,
		messages:  []string{string(message)},
	}
}
