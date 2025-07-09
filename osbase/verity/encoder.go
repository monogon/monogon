// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// This package implements the minimum of functionality needed to generate and
// map dm-verity images. It's provided in order to avoid a perceived higher
// long term cost of packaging, linking against and maintaining the original C
// veritysetup tool.
//
// dm-verity is a Linux device mapper target that allows integrity verification of
// a read-only block device. The block device whose integrity should be checked
// (the 'data device') must be first processed by a tool like veritysetup to
// generate a hash device and root hash.
// The original data device, hash device and root hash are then set up as a device
// mapper target, and any read performed from the data device through the verity
// target will be verified for integrity by Linux using the hash device and root
// hash.
//
// Internally, the hash device is a Merkle tree of all the bytes in the data
// device, layed out as layers of 'hash blocks'. Starting with data bytes, layers
// are built recursively, with each layer's output hash blocks becoming the next
// layer's data input, ending with the single root hash.
//
// For more information about the internals, see the Linux and cryptsetup
// upstream code:
//
// https://gitlab.com/cryptsetup/cryptsetup/wikis/DMVerity
package verity

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// superblock represents data layout inside of a dm-verity hash block
// device superblock. It follows a preexisting verity implementation:
//
// https://gitlab.com/cryptsetup/cryptsetup/wikis/DMVerity#verity-superblock-format
type superblock struct {
	// signature is the magic signature of a verity hash device superblock,
	// "verity\0\0".
	signature [8]byte
	// version specifies a superblock format. This structure describes version
	// '1'.
	version uint32
	// hashType defaults to '1' outside Chrome OS, according to scarce dm-verity
	// documentation.
	hashType uint32
	// uuid contains a UUID of the hash device.
	uuid [16]byte
	// algorithm stores an ASCII-encoded name of the hash function used.
	algorithm [32]byte

	// dataBlockSize specifies a size of a single data device block, in bytes.
	dataBlockSize uint32
	// hashBlockSize specifies a size of a single hash device block, in bytes.
	hashBlockSize uint32
	// dataBlocks contains a count of blocks available on the data device.
	dataBlocks uint64

	// saltSize encodes the size of hash block salt, up to the maximum of 256 bytes.
	saltSize uint16

	// padding
	_ [6]byte
	// exactly saltSize bytes of salt are prepended to data blocks before hashing.
	saltBuffer [256]byte
	// padding
	_ [168]byte
}

// newSuperblock builds a dm-verity hash device superblock based on
// hardcoded defaults. dataBlocks is the only field left for later
// initialization.
// It returns either a partially initialized superblock, or an error.
func newSuperblock() (*superblock, error) {
	// This implementation only handles SHA256-based verity hash images
	// with a specific 4096-byte block size.
	// Block sizes can be updated by adjusting the struct literal below.
	// A change of a hashing algorithm would require a refactor of
	// saltedDigest, and references to sha256.Size.
	//
	// Fill in the defaults (compare with superblock definition).
	sb := superblock{
		signature:     [8]byte{'v', 'e', 'r', 'i', 't', 'y', 0, 0},
		version:       1,
		hashType:      1,
		algorithm:     [32]byte{'s', 'h', 'a', '2', '5', '6'},
		dataBlockSize: 4096,
		hashBlockSize: 4096,
	}

	return &sb, nil
}

// salt returns a slice of sb.saltBuffer actually occupied by
// salt bytes, of sb.saltSize length.
func (sb *superblock) salt() []byte {
	return sb.saltBuffer[:int(sb.saltSize)]
}

// algorithmName returns a name of the algorithm used to hash data block
// digests.
func (sb *superblock) algorithmName() string {
	size := bytes.IndexByte(sb.algorithm[:], 0x00)
	return string(sb.algorithm[:size])
}

// saltedDigest computes and returns a SHA256 sum of a block prepended
// with a Superblock-defined salt.
func (sb *superblock) saltedDigest(data []byte) (digest [sha256.Size]byte) {
	h := sha256.New()
	h.Write(sb.salt())
	h.Write(data)
	copy(digest[:], h.Sum(nil))
	return
}

// dataBlocksPerHashBlock returns the count of hash operation outputs that
// fit in a hash device block. This is also the amount of data device
// blocks it takes to populate a hash device block.
func (sb *superblock) dataBlocksPerHashBlock() uint64 {
	return uint64(sb.hashBlockSize) / sha256.Size
}

// computeHashBlock reads at most sb.dataBlocksPerHashBlock blocks from
// the given reader object, returning a padded hash block of length
// defined by sb.hashBlockSize, the count of digests output, and an
// error, if encountered.
// In case a non-nil block is returned, it's guaranteed to contain at
// least one hash. An io.EOF signals that there is no more to be read.
func (sb *superblock) computeHashBlock(r io.Reader) ([]byte, uint64, error) {
	// dcnt stores the total count of data blocks processed, which is the
	// as the count of digests output.
	var dcnt uint64
	// Preallocate a whole hash block.
	hblk := bytes.NewBuffer(make([]byte, 0, sb.hashBlockSize))

	// For every data block, compute a hash and place it in hblk. Continue
	// till EOF.
	for b := uint64(0); b < sb.dataBlocksPerHashBlock(); b++ {
		dbuf := make([]byte, sb.dataBlockSize)
		// Attempt to read enough data blocks to make a complete hash block.
		n, err := io.ReadFull(r, dbuf)
		// If any data was read, make a hash and add it to the hash buffer.
		if n != 0 {
			hash := sb.saltedDigest(dbuf)
			hblk.Write(hash[:])
			dcnt++
		}
		// Handle the read errors.
		switch {
		case err == nil:
		case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
			// io.ReadFull returns io.ErrUnexpectedEOF after a partial read,
			// and io.EOF if no bytes were read. In both cases it's possible
			// to end up with a partially filled hash block.
			if hblk.Len() != 0 {
				// Return a zero-padded hash block if any hashes were written
				// to it, and signal that no more blocks can be built.
				res := hblk.Bytes()
				return res[:cap(res)], dcnt, io.EOF
			}
			// Return nil if the block doesn't contain any hashes.
			return nil, 0, io.EOF
		default:
			// Wrap unhandled read errors.
			return nil, 0, fmt.Errorf("while computing a hash block: %w", err)
		}
	}
	// Return a completely filled hash block.
	res := hblk.Bytes()
	return res[:cap(res)], dcnt, nil
}

// WriteTo writes a verity superblock to a given writer object.
// It returns the count of bytes written, and a write error, if
// encountered.
func (sb *superblock) WriteTo(w io.Writer) (int64, error) {
	// Write the superblock.
	if err := binary.Write(w, binary.LittleEndian, sb); err != nil {
		return -1, fmt.Errorf("while writing a header: %w", err)
	}

	// Get the padding size by substracting current offset from a hash block
	// size.
	co := binary.Size(sb)
	pbc := int(sb.hashBlockSize) - co
	if pbc <= 0 {
		return int64(co), fmt.Errorf("hash device block size smaller than dm-verity superblock")
	}

	// Write the padding bytes at the end of the block.
	n, err := w.Write(bytes.Repeat([]byte{0}, pbc))
	co += n
	if err != nil {
		return int64(co), fmt.Errorf("while writing padding: %w", err)
	}
	return int64(co), nil
}

// computeLevel produces a verity hash tree level based on data read from
// a given reader object.
// It returns a byte slice containing one or more hash blocks, or an
// error.
// BUG(mz): Current implementation requires a 1/128th of the data image
// size to be allocatable on the heap.
func (sb *superblock) computeLevel(r io.Reader) ([]byte, error) {
	// hbuf will store all the computed hash blocks.
	var hbuf bytes.Buffer
	// Compute one or more hash blocks, reading all data available in the
	// 'r' reader object, and write them into hbuf.
	for {
		hblk, _, err := sb.computeHashBlock(r)
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("while building a hash tree level: %w", err)
		}
		if hblk != nil {
			_, err := hbuf.Write(hblk)
			if err != nil {
				return nil, fmt.Errorf("while writing to hash block buffer: %w", err)
			}
		}
		if err == io.EOF {
			break
		}
	}
	return hbuf.Bytes(), nil
}

// hashTree stores hash tree levels, each level comprising one or more
// Verity hash blocks. Levels are ordered from bottom to top.
type hashTree [][]byte

// push appends a level to the hash tree.
func (ht *hashTree) push(nl []byte) {
	*ht = append(*ht, nl)
}

// top returns the topmost level of the hash tree.
func (ht *hashTree) top() []byte {
	if len(*ht) == 0 {
		return nil
	}
	return (*ht)[len(*ht)-1]
}

// WriteTo writes a verity-formatted hash tree to the given writer
// object.
// It returns a write error, if encountered.
func (ht *hashTree) WriteTo(w io.Writer) (int64, error) {
	// t keeps the count of bytes written to w.
	var t int64
	// Write the hash tree levels from top to bottom.
	for l := len(*ht) - 1; l >= 0; l-- {
		level := (*ht)[l]
		// Call w.Write until a whole level is written.
		for len(level) != 0 {
			n, err := w.Write(level)
			if err != nil {
				return t, fmt.Errorf("while writing a level: %w", err)
			}
			level = level[n:]
			t += int64(n)
		}
	}
	return t, nil
}

// MappingTable aggregates data needed to generate a complete Verity
// mapping table.
type MappingTable struct {
	// superblock defines the following elements of the mapping table:
	// - data device block size
	// - hash device block size
	// - total count of data blocks
	// - hash algorithm used
	// - cryptographic salt used
	superblock *superblock
	// DataDevicePath is the filesystem path of the data device used as part
	// of the Verity Device Mapper target.
	DataDevicePath string
	// HashDevicePath is the filesystem path of the hash device used as part
	// of the Verity Device Mapper target.
	HashDevicePath string
	// HashStart marks the starting block of the Verity hash tree.
	HashStart int64
	// rootHash stores a cryptographic hash of the top hash tree block.
	rootHash []byte
}

// VerityParameterList returns a list of Verity target parameters, ordered
// as they would appear in a parameter string.
func (t *MappingTable) VerityParameterList() []string {
	salt := hex.EncodeToString(t.superblock.salt())
	if salt == "" {
		salt = "-"
	}
	return []string{
		"1",
		t.DataDevicePath,
		t.HashDevicePath,
		strconv.FormatUint(uint64(t.superblock.dataBlockSize), 10),
		strconv.FormatUint(uint64(t.superblock.hashBlockSize), 10),
		strconv.FormatUint(t.superblock.dataBlocks, 10),
		strconv.FormatInt(t.HashStart, 10),
		t.superblock.algorithmName(),
		hex.EncodeToString(t.rootHash),
		salt,
	}
}

// TargetParameters returns the mapping table as a list of Device Mapper
// target parameters, ordered as they would appear in a parameter string
// (see: String).
func (t *MappingTable) TargetParameters() []string {
	return append(
		[]string{
			"0",
			strconv.FormatUint(t.Length(), 10),
			"verity",
		},
		t.VerityParameterList()...,
	)
}

// String returns a string-formatted mapping table for use with Device
// Mapper.
// BUG(mz): unescaped whitespace can appear in block device paths
func (t *MappingTable) String() string {
	return strings.Join(t.TargetParameters(), " ")
}

// Length returns the data device length, represented as a number of
// 512-byte sectors.
func (t *MappingTable) Length() uint64 {
	return t.superblock.dataBlocks * uint64(t.superblock.dataBlockSize) / 512
}

// encoder transforms data blocks written into it into a verity hash
// tree. It writes out the hash tree only after Close is called on it.
type encoder struct {
	// out is the writer object Encoder will write to.
	out io.Writer
	// writeSb, if true, will cause a Verity superblock to be written to the
	// writer object.
	writeSb bool
	// sb contains the most of information needed to build a mapping table.
	sb *superblock
	// bottom stands for the bottom level of the hash tree. It contains
	// complete hash blocks of data written to the encoder.
	bottom bytes.Buffer
	// dataBuffer stores incoming data for later processing.
	dataBuffer bytes.Buffer
	// rootHash stores the verity root hash set on Close.
	rootHash []byte
}

// computeHashTree builds a complete hash tree based on the encoder's
// state. Levels are appended to the returned hash tree starting from the
// bottom, with the top level written last.
// e.sb.dataBlocks is set according to the bottom level's length, which
// must be divisible by e.sb.hashBlockSize.
// e.rootHash is set on success.
// It returns an error, if encountered.
func (e *encoder) computeHashTree() (*hashTree, error) {
	// Put b at the bottom of the tree. Don't perform a deep copy.
	ht := hashTree{e.bottom.Bytes()}

	// Other levels are built by hashing the hash blocks comprising a level
	// below.
	for {
		if len(ht.top()) == int(e.sb.hashBlockSize) {
			// The last level to compute has a size of exactly one hash block.
			// That's the root level. Its hash serves as a cryptographic root of
			// trust and is saved into a encoder for later use.
			// In case the bottom level consists of only one hash block, no more
			// levels are computed.
			sd := e.sb.saltedDigest(ht.top())
			e.rootHash = sd[:]
			return &ht, nil
		}

		// Create the next level by hashing the previous one.
		nl, err := e.sb.computeLevel(bytes.NewReader(ht.top()))
		if err != nil {
			return nil, fmt.Errorf("while computing a level: %w", err)
		}
		// Append the resulting next level to a tree.
		ht.push(nl)
	}
}

// processDataBuffer processes data blocks contained in e.dataBuffer
// until no more data is available to form a completely filled hash block.
// If 'incomplete' is true, all remaining data in e.dataBuffer will be
// processed, producing a terminating incomplete block.
// It returns the count of data blocks processed, or an error, if
// encountered.
func (e *encoder) processDataBuffer(incomplete bool) (uint64, error) {
	// tdcnt stores the total count of data blocks processed.
	var tdcnt uint64
	// Compute the count of bytes needed to produce a complete hash block.
	bph := e.sb.dataBlocksPerHashBlock() * uint64(e.sb.dataBlockSize)

	// Iterate until no more data is available in e.dbuf.
	for uint64(e.dataBuffer.Len()) >= bph || incomplete && e.dataBuffer.Len() != 0 {
		hb, dcnt, err := e.sb.computeHashBlock(&e.dataBuffer)
		if err != nil && err != io.EOF {
			return 0, fmt.Errorf("while processing a data buffer: %w", err)
		}
		// Increment the total count of data blocks processed.
		tdcnt += dcnt
		// Write the resulting hash block into the level-zero buffer.
		e.bottom.Write(hb[:])
	}
	return tdcnt, nil
}

// NewEncoder returns a fully initialized encoder, or an error. The
// encoder will write to the given io.Writer object.
// A verity superblock will be written, preceding the hash tree, if
// writeSb is true.
func NewEncoder(out io.Writer, dataBlockSize, hashBlockSize uint32, salt []byte, writeSb bool) (*encoder, error) {
	sb, err := newSuperblock()
	if err != nil {
		return nil, fmt.Errorf("while creating a superblock: %w", err)
	}
	sb.dataBlockSize = dataBlockSize
	sb.hashBlockSize = hashBlockSize
	sb.saltSize = uint16(copy(sb.saltBuffer[:], salt))

	e := encoder{
		out:     out,
		writeSb: writeSb,
		sb:      sb,
	}
	return &e, nil
}

// Write hashes raw data to form the bottom hash tree level.
// It returns the number of bytes written, and an error, if encountered.
func (e *encoder) Write(data []byte) (int, error) {
	// Copy the input into the data buffer.
	n, _ := e.dataBuffer.Write(data)
	// Process only enough data to form a complete hash block. This may
	// leave excess data in e.dbuf to be processed later on.
	dcnt, err := e.processDataBuffer(false)
	if err != nil {
		return n, fmt.Errorf("while processing the data buffer: %w", err)
	}
	// Update the superblock with the count of data blocks written.
	e.sb.dataBlocks += dcnt
	return n, nil
}

// Close builds a complete hash tree based on cached bottom level blocks,
// then writes it to a preconfigured io.Writer object. A Verity superblock
// is written, if e.writeSb is true. No data, nor the superblock is written
// if the encoder is empty.
// It returns an error, if one was encountered.
func (e *encoder) Close() error {
	// Process all buffered data, including data blocks that may not form
	// a complete hash block.
	dcnt, err := e.processDataBuffer(true)
	if err != nil {
		return fmt.Errorf("while processing the data buffer: %w", err)
	}
	// Update the superblock with the count of data blocks written.
	e.sb.dataBlocks += dcnt

	// Don't write anything if nothing was written to the encoder.
	if e.bottom.Len() == 0 {
		return nil
	}

	// Compute remaining hash tree levels based on the bottom level: e.bottom.
	ht, err := e.computeHashTree()
	if err != nil {
		return fmt.Errorf("while encoding a hash tree: %w", err)
	}

	// Write the Verity superblock if the encoder was configured to do so.
	if e.writeSb {
		if _, err = e.sb.WriteTo(e.out); err != nil {
			return fmt.Errorf("while writing a superblock: %w", err)
		}
	}
	// Write the hash tree.
	_, err = ht.WriteTo(e.out)
	if err != nil {
		return fmt.Errorf("while writing a hash tree: %w", err)
	}

	return nil
}

// MappingTable returns a complete, string-convertible Verity target mapping
// table for use with Device Mapper, or an error. Close must be called on the
// encoder before calling this function. dataDevicePath, hashDevicePath, and
// hashStart parameters are parts of the mapping table. See:
// https://www.kernel.org/doc/html/latest/admin-guide/device-mapper/verity.html
func (e *encoder) MappingTable(dataDevicePath, hashDevicePath string, hashStart int64) (*MappingTable, error) {
	if e.rootHash == nil {
		if e.bottom.Len() != 0 {
			return nil, fmt.Errorf("encoder wasn't closed")
		}
		return nil, fmt.Errorf("encoder is empty")
	}

	if e.writeSb {
		// Account for the superblock.
		hashStart += 1
	}
	return &MappingTable{
		superblock:     e.sb,
		DataDevicePath: dataDevicePath,
		HashDevicePath: hashDevicePath,
		HashStart:      hashStart,
		rootHash:       e.rootHash,
	}, nil
}
