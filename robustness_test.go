package cowdiff

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

type rawEntry struct {
	offset, length uint64
	typ            SegmentType
	dataOff        uint64
}

// buildObject assembles a diff object with a VALID checksum, so tests exercise
// structural validation rather than the checksum path.
func buildObject(targetSize uint64, flags uint32, fromHash []byte, entries []rawEntry, data []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString(magic)
	binary.Write(&buf, binary.LittleEndian, uint32(formatVersion))
	binary.Write(&buf, binary.LittleEndian, flags)
	binary.Write(&buf, binary.LittleEndian, targetSize)
	binary.Write(&buf, binary.LittleEndian, uint64(len(entries)))
	if flags&flagHasFromHash != 0 {
		buf.Write(fromHash)
	}
	for _, e := range entries {
		binary.Write(&buf, binary.LittleEndian, e.offset)
		binary.Write(&buf, binary.LittleEndian, e.length)
		buf.WriteByte(byte(e.typ))
		binary.Write(&buf, binary.LittleEndian, e.dataOff)
	}
	buf.Write(data)
	sum := sha256.Sum256(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

// assertRejected requires every decode/apply entry point to error (not panic)
// on a malformed but checksum-valid object.
func assertRejected(t *testing.T, name string, obj, baseB []byte) {
	t.Helper()
	if err := Verify(bytes.NewReader(obj)); err == nil {
		t.Fatalf("%s: Verify accepted malformed object", name)
	}
	if _, err := ReadHeader(bytes.NewReader(obj)); err == nil {
		t.Fatalf("%s: ReadHeader accepted malformed object", name)
	}
	if err := ApplyTo(bytes.NewReader(baseB), toReaders([][]byte{obj}), &sliceWriterAt{}); err == nil {
		t.Fatalf("%s: ApplyTo accepted malformed object", name)
	}
	if err := Reconstruct(bytes.NewReader(baseB), toReaders([][]byte{obj}), io.Discard); err == nil {
		t.Fatalf("%s: Reconstruct accepted malformed object", name)
	}
	if _, err := Merge(toReaders([][]byte{obj}), io.Discard); err == nil {
		t.Fatalf("%s: Merge accepted malformed object", name)
	}
	// Apply (file path)
	dir := t.TempDir()
	bp := filepath.Join(dir, "base")
	writeFileT(t, bp, baseB)
	bf, _ := os.Open(bp)
	defer bf.Close()
	if err := Apply(bf, toReaders([][]byte{obj}), filepath.Join(dir, "out"), WithReflink(false)); err == nil {
		t.Fatalf("%s: Apply accepted malformed object", name)
	}
}

func TestRejectMalformed(t *testing.T) {
	baseB := make([]byte, 4096)
	rand.New(rand.NewSource(60)).Read(baseB)

	// The exact case codex flagged: unknown type 2 -> Apply would treat as ZERO
	// while Reconstruct/Merge would slice empty data and panic.
	assertRejected(t, "unknown-type",
		buildObject(1, 0, nil, []rawEntry{{offset: 0, length: 1, typ: SegmentType(2), dataOff: 0}}, nil), baseB)

	assertRejected(t, "unknown-flag",
		buildObject(10, flagHasFromHash|0x4, make([]byte, fromHashSize), nil, nil), baseB)

	assertRejected(t, "out-of-bounds",
		buildObject(10, 0, nil, []rawEntry{{offset: 5, length: 100, typ: SegData, dataOff: 0}}, make([]byte, 100)), baseB)

	assertRejected(t, "huge-length-overflow",
		buildObject(10, 0, nil, []rawEntry{{offset: 1, length: ^uint64(0), typ: SegData, dataOff: 0}}, nil), baseB)

	assertRejected(t, "noncanonical-dataoff",
		buildObject(20, 0, nil, []rawEntry{{offset: 0, length: 4, typ: SegData, dataOff: 7}}, make([]byte, 4)), baseB)

	assertRejected(t, "unsorted-overlap",
		buildObject(20, 0, nil, []rawEntry{
			{offset: 0, length: 10, typ: SegData, dataOff: 0},
			{offset: 5, length: 10, typ: SegData, dataOff: 10},
		}, make([]byte, 20)), baseB)

	assertRejected(t, "segcount-exceeds-target",
		buildObject(1, 0, nil, []rawEntry{
			{offset: 0, length: 1, typ: SegData, dataOff: 0},
			{offset: 1, length: 1, typ: SegData, dataOff: 1},
		}, make([]byte, 2)), baseB)
}

func TestTrailingBytes(t *testing.T) {
	baseB := make([]byte, 1024)
	rand.New(rand.NewSource(61)).Read(baseB)
	newB := append([]byte(nil), baseB...)
	newB[100] ^= 0xff
	d := contentDiffBytes(t, baseB, newB, WithBlockSize(256))

	if err := Verify(bytes.NewReader(d)); err != nil {
		t.Fatalf("clean object should verify: %v", err)
	}
	trailing := append(append([]byte(nil), d...), 0x00)
	if err := Verify(bytes.NewReader(trailing)); err == nil {
		t.Fatal("Verify accepted trailing bytes after object")
	}
}

// flakyReaderAt serves data below failAt, then returns a real (non-EOF) error.
type flakyReaderAt struct {
	data   []byte
	failAt int64
	err    error
}

func (f *flakyReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[off:])
	if off+int64(n) > f.failAt {
		if off < f.failAt {
			return int(f.failAt - off), f.err
		}
		return 0, f.err
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestBaseReadErrorPropagates(t *testing.T) {
	baseB := make([]byte, 100*1024)
	rand.New(rand.NewSource(62)).Read(baseB)
	// A no-op diff leaves the whole file as a base region, forcing base reads.
	noop := contentDiffBytes(t, baseB, baseB)

	injected := errors.New("injected I/O error")
	flaky := &flakyReaderAt{data: baseB, failAt: 50 * 1024, err: injected}

	if err := ApplyTo(flaky, toReaders([][]byte{noop}), &sliceWriterAt{}); !errors.Is(err, injected) {
		t.Fatalf("ApplyTo masked base read error: got %v", err)
	}
	flaky2 := &flakyReaderAt{data: baseB, failAt: 50 * 1024, err: injected}
	if err := Reconstruct(flaky2, toReaders([][]byte{noop}), io.Discard); !errors.Is(err, injected) {
		t.Fatalf("Reconstruct masked base read error: got %v", err)
	}
}

func TestApplyRejectsSameFile(t *testing.T) {
	dir := t.TempDir()
	bp := filepath.Join(dir, "data")
	orig := make([]byte, 8192)
	rand.New(rand.NewSource(63)).Read(orig)
	writeFileT(t, bp, orig)
	newB := append([]byte(nil), orig...)
	newB[10] ^= 0xff
	d := contentDiffBytes(t, orig, newB)

	base, _ := os.Open(bp)
	defer base.Close()
	if err := Apply(base, toReaders([][]byte{d}), bp, WithReflink(false)); err == nil {
		t.Fatal("Apply should reject writing over its own base input")
	}
	// base must be intact.
	if got, _ := os.ReadFile(bp); !bytes.Equal(got, orig) {
		t.Fatal("base file was modified despite alias rejection")
	}
}

func TestApplyAtomicOnChecksumFailure(t *testing.T) {
	dir := t.TempDir()
	bp := filepath.Join(dir, "base")
	tp := filepath.Join(dir, "target")
	baseB := make([]byte, 16*1024)
	rand.New(rand.NewSource(64)).Read(baseB)
	writeFileT(t, bp, baseB)

	// Pre-existing target content that must survive a failed apply.
	existing := bytes.Repeat([]byte{0x42}, 12345)
	writeFileT(t, tp, existing)

	newB := append([]byte(nil), baseB...)
	newB[1000] ^= 0xff
	d := contentDiffBytes(t, baseB, newB)
	corrupt := append([]byte(nil), d...)
	corrupt[len(corrupt)-1] ^= 0xff // flip trailer checksum

	base, _ := os.Open(bp)
	defer base.Close()
	if err := Apply(base, toReaders([][]byte{corrupt}), tp, WithReflink(false)); err == nil {
		t.Fatal("Apply should fail on a bad-checksum diff")
	}
	if got, _ := os.ReadFile(tp); !bytes.Equal(got, existing) {
		t.Fatal("target was clobbered by a failed apply (not atomic)")
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if name := e.Name(); name != "base" && name != "target" {
			t.Fatalf("leftover temp file after failed apply: %s", name)
		}
	}
}

// Verify must reject an impossible segment count incrementally, without
// attempting a huge directory allocation (bounded memory).
func TestVerifyBoundedDirectory(t *testing.T) {
	obj := buildObject(1<<40, 0, nil, []rawEntry{{offset: 0, length: 1, typ: SegData, dataOff: 0}}, []byte{0})
	binary.LittleEndian.PutUint64(obj[24:32], 1<<40) // claim ~1e12 segments
	if err := Verify(bytes.NewReader(obj)); err == nil {
		t.Fatal("Verify accepted an object with an impossible segment count")
	}
}

// Verify and ReadHeader must agree on the shared header ceilings (regression
// for a maxSegCount check present in one path but not the other).
func TestVerifyReadHeaderAgree(t *testing.T) {
	hdr := make([]byte, headerFixedSize)
	copy(hdr, magic)
	binary.LittleEndian.PutUint32(hdr[8:12], formatVersion)

	// segCount just over the ceiling: BOTH must reject at the header.
	binary.LittleEndian.PutUint64(hdr[16:24], maxSegCount+1) // targetSize
	binary.LittleEndian.PutUint64(hdr[24:32], maxSegCount+1) // segCount
	if err := Verify(bytes.NewReader(hdr)); err == nil {
		t.Fatal("Verify accepted segCount > maxSegCount")
	}
	if _, err := ReadHeader(bytes.NewReader(hdr)); err == nil {
		t.Fatal("ReadHeader accepted segCount > maxSegCount")
	}

	// At the ceiling both pass the header check and then fail identically for a
	// missing directory -- they must agree either way.
	binary.LittleEndian.PutUint64(hdr[16:24], maxSegCount)
	binary.LittleEndian.PutUint64(hdr[24:32], maxSegCount)
	ev := Verify(bytes.NewReader(hdr))
	_, rv := ReadHeader(bytes.NewReader(hdr))
	if (ev == nil) != (rv == nil) {
		t.Fatalf("Verify/ReadHeader disagree at maxSegCount: verify=%v readHeader=%v", ev, rv)
	}
}

// errAfterReader yields data, then returns err (not io.EOF) on the next read.
type errAfterReader struct {
	data []byte
	pos  int
	err  error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, r.err
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

// A non-EOF read error immediately after the trailer must surface, not be
// swallowed by the EOF probe.
func TestVerifyPropagatesReadError(t *testing.T) {
	baseB := make([]byte, 2048)
	rand.New(rand.NewSource(65)).Read(baseB)
	newB := append([]byte(nil), baseB...)
	newB[500] ^= 0xff
	d := contentDiffBytes(t, baseB, newB, WithBlockSize(256))

	injected := errors.New("injected read error")
	if err := Verify(&errAfterReader{data: d, err: injected}); !errors.Is(err, injected) {
		t.Fatalf("Verify swallowed post-trailer read error: got %v", err)
	}
}

// sliceWriterAt is an in-memory io.WriterAt that grows as needed.
type sliceWriterAt struct{ b []byte }

func (s *sliceWriterAt) WriteAt(p []byte, off int64) (int, error) {
	end := off + int64(len(p))
	if end > int64(len(s.b)) {
		s.b = append(s.b, make([]byte, end-int64(len(s.b)))...)
	}
	copy(s.b[off:], p)
	return len(p), nil
}

// Merge, Apply, ApplyTo, and Reconstruct must all reject an empty diff chain;
// an empty Merge would otherwise emit a valid object that truncates to zero.
func TestNoDiffsRejected(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base")
	if err := os.WriteFile(base, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := Merge(nil, &out); err == nil {
		t.Fatal("Merge(nil) succeeded; would emit a truncate-to-zero object")
	}

	bf, err := os.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	defer bf.Close()
	if err := Apply(bf, nil, filepath.Join(dir, "restored")); err == nil {
		t.Fatal("Apply with no diffs succeeded")
	}
	if err := ApplyTo(bf, nil, &sliceWriterAt{}); err == nil {
		t.Fatal("ApplyTo with no diffs succeeded")
	}
	if err := Reconstruct(bf, nil, io.Discard); err == nil {
		t.Fatal("Reconstruct with no diffs succeeded")
	}
}
