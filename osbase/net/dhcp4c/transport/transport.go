// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Package transport contains Linux-based transports for the DHCP broadcast and
// unicast specifications.
package transport

import (
	"errors"
	"fmt"
	"net"
)

var ErrDeadlineExceeded = errors.New("deadline exceeded")

func NewInvalidMessageError(internalErr error) error {
	return &InvalidMessageError{internalErr: internalErr}
}

type InvalidMessageError struct {
	internalErr error
}

func (i InvalidMessageError) Error() string {
	return fmt.Sprintf("received invalid packet: %v", i.internalErr.Error())
}

func (i InvalidMessageError) Unwrap() error {
	return i.internalErr
}

func deadlineFromTimeout(err error) error {
	var timeoutErr net.Error
	if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
		return ErrDeadlineExceeded
	}
	return err
}
