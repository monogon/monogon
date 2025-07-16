// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Package bringup implements a simple wrapper which configures all default
// mounts, logging and the corresponding forwarder ttyS0. It then configures
// a new logtree and starts a supervisor to run the provided
// supervisor.Runnable. Said Runnable is expected to return no error. If it
// does, the supervisor will exit, an error will be printed and the system will
// reboot after five seconds.
package bringup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/opencontainers/runc/libcontainer/cgroups"
	"go.uber.org/multierr"
	"golang.org/x/sys/unix"

	"source.monogon.dev/go/logging"
	"source.monogon.dev/osbase/bootparam"
	"source.monogon.dev/osbase/efivarfs"
	"source.monogon.dev/osbase/logtree"
	"source.monogon.dev/osbase/supervisor"
)

type Config struct {
	Console    ConsoleConfig
	Supervisor SupervisorConfig
}

type ConsoleConfig struct {
	ShortenDictionary logtree.ShortenDictionary

	// Filter is used to filter out some uselessly verbose logs from the
	// console. It should return true if an entry is allowed to be printed.
	Filter func(*logtree.LogEntry) bool
}

type SupervisorConfig struct {
	Metrics []supervisor.Metrics
}

type Runnable supervisor.Runnable

func (r Runnable) Run() {
	r.RunWith(Config{})
}

func (r Runnable) RunWith(cfg Config) {
	if err := setupMounts(); err != nil {
		// We cannot do anything if we fail to mount.
		panic(err)
	}

	// Root system logtree.
	lt := logtree.New()

	// Collect serial consoles from cmdline and defaults.
	serialConsoles := collectConsoles()

	// Setup console writers
	if err := setupConsoles(lt, serialConsoles, cfg.Console); err != nil {
		panic(err)
	}

	// Initialize persistent panic handler
	initPanicHandler(lt, serialConsoles)

	// Rewire os.Stdout and os.Stderr to logtree which then is printed
	// to serial consoles.
	if err := rewireStdIO(lt); err != nil {
		panic(err)
	}

	// Initial logger. Used until we get to a supervisor.
	logger := lt.MustLeveledFor("init")

	sCtx, cancel := context.WithCancelCause(context.Background())

	supervisorOptions := []supervisor.SupervisorOpt{
		supervisor.WithExistingLogtree(lt),
	}

	for _, m := range cfg.Supervisor.Metrics {
		supervisorOptions = append(supervisorOptions, supervisor.WithMetrics(m))
	}

	// Don't reschedule the root runnable...
	var started atomic.Bool
	supervisor.New(sCtx, func(ctx context.Context) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("root runnable paniced: \n%s:\n%s", r, debug.Stack())
				cancel(err)
			}
		}()

		if started.Swap(true) {
			err = fmt.Errorf("root runnable restarted")
			cancel(err)
			return
		}

		if err := supervisor.Run(ctx, "pstore", dumpAndCleanPstore); err != nil {
			return fmt.Errorf("when starting pstore: %w", err)
		}

		err = r(ctx)
		if err != nil {
			cancel(err)
			return
		}

		return
	}, supervisorOptions...)

	<-sCtx.Done()

	time.Sleep(time.Second)

	// Write final messages on panic to stderr.
	logger.Errorf("Fatal error: %+v", context.Cause(sCtx))
	logger.Error("This node could not be started. Rebooting...")
	time.Sleep(time.Second)

	// After a bit, kill all console log readers.
	for _, c := range serialConsoles {
		if c.reader == nil {
			continue
		}
		c.reader.Close()
		c.reader.Stream = nil
	}

	// Wait for final logs to flush to console...
	time.Sleep(5 * time.Second)
	unix.Sync()
	unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
}

func rewireStdIO(lt *logtree.LogTree) error {
	if err := rewireFD(lt, "stderr", os.Stderr, logging.Leveled.Error); err != nil {
		return fmt.Errorf("failed rewiring stderr: %w", err)
	}
	if err := rewireFD(lt, "stdout", os.Stdout, logging.Leveled.Info); err != nil {
		return fmt.Errorf("failed rewiring stdout: %w", err)
	}
	return nil
}

func rewireFD(lt *logtree.LogTree, dn logtree.DN, f *os.File, writeLog func(logging.Leveled, ...any)) error {
	r, w, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("creating pipe for %q: %w", dn, err)
	}
	defer w.Close()
	// We don't need to close this pipe since we need it for the entire
	// process lifetime.

	l := lt.MustLeveledFor(dn)
	go func() {
		r := bufio.NewReader(r)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				panic(err)
			}

			writeLog(l, strings.TrimRight(line, "\n"))
		}
	}()

	wConn, err := w.SyscallConn()
	if err != nil {
		return fmt.Errorf("error getting SyscallConn for %q: %w", dn, err)
	}
	fConn, err := f.SyscallConn()
	if err != nil {
		return fmt.Errorf("error getting SyscallConn for %q: %w", dn, err)
	}
	var wErr, fErr error
	wErr = wConn.Control(func(wFd uintptr) {
		fErr = fConn.Control(func(fFd uintptr) {
			err = unix.Dup2(int(wFd), int(fFd))
		})
	})

	err = errors.Join(wErr, fErr, err)
	if err != nil {
		return fmt.Errorf("failed to duplicate file descriptor %q: %w", dn, err)
	}

	return nil
}

func mkdirAndMount(dir, fs string, flags uintptr, data string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("could not make %s: %w", dir, err)
	}
	if err := unix.Mount(fs, dir, fs, flags, data); err != nil {
		return fmt.Errorf("could not mount %s on %s: %w", fs, dir, err)
	}
	return nil
}

// setupMounts sets up basic mounts like sysfs, procfs, devtmpfs and cgroups.
// This should be called early during init as a lot of processes depend on this
// being available.
func setupMounts() (err error) {
	// Set up target filesystems.
	for _, el := range []struct {
		dir   string
		fs    string
		flags uintptr
		data  string
	}{
		{"/sys", "sysfs", unix.MS_NOEXEC | unix.MS_NOSUID | unix.MS_NODEV, ""},
		{"/sys/kernel/tracing", "tracefs", unix.MS_NOEXEC | unix.MS_NOSUID | unix.MS_NODEV, ""},
		{"/sys/fs/pstore", "pstore", unix.MS_NOEXEC | unix.MS_NOSUID | unix.MS_NODEV, ""},
		{"/proc", "proc", unix.MS_NOEXEC | unix.MS_NOSUID | unix.MS_NODEV, ""},
		{"/dev", "devtmpfs", unix.MS_NOEXEC | unix.MS_NOSUID, ""},
		{"/dev/pts", "devpts", unix.MS_NOEXEC | unix.MS_NOSUID, ""},
		// Nothing currently uses /dev/shm, but it's required
		// by containerd when the host IPC namespace is shared, which
		// is required by "kubectl debug node/" and specific customer applications.
		// https://github.com/monogon/monogon/issues/305.
		{"/dev/shm", "tmpfs", unix.MS_NOEXEC | unix.MS_NOSUID | unix.MS_NODEV, ""},
		{"/sys/fs/cgroup", "cgroup2", unix.MS_NOEXEC | unix.MS_NOSUID | unix.MS_NODEV, "nsdelegate,memory_recursiveprot"},
	} {
		err = multierr.Append(err, mkdirAndMount(el.dir, el.fs, el.flags, el.data))
	}

	// We try to mount efivarfs but ignore any error,
	// as we don't want to crash on non-EFI systems.
	_ = mkdirAndMount(efivarfs.Path, "efivarfs", unix.MS_NOEXEC|unix.MS_NOSUID|unix.MS_NODEV, "")

	// Create main cgroup "everything" and move ourselves into it.
	err = multierr.Append(err, os.Mkdir("/sys/fs/cgroup/everything", 0755))
	err = multierr.Append(err, cgroups.WriteCgroupProc("/sys/fs/cgroup/everything", os.Getpid()))
	return
}

type console struct {
	path     string
	maxWidth int
	reader   *logtree.LogReader
}

func collectConsoles() []*console {
	const defaultMaxWidth = 120

	// Add the standard ttyS0 console
	consoles := map[string]int{
		"ttyS0": defaultMaxWidth,
	}

	// Parse consoles from the kernel command line.
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err == nil {
		params, _, err := bootparam.Unmarshal(string(cmdline))
		if err == nil {
			for v := range params.Consoles() {
				consoles[v] = defaultMaxWidth
			}
		}
	}

	var serialConsoles []*console
	for consolePath, maxWidth := range consoles {
		serialConsoles = append(serialConsoles, &console{
			path:     "/dev/" + consolePath,
			maxWidth: maxWidth,
		})
	}

	return serialConsoles
}

func setupConsoles(lt *logtree.LogTree, serialConsoles []*console, ltc ConsoleConfig) error {
	filterFn := ltc.Filter
	if filterFn == nil {
		filterFn = func(*logtree.LogEntry) bool {
			return true
		}
	}

	// Open up consoles and set up logging from logtree and crash channel.
	for _, c := range serialConsoles {
		f, err := os.OpenFile(c.path, os.O_WRONLY, 0)
		if err != nil {
			continue
		}

		reader, err := lt.Read("", logtree.WithChildren(), logtree.WithStream())
		if err != nil {
			return fmt.Errorf("could not set up root log reader: %w", err)
		}
		c.reader = reader

		go func() {
			fmt.Fprintf(f, "This is %s. Verbose node logs follow.\n\n", f.Name())
			for p := range reader.Stream {
				if filterFn(p) {
					fmt.Fprintf(f, "%s\n", p.ConciseString(ltc.ShortenDictionary, c.maxWidth))
				}
			}
		}()
	}

	return nil
}
