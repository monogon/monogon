// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// This is a test for PersistentVolumes provided by our provisioner. It tests
// that volumes have the right mount flags, and the expected quotas.
//
// The package here is a binary which will run in a Pod in our Kubernetes
// end-to-end test. See the function makeTestStatefulSet in
// metropolis/test/e2e/suites/kubernetes/kubernetes_helpers.go for how the Pod
// is created.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"source.monogon.dev/osbase/blockdev"
)

// This is a copy of the constant in metropolis/node/kubernetes/provisioner.go.
const inodeCapacityRatio = 4 * 512

var testCaseName = flag.String("name", "", "Name of the test case")
var hasBlock = flag.Bool("hasblock", false, "Has block device")

// checkFilesystemVolume checks that the filesystem containing path has the
// given mount flags and capacity.
func checkFilesystemVolume(path string, expectedFlags int64, expectedBytes uint64) error {
	var statfs unix.Statfs_t
	err := unix.Statfs(path, &statfs)
	if err != nil {
		return fmt.Errorf("failed to statfs volume %q: %w", path, err)
	}

	if statfs.Flags&unix.ST_RDONLY != expectedFlags&unix.ST_RDONLY {
		return fmt.Errorf("volume %q has readonly flag %v, expected the opposite", path, statfs.Flags&unix.ST_RDONLY != 0)
	}
	if statfs.Flags&unix.ST_NOSUID != expectedFlags&unix.ST_NOSUID {
		return fmt.Errorf("volume %q has nosuid flag %v, expected the opposite", path, statfs.Flags&unix.ST_NOSUID != 0)
	}
	if statfs.Flags&unix.ST_NODEV != expectedFlags&unix.ST_NODEV {
		return fmt.Errorf("volume %q has nodev flag %v, expected the opposite", path, statfs.Flags&unix.ST_NODEV != 0)
	}
	if statfs.Flags&unix.ST_NOEXEC != expectedFlags&unix.ST_NOEXEC {
		return fmt.Errorf("volume %q has noexec flag %v, expected the opposite", path, statfs.Flags&unix.ST_NOEXEC != 0)
	}

	sizeBytes := statfs.Blocks * uint64(statfs.Bsize)
	if sizeBytes != expectedBytes {
		return fmt.Errorf("volume %q has capacity %v bytes, expected %v bytes", path, sizeBytes, expectedBytes)
	}
	expectedFiles := expectedBytes / inodeCapacityRatio
	if statfs.Files != expectedFiles {
		return fmt.Errorf("volume %q has capacity for %v files, expected %v files", path, statfs.Files, expectedFiles)
	}

	// Try writing a file. This should only work if the volume is not read-only.
	err = os.WriteFile(filepath.Join(path, "test.txt"), []byte("hello"), 0o644)
	if expectedFlags&unix.ST_RDONLY != 0 {
		if err == nil {
			return fmt.Errorf("write did not fail in read-only volume %q", path)
		} else if !errors.Is(err, syscall.EROFS) {
			return fmt.Errorf("write failed with unexpected error in read-only volume %q: %w", path, err)
		}
	} else if err != nil {
		return fmt.Errorf("failed to write file in volume %q: %w", path, err)
	}

	return nil
}

func checkBlockVolume(path string, expectedBytes uint64) error {
	blk, err := blockdev.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open block device %q: %w", path, err)
	}
	defer blk.Close()
	sizeBytes := blk.BlockCount() * blk.BlockSize()
	if sizeBytes != int64(expectedBytes) {
		return fmt.Errorf("block device %q has size %v bytes, expected %v bytes", path, sizeBytes, expectedBytes)
	}
	return nil
}

func testPersistentVolume(expectedBytes uint64) error {
	if err := checkFilesystemVolume("/vol/default", 0, expectedBytes); err != nil {
		return err
	}
	if err := checkFilesystemVolume("/vol/readonly", unix.ST_RDONLY, expectedBytes); err != nil {
		return err
	}
	// Block volumes are not supported on gVisor or in user namespace.
	if *hasBlock {
		if err := checkBlockVolume("/vol/block", expectedBytes); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	flag.Parse()
	fmt.Printf("PersistentVolume tests starting on %s...\n", *testCaseName)

	if err := testPersistentVolume(1 * 1024 * 1024); err != nil {
		fmt.Println(err.Error())
		// The final log line communicates the test outcome to the e2e test.
		fmt.Println("[INIT-FAILED]")
	} else {
		fmt.Println("[INIT-PASSED]")

		nextLog := time.Now().Add(10 * time.Second)
		for {
			if err := testPersistentVolume(4 * 1024 * 1024); err != nil {
				if time.Now().After(nextLog) {
					fmt.Println("Waiting for resize:", err.Error())
					nextLog = time.Now().Add(10 * time.Second)
				}
				time.Sleep(100 * time.Millisecond)
			} else {
				fmt.Println("[RESIZE-PASSED]")
				break
			}
		}
	}

	// Sleep forever, because if the process exits, Kubernetes will restart it.
	for {
		time.Sleep(time.Hour)
	}
}
