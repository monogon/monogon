// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Package cmd contains helpers that abstract away the chore of starting new
// processes, tracking their lifetime, inspecting their output, etc.
package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"source.monogon.dev/osbase/logbuffer"
)

// RunCommand starts a new process and waits until either its completion, or
// until the supplied predicate function pf returns true. The function is called
// for each line produced by the new process.
//
// The returned boolean value equals the last value returned by pf.
//
// The process will be killed both in the event the context is cancelled, and
// when expectedOutput is found.
func RunCommand(ctx context.Context, path string, args []string, pf func(string) bool) (bool, error) {
	// Make a sub-context to ensure the process exits when this function is done.
	ctx, ctxC := context.WithCancel(ctx)
	defer ctxC()

	// Copy the stdout and stderr output to a single channel of lines so that they
	// can then be matched against expectedOutput.

	// Since LineBuffer can write its buffered contents on a deferred Close,
	// after the reader loop is broken, avoid deadlocks by making lineC a
	// buffered channel.
	lineC := make(chan string, 2)
	lineCB := func(l *logbuffer.Line) {
		// If the context is canceled, no-one is listening on lineC anymore, so we would
		// block.
		select {
		case <-ctx.Done():
			return
		case lineC <- l.Data:
		}
	}
	outBuffer := logbuffer.NewLineBuffer(1024, lineCB)
	defer outBuffer.Close()
	errBuffer := logbuffer.NewLineBuffer(1024, lineCB)
	defer errBuffer.Close()

	// Prepare the command context, and start the process.
	cmd := exec.CommandContext(ctx, path, args...)
	// Tee std{out,err} into the linebuffers above and the process' std{out,err}, to
	// allow easier debugging.
	cmd.Stdout = io.MultiWriter(os.Stdout, outBuffer)
	cmd.Stderr = io.MultiWriter(os.Stderr, errBuffer)
	if err := cmd.Start(); err != nil {
		return false, fmt.Errorf("couldn't start the process: %w", err)
	}

	// Handle the case in which the process finishes before pf takes the chance to
	// kill it.
	complC := make(chan error, 1)
	go func() {
		complC <- cmd.Wait()
	}()

	// Try matching against expectedOutput and return the result.
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case line := <-lineC:
			if pf(line) {
				cmd.Process.Kill()
				<-complC
				return true, nil
			}
		case err := <-complC:
			return false, err
		}
	}
}

// TerminateIfFound creates RunCommand predicates that instantly terminate
// program execution in the event the given string is found in any line
// produced. RunCommand will return true, if the string searched for was found,
// and false otherwise. If logf isn't nil, it will be called whenever a new
// line is received.
func TerminateIfFound(needle string, logf func(string)) func(string) bool {
	return func(haystack string) bool {
		if logf != nil {
			logf(haystack)
		}
		return strings.Contains(haystack, needle)
	}
}

// WaitUntilCompletion creates a RunCommand predicate that will make it wait
// for the process to exit on its own. If logf isn't nil, it will be called
// whenever a new line is received.
func WaitUntilCompletion(logf func(string)) func(string) bool {
	return func(line string) bool {
		if logf != nil {
			logf(line)
		}
		return false
	}
}
