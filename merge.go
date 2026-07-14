package cowdiff

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"slices"
)

type pieceType int

const (
	pieceBase pieceType = iota // unchanged; comes from base
	pieceData                  // literal bytes from a diff
	pieceZero                  // hole / zeros
)

// piece is one contiguous span of the resolved output over [0, finalSize).
type piece struct {
	offset uint64
	length uint64
	typ    pieceType
	data   []byte // for pieceData
}

// Merge folds an ordered chain of diffs (oldest first) into a single diff with
// the same effect as applying them in sequence; later writes win.
func Merge(diffs []io.Reader, out io.Writer) (*Header, error) {
	parsed, err := parseAll(diffs)
	if err != nil {
		return nil, err
	}
	if len(parsed) == 0 {
		return nil, fmt.Errorf("cowdiff: no diffs")
	}
	finalSize, pieces := resolveChain(parsed)

	segs := make([]outSeg, 0, len(pieces))
	for i := range pieces {
		p := &pieces[i]
		switch p.typ {
		case pieceZero:
			segs = append(segs, outSeg{offset: p.offset, length: p.length, typ: SegZero})
		case pieceData:
			segs = append(segs, outSeg{offset: p.offset, length: p.length, typ: SegData, dataReader: bytes.NewReader(p.data)})
		}
	}

	return writeObject(out, finalSize, parsed[0].fromHash, segs)
}

// resolveChain computes the final size and the fully-covered piece list for a
// chain. For each elementary interval the newest diff that covers it wins;
// intervals covered by no diff resolve to base.
func resolveChain(diffs []*parsedDiff) (uint64, []piece) {
	n := len(diffs)
	var finalSize uint64
	if n > 0 {
		finalSize = diffs[n-1].targetSize
	}
	// suffixMin[i] = min targetSize over diffs[i:]. A write (or the base) at
	// offset X survives to the final image only while X stays in bounds through
	// every later truncation; once some diff truncates to <= X, a later grow
	// re-exposes X as zero, not its old content.
	suffixMin := make([]uint64, n)
	for i := n - 1; i >= 0; i-- {
		suffixMin[i] = diffs[i].targetSize
		if i+1 < n && suffixMin[i+1] < suffixMin[i] {
			suffixMin[i] = suffixMin[i+1]
		}
	}
	minTarget := finalSize
	if n > 0 {
		minTarget = suffixMin[0]
	}
	pts := boundaryPoints(diffs, finalSize)

	var pieces []piece
	for i := 0; i+1 < len(pts); i++ {
		a, b := pts[i], pts[i+1]
		if a >= b {
			continue
		}

		var found *rawSeg
		var owner *parsedDiff
		for di := n - 1; di >= 0; di-- {
			s := segCovering(diffs[di].segs, a)
			if s == nil {
				continue
			}
			// Topmost covering diff. Its write survives only if a is still in
			// bounds through all later truncations; lower diffs are then deader.
			if a < suffixMin[di] {
				found, owner = s, diffs[di]
			}
			break
		}

		p := piece{offset: a, length: b - a}
		switch {
		case found == nil:
			if a < minTarget {
				p.typ = pieceBase
			} else {
				p.typ = pieceZero
			}
		case found.typ == SegZero:
			p.typ = pieceZero
		default:
			p.typ = pieceData
			start := found.dataOff + (a - found.offset)
			p.data = owner.data[start : start+(b-a)]
		}

		if n := len(pieces); n > 0 && pieces[n-1].typ == p.typ && pieces[n-1].offset+pieces[n-1].length == p.offset {
			if p.typ == pieceData {
				pieces[n-1].data = append(pieces[n-1].data, p.data...)
			}
			pieces[n-1].length += p.length
			continue
		}
		if p.typ == pieceData {
			p.data = append([]byte(nil), p.data...)
		}
		pieces = append(pieces, p)
	}
	return finalSize, pieces
}

func boundaryPoints(diffs []*parsedDiff, finalSize uint64) []uint64 {
	set := map[uint64]struct{}{0: {}, finalSize: {}}
	for _, d := range diffs {
		// Every targetSize is a truncation threshold used by resolveChain's
		// suffix-min liveness test, so it must be an interval boundary.
		if d.targetSize < finalSize {
			set[d.targetSize] = struct{}{}
		}
		for _, s := range d.segs {
			if s.offset < finalSize {
				set[s.offset] = struct{}{}
			}
			if end := s.offset + s.length; end < finalSize {
				set[end] = struct{}{}
			}
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// segCovering returns the segment containing off, or nil. segs must be sorted
// by offset and non-overlapping (as produced by this package).
func segCovering(segs []rawSeg, off uint64) *rawSeg {
	lo, hi := 0, len(segs)
	for lo < hi {
		mid := (lo + hi) / 2
		if segs[mid].offset <= off {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	idx := lo - 1
	if idx < 0 {
		return nil
	}
	s := &segs[idx]
	if off < s.offset+s.length {
		return s
	}
	return nil
}

// reconstruct writes base + diffs sequentially to w over [0, finalSize). Used
// for streaming apply to a non-seekable sink such as stdout.
func reconstruct(base io.ReaderAt, diffs []*parsedDiff, w io.Writer) error {
	_, pieces := resolveChain(diffs)
	buf := make([]byte, 1<<20)
	for i := range pieces {
		p := &pieces[i]
		switch p.typ {
		case pieceData:
			if _, err := w.Write(p.data); err != nil {
				return err
			}
		case pieceZero:
			if err := writeZerosSeq(w, int64(p.length)); err != nil {
				return err
			}
		case pieceBase:
			if err := copyBaseSeq(base, w, int64(p.offset), int64(p.length), buf); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeZerosSeq(w io.Writer, n int64) error {
	for n > 0 {
		c := int64(len(zeroChunk))
		if c > n {
			c = n
		}
		if _, err := w.Write(zeroChunk[:c]); err != nil {
			return err
		}
		n -= c
	}
	return nil
}

func copyBaseSeq(base io.ReaderAt, w io.Writer, off, n int64, buf []byte) error {
	for n > 0 {
		c := int64(len(buf))
		if c > n {
			c = n
		}
		m, err := base.ReadAt(buf[:c], off)
		if m > 0 {
			if _, werr := w.Write(buf[:m]); werr != nil {
				return werr
			}
		}
		if err != nil {
			if err != io.EOF {
				return err // a real read error must not be masked as zeros
			}
			return writeZerosSeq(w, n-int64(m))
		}
		off += c
		n -= c
	}
	return nil
}
