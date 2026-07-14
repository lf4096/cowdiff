//go:build linux

package cowdiff

import (
	"encoding/binary"
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// native is the host byte order used for the FIEMAP ioctl struct.
var native = binary.NativeEndian

// FS_IOC_FIEMAP = _IOWR('f', 11, struct fiemap); struct fiemap is 32 bytes.
const (
	fsIOCFiemap      = 0xC020660B
	fiemapFlagSync   = 0x00000001
	fiemapExtentLast = 0x00000001
	// Flags that make a physical offset unreliable for the "same physical =
	// unchanged" test; such ranges are treated as changed (read from new).
	// NOT_ALIGNED / DATA_INLINE / DATA_TAIL cover inline/tail-packed layouts
	// (e.g. Btrfs inline extents reported with physical offset 0), where equal
	// physical offsets do not imply identical bytes.
	fiemapExtentUnknown       = 0x00000002
	fiemapExtentDelalloc      = 0x00000004
	fiemapExtentEncoded       = 0x00000008
	fiemapExtentDataEncrypted = 0x00000080
	fiemapExtentNotAligned    = 0x00000100
	fiemapExtentDataInline    = 0x00000200
	fiemapExtentDataTail      = 0x00000400
	fiemapStructSize          = 32
	fiemapExtentStructSize    = 56
	fiemapExtentBatch         = 512
)

type extent struct {
	logical  uint64
	physical uint64
	length   uint64
	flags    uint32
}

func fiemapExtents(f *os.File) ([]extent, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := uint64(st.Size())
	if size == 0 {
		return nil, nil
	}

	buf := make([]byte, fiemapStructSize+fiemapExtentBatch*fiemapExtentStructSize)
	var out []extent
	for start := uint64(0); start < size; {
		native.PutUint64(buf[0:8], start)               // fm_start
		native.PutUint64(buf[8:16], size-start)         // fm_length
		native.PutUint32(buf[16:20], fiemapFlagSync)    // fm_flags
		native.PutUint32(buf[20:24], 0)                 // fm_mapped_extents
		native.PutUint32(buf[24:28], fiemapExtentBatch) // fm_extent_count
		native.PutUint32(buf[28:32], 0)                 // fm_reserved

		_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), uintptr(fsIOCFiemap), uintptr(unsafe.Pointer(&buf[0])))
		if errno != 0 {
			return nil, os.NewSyscallError("ioctl(FS_IOC_FIEMAP)", errno)
		}

		mapped := native.Uint32(buf[20:24])
		if mapped == 0 {
			break
		}
		var last extent
		for i := uint32(0); i < mapped; i++ {
			e := buf[fiemapStructSize+int(i)*fiemapExtentStructSize:]
			last = extent{
				logical:  native.Uint64(e[0:8]),
				physical: native.Uint64(e[8:16]),
				length:   native.Uint64(e[16:24]),
				flags:    native.Uint32(e[40:44]),
			}
			out = append(out, last)
		}
		if last.flags&fiemapExtentLast != 0 {
			break
		}
		start = last.logical + last.length
	}
	return out, nil
}

// reflinkSegments computes changed ranges by comparing physical extents, then
// reads only those ranges from newFile.
func reflinkSegments(base, newFile *os.File) (uint64, []outSeg, error) {
	bst, err := base.Stat()
	if err != nil {
		return 0, nil, err
	}
	nst, err := newFile.Stat()
	if err != nil {
		return 0, nil, err
	}
	// Physical extent offsets are only comparable within one device; on
	// different filesystems equal offsets do not imply shared/unchanged bytes.
	if bsys, ok := bst.Sys().(*syscall.Stat_t); ok {
		if nsys, ok := nst.Sys().(*syscall.Stat_t); ok && bsys.Dev != nsys.Dev {
			return 0, nil, fmt.Errorf("cowdiff: reflink mode requires base and new on the same filesystem")
		}
	}
	newSize := uint64(nst.Size())

	baseExts, err := fiemapExtents(base)
	if err != nil {
		return 0, nil, err
	}
	newExts, err := fiemapExtents(newFile)
	if err != nil {
		return 0, nil, err
	}

	var segs []outSeg
	for _, r := range changedRanges(baseExts, newExts, newSize) {
		typ := SegData
		if r.newHole {
			typ = SegZero
		}
		segs = appendCoalesced(segs, newFile, int64(r.offset), int64(r.length), typ)
	}
	return newSize, segs, nil
}

type crange struct {
	offset  uint64
	length  uint64
	newHole bool
}

func changedRanges(baseExts, newExts []extent, newSize uint64) []crange {
	bs := newStepper(baseExts)
	ns := newStepper(newExts)
	pts := boundarySet(baseExts, newExts, newSize)

	var out []crange
	for i := 0; i+1 < len(pts); i++ {
		a, b := pts[i], pts[i+1]
		if a >= newSize {
			break
		}
		if b > newSize {
			b = newSize
		}
		if a >= b {
			continue
		}
		changed, newHole := classify(bs.at(a), ns.at(a))
		if !changed {
			continue
		}
		if n := len(out); n > 0 && out[n-1].newHole == newHole && out[n-1].offset+out[n-1].length == a {
			out[n-1].length += b - a
		} else {
			out = append(out, crange{offset: a, length: b - a, newHole: newHole})
		}
	}
	return out
}

type physAt struct {
	hole     bool
	phys     uint64
	reliable bool
}

func classify(bp, np physAt) (changed, newHole bool) {
	if np.hole {
		if bp.hole {
			return false, false
		}
		return true, true
	}
	if !np.reliable || bp.hole || !bp.reliable || np.phys != bp.phys {
		return true, false
	}
	return false, false
}

type stepper struct{ exts []extent }

func newStepper(exts []extent) stepper {
	s := append([]extent(nil), exts...)
	sort.Slice(s, func(i, j int) bool { return s[i].logical < s[j].logical })
	return stepper{s}
}

func (s stepper) at(off uint64) physAt {
	lo, hi := 0, len(s.exts)
	for lo < hi {
		mid := (lo + hi) / 2
		if s.exts[mid].logical <= off {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	idx := lo - 1
	if idx < 0 {
		return physAt{hole: true}
	}
	e := s.exts[idx]
	if off >= e.logical+e.length {
		return physAt{hole: true}
	}
	return physAt{phys: e.physical + (off - e.logical), reliable: extReliable(e.flags)}
}

func extReliable(flags uint32) bool {
	const unreliable = fiemapExtentUnknown | fiemapExtentDelalloc | fiemapExtentEncoded |
		fiemapExtentDataEncrypted | fiemapExtentNotAligned | fiemapExtentDataInline | fiemapExtentDataTail
	return flags&unreliable == 0
}

func boundarySet(baseExts, newExts []extent, newSize uint64) []uint64 {
	set := map[uint64]struct{}{0: {}, newSize: {}}
	add := func(v uint64) {
		if v <= newSize {
			set[v] = struct{}{}
		}
	}
	for _, e := range baseExts {
		add(e.logical)
		add(e.logical + e.length)
	}
	for _, e := range newExts {
		add(e.logical)
		add(e.logical + e.length)
	}
	return slices.Sorted(maps.Keys(set))
}

func tryReflinkClone(dst, src *os.File) error {
	return unix.IoctlFileClone(int(dst.Fd()), int(src.Fd()))
}

func punchHole(f *os.File, off, length int64) error {
	return unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, off, length)
}

func checkpoint(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return err
	}
	if di, derr := os.Stat(dstPath); derr == nil && os.SameFile(st, di) {
		return fmt.Errorf("cowdiff: checkpoint output %q is the same file as the source", dstPath)
	}

	// Clone into a temporary sibling and rename over dstPath, so a failed clone
	// or verification never truncates an existing destination, and the final
	// path is never opened with O_TRUNC. verifyShared uses FIEMAP_FLAG_SYNC, so
	// it needs no explicit flush before atomicWriteFile's own sync.
	return atomicWriteFile(dstPath, st.Mode().Perm(), func(tmp *os.File) error {
		if err := unix.IoctlFileClone(int(tmp.Fd()), int(src.Fd())); err != nil {
			return fmt.Errorf("cowdiff: reflink clone failed (need a CoW filesystem: XFS/Btrfs/bcachefs): %w", err)
		}
		return verifyShared(src, tmp)
	})
}

// normalizeExtents merges physically-contiguous neighboring extents so two
// FIEMAP views of the same mapping compare equal even when the kernel reports
// the mapping split at different points (common right after cloning a file
// with freshly-flushed delalloc state).
func normalizeExtents(exts []extent) []extent {
	out := make([]extent, 0, len(exts))
	for _, e := range exts {
		if n := len(out); n > 0 {
			p := &out[n-1]
			if e.logical == p.logical+p.length && e.physical == p.physical+p.length {
				p.length += e.length
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

// verifyShared confirms dst is a reflink clone of src by comparing their extent
// maps through the already-open descriptors (no reopen -> no TOCTOU).
func verifyShared(src, dst *os.File) error {
	compare := func() error {
		se, err := fiemapExtents(src)
		if err != nil {
			return err
		}
		de, err := fiemapExtents(dst)
		if err != nil {
			return err
		}
		if err := extentsCovered(normalizeExtents(se), normalizeExtents(de)); err != nil {
			return fmt.Errorf("cowdiff: clone did not share extents: %w", err)
		}
		return nil
	}
	if err := compare(); err == nil {
		return nil
	}
	// FIEMAP_FLAG_SYNC flushes delalloc as a side effect, so on a source with
	// heavy dirty state the first pass can observe the mapping mid-conversion.
	// The maps settle once the flush completes; a genuinely diverged clone
	// still fails because its physical blocks differ.
	return compare()
}

// extentsCovered checks that every block dst maps is backed by the same
// physical block at the same logical offset in src. The containment is
// one-sided on purpose: right after a clone of a freshly-written file, src
// may own speculative preallocation beyond the data the clone shares, so
// src-only allocation is legitimate, while any dst block that src does not
// map identically means the clone diverged.
func extentsCovered(src, dst []extent) error {
	i := 0
	for _, d := range dst {
		for d.length > 0 {
			for i < len(src) && src[i].logical+src[i].length <= d.logical {
				i++
			}
			if i == len(src) {
				return fmt.Errorf("extent at logical %d not mapped in source", d.logical)
			}
			s := src[i]
			if s.logical > d.logical || s.physical+(d.logical-s.logical) != d.physical {
				return fmt.Errorf("extent at logical %d maps to a different physical block", d.logical)
			}
			covered := s.logical + s.length - d.logical
			if covered >= d.length {
				break
			}
			d.logical += covered
			d.physical += covered
			d.length -= covered
		}
	}
	return nil
}
