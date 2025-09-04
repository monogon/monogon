// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Package time implements a supervisor runnable which is responsible for
// keeping both the system clock and the RTC accurate.
// Metropolis nodes need accurate time both for themselves (for log
// timestamping, validating certain certificates, ...) as well as workloads
// running on top of it expecting accurate time.
// This initial implementation is very minimalistic, running just a stateless
// NTP client per node for the whole lifecycle of it.
// This implementation is simple, but is fairly unsafe as NTP by itself does
// not offer any cryptography, so it's easy to tamper with the responses.
// See #73 for further work in that direction.
package time

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"source.monogon.dev/metropolis/node/allocs"
	"source.monogon.dev/osbase/fileargs"
	"source.monogon.dev/osbase/supervisor"
)

// Service implements the time service. See package documentation for further
// information.
type Service struct{}

func New() *Service {
	return &Service{}
}

func (s *Service) Run(ctx context.Context) error {
	// TODO(#72): Apply for a NTP pool vendor zone
	config := strings.Join([]string{
		"pool ntp.monogon.dev iburst",
		"bindcmdaddress /",
		"stratumweight 0.01",
		"leapsecmode slew",
		"maxslewrate 10000",
		"makestep 2.0 3",
		"rtconutc",
		"rtcsync",
	}, "\n")
	args, err := fileargs.New()
	if err != nil {
		return fmt.Errorf("cannot create fileargs: %w", err)
	}
	defer args.Close()
	cmd := exec.CommandContext(ctx,
		"/time/chrony",
		"-d",
		"-i", strconv.Itoa(allocs.UidTime),
		"-g", strconv.Itoa(allocs.UidTime),
		"-f", args.ArgPath("chrony.conf", []byte(config)),
	)
	cmd.Stdout = supervisor.RawLogger(ctx)
	cmd.Stderr = supervisor.RawLogger(ctx)
	return supervisor.RunCommand(ctx, cmd)
}
