package cowdiff

import (
	"bytes"
	"fmt"
	"io"
	"os"
)

// Mode selects how Diff finds changes.
type Mode int

const (
	ModeReflink Mode = iota // FIEMAP fast path (default)
	ModeContent             // full content comparison (fallback)
)

const defaultBlockSize = 64 * 1024

type diffConfig struct {
	mode        Mode
	blockSize   int
	fromHashHex string
}

// DiffOption configures Diff.
type DiffOption func(*diffConfig)

// WithMode selects the diff mode (default ModeReflink).
func WithMode(m Mode) DiffOption { return func(c *diffConfig) { c.mode = m } }

// WithBlockSize sets the content-mode comparison granularity.
func WithBlockSize(n int) DiffOption {
	return func(c *diffConfig) {
		if n > 0 {
			c.blockSize = n
		}
	}
}

// WithFromHash records a caller-supplied base hash (hex of 32 bytes) in the
// header; it is never computed by the tool.
func WithFromHash(h string) DiffOption { return func(c *diffConfig) { c.fromHashHex = h } }

// Diff writes a diff of newFile relative to base to out. In ModeReflink, base
// and newFile must be on a reflink filesystem and newFile must share reflink
// lineage with base; only changed extents are read. In ModeContent, any two
// readable files work but both are read in full.
func Diff(base, newFile *os.File, out io.Writer, opts ...DiffOption) (*Header, error) {
	cfg := diffConfig{mode: ModeReflink, blockSize: defaultBlockSize}
	for _, o := range opts {
		o(&cfg)
	}
	fromHash, err := decodeFromHash(cfg.fromHashHex)
	if err != nil {
		return nil, err
	}

	var (
		targetSize uint64
		segs       []outSeg
	)
	switch cfg.mode {
	case ModeContent:
		targetSize, segs, err = contentSegments(base, newFile, cfg.blockSize)
	case ModeReflink:
		targetSize, segs, err = reflinkSegments(base, newFile)
	default:
		return nil, fmt.Errorf("cowdiff: unknown mode %d", cfg.mode)
	}
	if err != nil {
		return nil, err
	}
	return writeObject(out, targetSize, fromHash, segs)
}

// appendCoalesced appends a segment, merging it with the previous one when they
// are adjacent and of the same type. For SegData the merged data reader spans
// the whole run in newFile.
func appendCoalesced(segs []outSeg, newFile *os.File, off, length int64, typ SegmentType) []outSeg {
	if n := len(segs); n > 0 {
		last := &segs[n-1]
		if last.typ == typ && int64(last.offset+last.length) == off {
			last.length += uint64(length)
			if typ == SegData {
				last.dataReader = io.NewSectionReader(newFile, int64(last.offset), int64(last.length))
			}
			return segs
		}
	}
	s := outSeg{offset: uint64(off), length: uint64(length), typ: typ}
	if typ == SegData {
		s.dataReader = io.NewSectionReader(newFile, off, length)
	}
	return append(segs, s)
}

// contentSegments diffs newFile against base block by block. Changed blocks
// become SegData, or SegZero when the new block is all zeros and lies within
// the base's size (zero extensions past the base are handled by target_size).
func contentSegments(base, newFile *os.File, blockSize int) (uint64, []outSeg, error) {
	if blockSize <= 0 {
		blockSize = defaultBlockSize
	}
	bst, err := base.Stat()
	if err != nil {
		return 0, nil, err
	}
	nst, err := newFile.Stat()
	if err != nil {
		return 0, nil, err
	}
	baseSize := bst.Size()
	newSize := nst.Size()

	var segs []outSeg
	nbuf := make([]byte, blockSize)
	bbuf := make([]byte, blockSize)

	for off := int64(0); off < newSize; {
		n := int64(blockSize)
		if off+n > newSize {
			n = newSize - off
		}
		nb := nbuf[:n]
		if err := readFullAt(newFile, nb, off); err != nil {
			return 0, nil, err
		}

		changed := true
		if off < baseSize {
			bn := n
			if off+bn > baseSize {
				bn = baseSize - off
			}
			bb := bbuf[:bn]
			if err := readFullAt(base, bb, off); err != nil {
				return 0, nil, err
			}
			changed = bn != n || !bytes.Equal(nb, bb)
		}

		if changed {
			if allZero(nb) {
				if off < baseSize {
					segs = appendCoalesced(segs, newFile, off, n, SegZero)
				}
			} else {
				segs = appendCoalesced(segs, newFile, off, n, SegData)
			}
		}
		off += n
	}
	return uint64(newSize), segs, nil
}

func readFullAt(f *os.File, b []byte, off int64) error {
	n, err := f.ReadAt(b, off)
	if err == io.EOF && n == len(b) {
		return nil
	}
	return err
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
