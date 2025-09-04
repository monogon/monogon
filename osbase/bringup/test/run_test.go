// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"

	"source.monogon.dev/osbase/test/cmd"
)

var (
	// These are filled by bazel at linking time with the canonical path of
	// their corresponding file. Inside the init function we resolve it
	// with the rules_go runfiles package to the real path.
	xOvmfCodePath      string
	xOvmfVarsPath      string
	xSucceedKernelPath string
	xPanicKernelPath   string
	xErrorKernelPath   string
	xQEMUPath          string
)

func init() {
	var err error
	for _, path := range []*string{
		&xOvmfCodePath, &xOvmfVarsPath, &xQEMUPath,
		&xSucceedKernelPath, &xPanicKernelPath, &xErrorKernelPath,
	} {
		*path, err = runfiles.Rlocation(*path)
		if err != nil {
			panic(err)
		}
	}
	// When running QEMU with snapshot=on set, QEMU creates a copy of the
	// provided file in $TMPDIR. If $TMPDIR is set to /tmp, it will always
	// be overridden to /var/tmp. This creates an issue for us, as the
	// bazel tests only wire up /tmp, with /var/tmp being unaccessible
	// because of permissions. Bazel provides $TEST_TMPDIR for this
	// usecase, which we resolve to an absolute path and then override
	// $TMPDIR.
	tmpDir, err := filepath.Abs(os.Getenv("TEST_TMPDIR"))
	if err != nil {
		panic(err)
	}
	os.Setenv("TMPDIR", tmpDir)
}

// runQemu starts a new QEMU process, expecting the given output to appear
// in any line printed. It returns true, if the expected string was found,
// and false otherwise.
//
// QEMU is killed shortly after the string is found, or when the context is
// cancelled.
func runQemu(ctx context.Context, args []string, expectedOutput string) (bool, error) {
	defaultArgs := []string{
		"-machine", "q35", "-accel", "kvm", "-nographic", "-nodefaults",
		"-m", "512",
		"-smp", "2",
		"-cpu", "host",
		"-drive", "if=pflash,format=raw,snapshot=on,file=" + xOvmfCodePath,
		"-drive", "if=pflash,format=raw,readonly=on,file=" + xOvmfVarsPath,
		"-serial", "stdio",
		"-no-reboot",
	}
	qemuArgs := append(defaultArgs, args...)
	pf := cmd.TerminateIfFound(expectedOutput, nil)
	return cmd.RunCommand(ctx, xQEMUPath, qemuArgs, pf)
}

func TestBringupSuccess(t *testing.T) {
	ctx, ctxC := context.WithTimeout(context.Background(), time.Second*30)
	defer ctxC()

	extraArgs := append([]string(nil), "-kernel", xSucceedKernelPath)

	// Run QEMU. Expect the installer to succeed with a predefined error string.
	expectedOutput := "_BRINGUP_LAUNCH_SUCCESS_"
	result, err := runQemu(ctx, extraArgs, expectedOutput)
	if err != nil {
		t.Error(err)
	}
	if !result {
		t.Errorf("QEMU didn't produce the expected output %q", expectedOutput)
	}
}
func TestBringupPanic(t *testing.T) {
	ctx, ctxC := context.WithTimeout(context.Background(), time.Second*30)
	defer ctxC()

	extraArgs := append([]string(nil), "-kernel", xPanicKernelPath)

	// Run QEMU. Expect the installer to fail with a predefined error string.
	expectedOutput := "root runnable paniced"
	result, err := runQemu(ctx, extraArgs, expectedOutput)
	if err != nil {
		t.Error(err)
	}
	if !result {
		t.Errorf("QEMU didn't produce the expected output %q", expectedOutput)
	}
}

func TestBringupError(t *testing.T) {
	ctx, ctxC := context.WithTimeout(context.Background(), time.Second*30)
	defer ctxC()

	extraArgs := append([]string(nil), "-kernel", xErrorKernelPath)

	// Run QEMU. Expect the installer to fail with a predefined error string.
	expectedOutput := "this is an error"
	result, err := runQemu(ctx, extraArgs, expectedOutput)
	if err != nil {
		t.Error(err)
	}
	if !result {
		t.Errorf("QEMU didn't produce the expected output %q", expectedOutput)
	}
}
