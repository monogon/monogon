// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package verity

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	dm "source.monogon.dev/osbase/devicemapper"
)

const (
	// testDataSize configures the size of Verity-protected data devices.
	testDataSize int64 = 2 * 1024 * 1024
	// accessMode configures new files' permission bits.
	accessMode = 0600
)

// getRamdisk creates a device file pointing to an unused ramdisk.
// Returns a filesystem path.
func getRamdisk() (string, error) {
	for i := 0; ; i++ {
		path := fmt.Sprintf("/dev/ram%d", i)
		dn := unix.Mkdev(1, uint32(i))
		err := unix.Mknod(path, accessMode|unix.S_IFBLK, int(dn))
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		return path, nil
	}
}

// verityDMTarget returns a dm.Target based on a Verity mapping table.
func verityDMTarget(mt *MappingTable) *dm.Target {
	return &dm.Target{
		Type:        "verity",
		StartSector: 0,
		Length:      mt.Length(),
		Parameters:  mt.VerityParameterList(),
	}
}

// devZeroReader is a helper type used by writeRandomBytes.
type devZeroReader struct{}

// Read implements io.Reader on devZeroReader, making it a source of zero
// bytes.
func (devZeroReader) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = 0
	}
	return len(b), nil
}

// writeRandomBytes writes length pseudorandom bytes to a given io.Writer.
func writeRandomBytes(w io.Writer, length int64) error {
	keyiv := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	blkCipher, err := aes.NewCipher(keyiv)
	if err != nil {
		return err
	}
	var z devZeroReader
	c := cipher.StreamReader{S: cipher.NewCTR(blkCipher, keyiv), R: z}
	_, err = io.CopyN(w, c, length)
	return err
}

// fillVerityRamdisks fills a block device at dataDevPath with
// pseudorandom data and writes a complementary Verity hash device to
// a block device at hashDevPath. Returns a dm.Target configuring a
// resulting Verity device, and a buffer containing random data written
// the data device.
func fillVerityRamdisks(t *testing.T, dataDevPath, hashDevPath string) (*dm.Target, bytes.Buffer) {
	// Open the data device for writing.
	dfd, err := os.OpenFile(dataDevPath, os.O_WRONLY, accessMode)
	require.NoError(t, err, "while opening the data device at %s", dataDevPath)
	// Open the hash device for writing.
	hfd, err := os.OpenFile(hashDevPath, os.O_WRONLY, accessMode)
	require.NoError(t, err, "while opening the hash device at %s", hashDevPath)

	// Create a Verity encoder, backed with hfd. Configure it to write the
	// Verity superblock. Use 4096-byte blocks.
	bs := uint32(4096)
	salt := []byte("testsalt")
	verityEnc, err := NewEncoder(hfd, bs, bs, salt, true)
	require.NoError(t, err, "while creating a Verity encoder")

	// Write pseudorandom data both to the Verity-protected data device, and
	// into the Verity encoder, which in turn will write a resulting hash
	// tree to hfd on Close().
	var testData bytes.Buffer
	tdw := io.MultiWriter(dfd, verityEnc, &testData)
	err = writeRandomBytes(tdw, testDataSize)
	require.NoError(t, err, "while writing test data")

	// Close the file descriptors.
	err = verityEnc.Close()
	require.NoError(t, err, "while closing the Verity encoder")
	err = hfd.Close()
	require.NoError(t, err, "while closing the hash device descriptor")
	err = dfd.Close()
	require.NoError(t, err, "while closing the data device descriptor")

	// Generate the Verity mapping table based on the encoder state, device
	// file paths and the metadata starting block, then return it along with
	// the test data buffer.
	mt, err := verityEnc.MappingTable(dataDevPath, hashDevPath, 0)
	require.NoError(t, err, "while building a Verity mapping table")
	return verityDMTarget(mt), testData
}

// createVerityDevice maps a Verity device described by dmt while
// assigning it a name equal to devName. It returns a Verity device path.
func createVerityDevice(t *testing.T, dmt *dm.Target, devName string) string {
	devNum, err := dm.CreateActiveDevice(devName, true, []dm.Target{*dmt})
	require.NoError(t, err, "while creating a Verity device")

	devPath := fmt.Sprintf("/dev/%s", devName)
	err = unix.Mknod(devPath, accessMode|unix.S_IFBLK, int(devNum))
	require.NoError(t, err, "while creating a Verity device file at %s", devPath)
	return devPath
}

// cleanupVerityDevice deactivates a Verity device previously mapped by
// createVerityDevice, and removes an associated device file.
func cleanupVerityDevice(t *testing.T, devName string) {
	err := dm.RemoveDevice(devName)
	require.NoError(t, err, "while removing a Verity device %s", devName)

	devPath := fmt.Sprintf("/dev/%s", devName)
	err = os.Remove(devPath)
	require.NoError(t, err, "while removing a Verity device file at %s", devPath)
}

// testRead compares contents of a block device at devPath with
// expectedData. The length of data read is equal to the length
// of expectedData.
// It returns 'false', if either data could not be read or it does not
// match expectedData, and 'true' otherwise.
func testRead(t *testing.T, devPath string, expectedData []byte) bool {
	// Open the Verity device.
	verityDev, err := os.Open(devPath)
	require.NoError(t, err, "while opening a Verity device at %s", devPath)
	defer verityDev.Close()

	// Attempt to read the test data. Abort on read errors.
	readData := make([]byte, len(expectedData))
	_, err = io.ReadFull(verityDev, readData)
	if err != nil {
		return false
	}

	// Return true, if read data matches expectedData.
	if bytes.Equal(expectedData, readData) {
		return true
	}
	return false
}

// TestMakeAndRead attempts to create a Verity device, then verifies the
// integrity of its contents.
func TestMakeAndRead(t *testing.T) {
	if os.Getenv("IN_KTEST") != "true" {
		t.Skip("Not in ktest")
	}

	// Allocate block devices backing the Verity target.
	dataDevPath, err := getRamdisk()
	require.NoError(t, err, "while allocating a data device ramdisk")
	hashDevPath, err := getRamdisk()
	require.NoError(t, err, "while allocating a hash device ramdisk")

	// Fill the data device with test data and write a corresponding Verity
	// hash tree to the hash device.
	dmTarget, expectedDataBuf := fillVerityRamdisks(t, dataDevPath, hashDevPath)

	// Create a Verity device using dmTarget. Use the test name as a device
	// handle. verityPath will point to a resulting new block device.
	verityPath := createVerityDevice(t, dmTarget, t.Name())
	defer cleanupVerityDevice(t, t.Name())

	// Use testRead to compare Verity target device contents with test data
	// written to the data block device at dataDevPath by fillVerityRamdisks.
	if !testRead(t, verityPath, expectedDataBuf.Bytes()) {
		t.Error("data read from the verity device doesn't match the source")
	}
}

// TestMalformed checks whenever Verity would prevent reading from a
// target whose hash device contents have been corrupted, as is expected.
func TestMalformed(t *testing.T) {
	if os.Getenv("IN_KTEST") != "true" {
		t.Skip("Not in ktest")
	}

	// Allocate block devices backing the Verity target.
	dataDevPath, err := getRamdisk()
	require.NoError(t, err, "while allocating a data device ramdisk")
	hashDevPath, err := getRamdisk()
	require.NoError(t, err, "while allocating a hash device ramdisk")

	// Fill the data device with test data and write a corresponding Verity
	// hash tree to the hash device.
	dmTarget, expectedDataBuf := fillVerityRamdisks(t, dataDevPath, hashDevPath)

	// Corrupt the first hash device block before mapping the Verity target.
	hfd, err := os.OpenFile(hashDevPath, os.O_RDWR, accessMode)
	require.NoError(t, err, "while opening a hash device at %s", hashDevPath)
	// Place an odd byte at the 256th byte of the first hash block, skipping
	// a 4096-byte Verity superblock.
	hfd.Seek(4096+256, io.SeekStart)
	hfd.Write([]byte{'F'})
	hfd.Close()

	// Create a Verity device using dmTarget. Use the test name as a device
	// handle. verityPath will point to a resulting new block device.
	verityPath := createVerityDevice(t, dmTarget, t.Name())
	defer cleanupVerityDevice(t, t.Name())

	// Use testRead to compare Verity target device contents with test data
	// written to the data block device at dataDevPath by fillVerityRamdisks.
	// This step is expected to fail after an incomplete read.
	if testRead(t, verityPath, expectedDataBuf.Bytes()) {
		t.Error("data matches the source when it shouldn't")
	}
}
