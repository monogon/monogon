// Copyright 2021 The Cockroach Authors.
// SPDX-License-Identifier: Apache-2.0

package errwrap

// ErrorFnFormatStringIndex contains functions that should be checked for
// improperly wrapped errors. The value is the index of the function
// parameter containing the format string. It is -1 if there is no format
// string parameter.
var ErrorFnFormatStringIndex = map[string]int{
	"errors.New": -1,

	"github.com/pkg/errors.New":  -1,
	"github.com/pkg/errors.Wrap": -1,

	"fmt.Errorf": 0,

	"github.com/pkg/errors.Errorf": 0,
	"github.com/pkg/errors.Wrapf":  1,
}
