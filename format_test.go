package cowdiff

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"testing"
)

func TestFromHash(t *testing.T) {
	rng := rand.New(rand.NewSource(10))
	base := randBytes(rng, 40*1024)
	newB := append([]byte(nil), base...)
	copy(newB[1024:2048], make([]byte, 1024))

	fh := "0011223344556677889900112233445566778899001122334455667788990011"

	// round-trips through the header
	d := contentDiffBytes(t, base, newB, WithFromHash(fh))
	h, err := ReadHeader(bytes.NewReader(d))
	if err != nil {
		t.Fatal(err)
	}
	if h.FromHash != fh {
		t.Fatalf("from_hash = %q, want %q", h.FromHash, fh)
	}

	// empty -> no flag, empty string back
	d0 := contentDiffBytes(t, base, newB)
	h0, _ := ReadHeader(bytes.NewReader(d0))
	if h0.FromHash != "" {
		t.Fatalf("expected empty from_hash, got %q", h0.FromHash)
	}

	// invalid hex and wrong length are rejected at Diff time
	for _, bad := range []string{"zz", "abc", "00112233"} {
		dir := t.TempDir()
		bp := dir + "/b"
		np := dir + "/n"
		writeFileT(t, bp, base)
		writeFileT(t, np, newB)
		bf, _ := openRO(t, bp)
		nf, _ := openRO(t, np)
		var buf bytes.Buffer
		if _, err := Diff(bf, nf, &buf, WithMode(ModeContent), WithFromHash(bad)); err == nil {
			t.Fatalf("expected error for bad from_hash %q", bad)
		}
		bf.Close()
		nf.Close()
	}
}

func TestVerifyCorruption(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	base := randBytes(rng, 64*1024)
	newB := append([]byte(nil), base...)
	rng.Read(newB[10*1024 : 12*1024]) // a DATA change
	fh := "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

	d := contentDiffBytes(t, base, newB, WithFromHash(fh), WithBlockSize(4096))
	if err := Verify(bytes.NewReader(d)); err != nil {
		t.Fatalf("clean object should verify: %v", err)
	}

	// A byte flip anywhere must be rejected (checksum, or an earlier structural
	// error for version/seg_count). Sample every region.
	offsets := []int{
		0,           // magic
		8,           // version
		12,          // flags
		16,          // target_size
		24,          // seg_count
		33,          // inside from_hash (starts at 32)
		len(d) - 1,  // trailer
		len(d) - 40, // data section (trailer is last 32)
	}
	for _, off := range offsets {
		c := append([]byte(nil), d...)
		c[off] ^= 0xff
		if err := Verify(bytes.NewReader(c)); err == nil {
			t.Fatalf("corruption at offset %d not detected", off)
		}
	}

	// Truncated object.
	if err := Verify(bytes.NewReader(d[:len(d)-10])); err == nil {
		t.Fatal("truncated object not detected")
	}
	if err := Verify(bytes.NewReader(d[:5])); err == nil {
		t.Fatal("short object not detected")
	}
}

func TestErrorPaths(t *testing.T) {
	rng := rand.New(rand.NewSource(12))
	base := randBytes(rng, 8*1024)
	newB := randBytes(rng, 8*1024)

	// unknown mode
	dir := t.TempDir()
	bp := dir + "/b"
	np := dir + "/n"
	writeFileT(t, bp, base)
	writeFileT(t, np, newB)
	bf, _ := openRO(t, bp)
	nf, _ := openRO(t, np)
	var buf bytes.Buffer
	if _, err := Diff(bf, nf, &buf, WithMode(Mode(99))); err == nil {
		t.Fatal("expected error for unknown mode")
	}
	bf.Close()
	nf.Close()

	// ApplyTo / Reconstruct with zero diffs
	if err := ApplyTo(bytes.NewReader(base), nil, nil); err == nil {
		t.Fatal("ApplyTo with no diffs should error")
	}
	if err := Reconstruct(bytes.NewReader(base), nil, &bytes.Buffer{}); err == nil {
		t.Fatal("Reconstruct with no diffs should error")
	}

	// bad magic
	if _, err := ReadHeader(bytes.NewReader([]byte("NOTCOWDIFF...."))); err == nil {
		t.Fatal("bad magic should error")
	}

	// unsupported version: craft a header with version 999
	d := contentDiffBytes(t, base, newB)
	bad := append([]byte(nil), d...)
	binary.LittleEndian.PutUint32(bad[8:12], 999)
	if _, err := ReadHeader(bytes.NewReader(bad)); err == nil {
		t.Fatal("unsupported version should error")
	}
}

func TestBlockSizeBoundaries(t *testing.T) {
	rng := rand.New(rand.NewSource(13))
	base := randBytes(rng, 20000)
	newB := append([]byte(nil), base...)
	rng.Read(newB[5000:7000])
	copy(newB[12000:14000], make([]byte, 2000)) // zero region -> ZERO seg

	// Every block size must reconstruct identically.
	for _, bs := range []int{1, 7, 512, 4096, 65536, 1 << 20} {
		d := contentDiffBytes(t, base, newB, WithBlockSize(bs))
		checkWellFormed(t, d, uint64(len(newB)))
		got := applyAll(t, base, [][]byte{d})
		if !bytes.Equal(got, newB) {
			t.Fatalf("blockSize=%d: round-trip mismatch", bs)
		}
	}
}

func TestExactSegmentCount(t *testing.T) {
	// Deterministic pattern with block size 1: base all 0xAA; new has one
	// literal run and one zero run separated by an unchanged gap -> exactly
	// two coalesced segments (one DATA, one ZERO).
	base := bytes.Repeat([]byte{0xAA}, 100)
	newB := append([]byte(nil), base...)
	for i := 10; i < 20; i++ {
		newB[i] = 0x55 // DATA run [10,20)
	}
	for i := 40; i < 50; i++ {
		newB[i] = 0x00 // ZERO run [40,50)
	}
	d := contentDiffBytes(t, base, newB, WithBlockSize(1))
	checkWellFormed(t, d, uint64(len(newB)))
	h, _ := ReadHeader(bytes.NewReader(d))
	if len(h.Segments) != 2 {
		t.Fatalf("want 2 coalesced segments, got %d: %+v", len(h.Segments), h.Segments)
	}
	if h.Segments[0].Type != SegData || h.Segments[0].Offset != 10 || h.Segments[0].Length != 10 {
		t.Fatalf("seg0 = %+v, want DATA [10,20)", h.Segments[0])
	}
	if h.Segments[1].Type != SegZero || h.Segments[1].Offset != 40 || h.Segments[1].Length != 10 {
		t.Fatalf("seg1 = %+v, want ZERO [40,50)", h.Segments[1])
	}
	got := applyAll(t, base, [][]byte{d})
	if !bytes.Equal(got, newB) {
		t.Fatal("round-trip mismatch")
	}
}
