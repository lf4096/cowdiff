package cowdiff

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
)

// SegmentType classifies a diff segment.
type SegmentType uint8

const (
	SegData SegmentType = iota // literal bytes stored in the object
	SegZero                    // hole / discard (zeros on apply)
)

// Segment is one contiguous change in the target file.
type Segment struct {
	Offset uint64
	Length uint64
	Type   SegmentType
}

// Header describes a diff object.
type Header struct {
	Version    uint32
	TargetSize uint64
	FromHash   string // optional, caller-supplied; hash of the full file this diff applies onto (never computed by the tool)
	Segments   []Segment
}

const (
	magic           = "COWDIFF\x01"
	formatVersion   = 1
	flagHasFromHash = 1 << 0
	headerFixedSize = 32 // magic(8) + version(4) + flags(4) + targetSize(8) + segCount(8)
	dirEntrySize    = 25 // offset(8) + length(8) + type(1) + dataOff(8)
	fromHashSize    = 32
	checksumSize    = sha256.Size
	maxSegCount     = 1 << 32 // guards allocation against corrupt input
)

var (
	errBadMagic = errors.New("cowdiff: bad magic")
	errChecksum = errors.New("cowdiff: checksum mismatch")
)

// outSeg is a segment to encode. For SegData, dataReader yields exactly length bytes.
type outSeg struct {
	offset     uint64
	length     uint64
	typ        SegmentType
	dataReader io.Reader
}

// rawSeg is a parsed directory entry.
type rawSeg struct {
	offset  uint64
	length  uint64
	typ     SegmentType
	dataOff uint64
}

// parsedDiff is a fully decoded, checksum-verified diff object.
type parsedDiff struct {
	targetSize uint64
	fromHash   []byte
	segs       []rawSeg
	data       []byte
}

// writeObject encodes a diff object: header, segment directory, data section, checksum trailer.
func writeObject(w io.Writer, targetSize uint64, fromHash []byte, segs []outSeg) (*Header, error) {
	if len(fromHash) != 0 && len(fromHash) != fromHashSize {
		return nil, fmt.Errorf("cowdiff: from_hash must be %d bytes, got %d", fromHashSize, len(fromHash))
	}
	var flags uint32
	if len(fromHash) == fromHashSize {
		flags |= flagHasFromHash
	}

	h := sha256.New()
	mw := io.MultiWriter(w, h)

	hdr := make([]byte, 0, headerFixedSize+fromHashSize)
	hdr = append(hdr, magic...)
	hdr = binary.LittleEndian.AppendUint32(hdr, formatVersion)
	hdr = binary.LittleEndian.AppendUint32(hdr, flags)
	hdr = binary.LittleEndian.AppendUint64(hdr, targetSize)
	hdr = binary.LittleEndian.AppendUint64(hdr, uint64(len(segs)))
	if flags&flagHasFromHash != 0 {
		hdr = append(hdr, fromHash...)
	}
	if _, err := mw.Write(hdr); err != nil {
		return nil, err
	}

	dir := make([]byte, len(segs)*dirEntrySize)
	pub := make([]Segment, len(segs))
	var dataOff uint64
	for i := range segs {
		s := &segs[i]
		b := dir[i*dirEntrySize : (i+1)*dirEntrySize]
		binary.LittleEndian.PutUint64(b[0:8], s.offset)
		binary.LittleEndian.PutUint64(b[8:16], s.length)
		b[16] = byte(s.typ)
		if s.typ == SegData {
			binary.LittleEndian.PutUint64(b[17:25], dataOff)
			dataOff += s.length
		}
		pub[i] = Segment{Offset: s.offset, Length: s.length, Type: s.typ}
	}
	if _, err := mw.Write(dir); err != nil {
		return nil, err
	}

	for i := range segs {
		s := &segs[i]
		if s.typ != SegData || s.length == 0 {
			continue
		}
		if _, err := io.CopyN(mw, s.dataReader, int64(s.length)); err != nil {
			return nil, err
		}
	}

	if _, err := w.Write(h.Sum(nil)); err != nil {
		return nil, err
	}

	return &Header{
		Version:    formatVersion,
		TargetSize: targetSize,
		FromHash:   hashString(fromHash),
		Segments:   pub,
	}, nil
}

// readHeader decodes the fixed header, optional from_hash, and the segment
// directory. On return the reader is positioned at the data section.
// parseFixedHeader validates the 32-byte fixed header and returns its fields.
// It is the single source of header validation shared by readHeader and Verify,
// so both accept and reject exactly the same objects.
func parseFixedHeader(fixed []byte) (targetSize, segCount uint64, hasFromHash bool, err error) {
	if string(fixed[0:8]) != magic {
		return 0, 0, false, errBadMagic
	}
	if ver := binary.LittleEndian.Uint32(fixed[8:12]); ver != formatVersion {
		return 0, 0, false, fmt.Errorf("cowdiff: unsupported version %d", ver)
	}
	flags := binary.LittleEndian.Uint32(fixed[12:16])
	targetSize = binary.LittleEndian.Uint64(fixed[16:24])
	segCount = binary.LittleEndian.Uint64(fixed[24:32])
	if flags&^flagHasFromHash != 0 {
		return 0, 0, false, fmt.Errorf("cowdiff: unknown header flags 0x%x", flags)
	}
	if targetSize > uint64(math.MaxInt64) {
		return 0, 0, false, fmt.Errorf("cowdiff: target_size %d too large", targetSize)
	}
	// Segments are non-overlapping and >=1 byte within [0,targetSize), so a
	// valid object has segCount <= targetSize; this also bounds the directory.
	if segCount > targetSize || segCount > maxSegCount {
		return 0, 0, false, fmt.Errorf("cowdiff: implausible segment count %d (target_size %d)", segCount, targetSize)
	}
	return targetSize, segCount, flags&flagHasFromHash != 0, nil
}

// decodeSeg parses one 25-byte directory entry.
func decodeSeg(b []byte) rawSeg {
	return rawSeg{
		offset:  binary.LittleEndian.Uint64(b[0:8]),
		length:  binary.LittleEndian.Uint64(b[8:16]),
		typ:     SegmentType(b[16]),
		dataOff: binary.LittleEndian.Uint64(b[17:25]),
	}
}

func readHeader(r io.Reader) (targetSize uint64, fromHash []byte, segs []rawSeg, err error) {
	fixed := make([]byte, headerFixedSize)
	if _, err = io.ReadFull(r, fixed); err != nil {
		return
	}
	targetSize, segCount, hasFromHash, err := parseFixedHeader(fixed)
	if err != nil {
		return
	}
	if hasFromHash {
		fromHash = make([]byte, fromHashSize)
		if _, err = io.ReadFull(r, fromHash); err != nil {
			return
		}
	}
	dir, err := readExact(r, segCount*dirEntrySize)
	if err != nil {
		return
	}
	segs = make([]rawSeg, segCount)
	for i := range segs {
		segs[i] = decodeSeg(dir[i*dirEntrySize:])
	}
	err = validateSegs(targetSize, segs)
	return
}

// validateSegs rejects a directory that is not well-formed: unknown segment
// types, zero-length or out-of-bounds/overflowing ranges, unsorted or
// overlapping segments, and non-canonical or overflowing DATA offsets. Without
// this a checksum-valid but malformed object could panic or make the apply
// paths disagree.
func validateSegs(targetSize uint64, segs []rawSeg) error {
	var prevEnd, dataLen uint64
	for i := range segs {
		s := segs[i]
		if err := validateEntry(uint64(i), s.offset, s.length, s.typ, s.dataOff, &prevEnd, &dataLen, targetSize); err != nil {
			return err
		}
	}
	return nil
}

// validateEntry checks one directory entry against the running end/data-length
// invariants; shared by validateSegs and the incremental Verify parser.
func validateEntry(i, offset, length uint64, typ SegmentType, dataOff uint64, prevEnd, dataLen *uint64, targetSize uint64) error {
	if typ != SegData && typ != SegZero {
		return fmt.Errorf("cowdiff: segment %d has unknown type %d", i, typ)
	}
	if length == 0 {
		return fmt.Errorf("cowdiff: segment %d has zero length", i)
	}
	if offset < *prevEnd {
		return fmt.Errorf("cowdiff: segment %d unsorted or overlapping", i)
	}
	end := offset + length
	if end < offset || end > targetSize {
		return fmt.Errorf("cowdiff: segment %d out of bounds", i)
	}
	if typ == SegData {
		if dataOff != *dataLen {
			return fmt.Errorf("cowdiff: segment %d has non-canonical data_off", i)
		}
		next := *dataLen + length
		if next < *dataLen {
			return fmt.Errorf("cowdiff: data section length overflow")
		}
		*dataLen = next
	}
	*prevEnd = end
	return nil
}

// ReadHeader parses a diff's header and segment directory without its data.
func ReadHeader(r io.Reader) (*Header, error) {
	ts, fh, segs, err := readHeader(r)
	if err != nil {
		return nil, err
	}
	pub := make([]Segment, len(segs))
	for i, s := range segs {
		pub[i] = Segment{Offset: s.offset, Length: s.length, Type: s.typ}
	}
	return &Header{Version: formatVersion, TargetSize: ts, FromHash: hashString(fh), Segments: pub}, nil
}

// parseDiff decodes an entire diff object into memory and verifies its checksum.
func parseDiff(r io.Reader) (*parsedDiff, error) {
	h := sha256.New()
	tr := io.TeeReader(r, h)
	ts, fh, segs, err := readHeader(tr)
	if err != nil {
		return nil, err
	}
	var dataLen uint64
	for _, s := range segs {
		if s.typ == SegData {
			dataLen += s.length
		}
	}
	data, err := readExact(tr, dataLen)
	if err != nil {
		return nil, err
	}
	sum := make([]byte, checksumSize)
	if _, err := io.ReadFull(r, sum); err != nil {
		return nil, err
	}
	if !bytes.Equal(sum, h.Sum(nil)) {
		return nil, errChecksum
	}
	if err := expectEOF(r); err != nil {
		return nil, err
	}
	return &parsedDiff{targetSize: ts, fromHash: fh, segs: segs, data: data}, nil
}

// expectEOF errors if any bytes remain, so an object with trailing junk (or a
// second concatenated object) is rejected rather than silently accepted.
func expectEOF(r io.Reader) error {
	var b [1]byte
	n, err := io.ReadFull(r, b[:])
	if n > 0 {
		return errors.New("cowdiff: trailing bytes after object")
	}
	if err == io.EOF {
		return nil
	}
	return err // propagate a real read error rather than reporting success
}

// Verify reads a diff object and checks its structure and integrity checksum.
// It parses the segment directory one entry at a time and streams the data
// section without retaining either, so it runs in bounded memory regardless of
// the object's (or a malicious header's) claimed size.
func Verify(r io.Reader) error {
	h := sha256.New()
	tr := io.TeeReader(r, h)

	fixed := make([]byte, headerFixedSize)
	if _, err := io.ReadFull(tr, fixed); err != nil {
		return err
	}
	targetSize, segCount, hasFromHash, err := parseFixedHeader(fixed)
	if err != nil {
		return err
	}
	if hasFromHash {
		if _, err := io.CopyN(io.Discard, tr, fromHashSize); err != nil {
			return err
		}
	}

	var prevEnd, dataLen uint64
	var entry [dirEntrySize]byte
	for i := uint64(0); i < segCount; i++ {
		if _, err := io.ReadFull(tr, entry[:]); err != nil {
			return err
		}
		s := decodeSeg(entry[:])
		if err := validateEntry(i, s.offset, s.length, s.typ, s.dataOff, &prevEnd, &dataLen, targetSize); err != nil {
			return err
		}
	}
	if _, err := io.CopyN(io.Discard, tr, int64(dataLen)); err != nil {
		if err == io.EOF {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	sum := make([]byte, checksumSize)
	if _, err := io.ReadFull(r, sum); err != nil {
		return err
	}
	if !bytes.Equal(sum, h.Sum(nil)) {
		return errChecksum
	}
	return expectEOF(r)
}

// readExact reads exactly n bytes, allocating only as bytes actually arrive so
// a corrupt length field cannot trigger a huge preallocation or panic.
func readExact(r io.Reader, n uint64) ([]byte, error) {
	if n == 0 {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(r, int64(n)))
	if err != nil {
		return nil, err
	}
	if uint64(len(b)) != n {
		return nil, io.ErrUnexpectedEOF
	}
	return b, nil
}

func hashString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return hex.EncodeToString(b)
}

func decodeFromHash(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("cowdiff: from_hash is not valid hex: %w", err)
	}
	if len(b) != fromHashSize {
		return nil, fmt.Errorf("cowdiff: from_hash must decode to %d bytes, got %d", fromHashSize, len(b))
	}
	return b, nil
}
