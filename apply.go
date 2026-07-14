package cowdiff

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// zeroChunk is a read-only source of zeros for hole-filling fallbacks.
var zeroChunk = make([]byte, 1<<20)

type applyConfig struct {
	reflink *bool
}

// ApplyOption configures Apply.
type ApplyOption func(*applyConfig)

// WithReflink forces reflink-accelerated apply on or off (default: auto-detect).
func WithReflink(enabled bool) ApplyOption {
	return func(c *applyConfig) { c.reflink = &enabled }
}

// Apply reconstructs targetPath = base + diffs applied in order (oldest first).
// It materializes and patches a temporary sibling (reflink-cloning base on a
// CoW filesystem, else copying), verifies every diff's checksum, then atomically
// renames it over targetPath. On any error the target is left untouched and the
// temporary is removed. Apply refuses to run if targetPath is the same file as
// base.
func Apply(base *os.File, diffs []io.Reader, targetPath string, opts ...ApplyOption) error {
	var cfg applyConfig
	for _, o := range opts {
		o(&cfg)
	}
	if len(diffs) == 0 {
		return fmt.Errorf("cowdiff: no diffs")
	}
	if err := checkNotSameFile(base, targetPath); err != nil {
		return err
	}
	st, err := base.Stat()
	if err != nil {
		return err
	}
	return atomicWriteFile(targetPath, st.Mode().Perm(), func(out *os.File) error {
		if err := materializeInto(out, base, st, cfg.reflink); err != nil {
			return err
		}
		for _, d := range diffs {
			if err := applyOne(d, out); err != nil {
				return err
			}
		}
		return nil
	})
}

// atomicWriteFile creates a temporary sibling of path, lets fn populate it, then
// fsyncs and atomically renames it over path (and fsyncs the directory). On any
// error the temporary is removed and path is left untouched. The temporary
// keeps mode, so the final file does.
func atomicWriteFile(path string, mode os.FileMode, fn func(f *os.File) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".cowdiff-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			tmp.Close()
			os.Remove(name)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if err := fn(tmp); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	committed = true
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir best-effort fsyncs a directory so a rename survives a crash. Failures
// (e.g. platforms that disallow directory fsync) are non-fatal.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// checkNotSameFile rejects a targetPath that resolves to the same file as base
// (same path, or a hard link), which would otherwise be truncated as an input.
func checkNotSameFile(base *os.File, targetPath string) error {
	bi, err := base.Stat()
	if err != nil {
		return err
	}
	ti, err := os.Stat(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if os.SameFile(bi, ti) {
		return fmt.Errorf("cowdiff: output %q is the same file as the base input", targetPath)
	}
	return nil
}

// ApplyTo reconstructs base + diffs into out over the range [0, final target
// size). It is storage- and filesystem-agnostic (no reflink optimization) and
// does not truncate out; the caller sizes out to the final target size. out is
// assumed to read back as zeros where nothing is written.
func ApplyTo(base io.ReaderAt, diffs []io.Reader, out io.WriterAt) error {
	parsed, err := parseAll(diffs)
	if err != nil {
		return err
	}
	if len(parsed) == 0 {
		return fmt.Errorf("cowdiff: no diffs")
	}
	_, pieces := resolveChain(parsed)
	for _, p := range pieces {
		switch p.typ {
		case pieceData:
			if _, err := out.WriteAt(p.data, int64(p.offset)); err != nil {
				return err
			}
		case pieceZero:
			if err := zeroWriterAt(out, int64(p.offset), int64(p.length)); err != nil {
				return err
			}
		case pieceBase:
			if err := copyBaseRangeAt(base, out, int64(p.offset), int64(p.length)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Reconstruct writes base + diffs applied in order (oldest first) to out
// sequentially over [0, final target size). Unlike ApplyTo it needs no seekable
// sink, so it suits streaming to a pipe or stdout.
func Reconstruct(base io.ReaderAt, diffs []io.Reader, out io.Writer) error {
	parsed, err := parseAll(diffs)
	if err != nil {
		return err
	}
	if len(parsed) == 0 {
		return fmt.Errorf("cowdiff: no diffs")
	}
	return reconstruct(base, parsed, out)
}

// materializeInto fills the (empty) out file with base's contents, by reflink
// clone when possible/requested, else by copy.
func materializeInto(out, base *os.File, st os.FileInfo, wantReflink *bool) error {
	if wantReflink == nil || *wantReflink {
		if err := tryReflinkClone(out, base); err == nil {
			return nil
		} else if wantReflink != nil && *wantReflink {
			return fmt.Errorf("cowdiff: reflink apply requested but clone failed: %w", err)
		}
	}
	_, err := io.Copy(out, io.NewSectionReader(base, 0, st.Size()))
	return err
}

// applyOne patches a single diff into out, streaming its data section and
// verifying its checksum.
func applyOne(r io.Reader, out *os.File) error {
	h := sha256.New()
	tr := io.TeeReader(r, h)
	ts, _, segs, err := readHeader(tr)
	if err != nil {
		return err
	}
	if err := out.Truncate(int64(ts)); err != nil {
		return err
	}
	buf := make([]byte, 1<<20)
	for _, s := range segs {
		if s.length == 0 {
			continue
		}
		if s.typ == SegData {
			if err := copyStreamAt(out, tr, int64(s.offset), int64(s.length), buf); err != nil {
				return err
			}
		} else {
			if err := zeroFileRange(out, int64(s.offset), int64(s.length)); err != nil {
				return err
			}
		}
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

func copyStreamAt(out *os.File, r io.Reader, off, n int64, buf []byte) error {
	for n > 0 {
		c := int64(len(buf))
		if c > n {
			c = n
		}
		if _, err := io.ReadFull(r, buf[:c]); err != nil {
			return err
		}
		if _, err := out.WriteAt(buf[:c], off); err != nil {
			return err
		}
		off += c
		n -= c
	}
	return nil
}

// zeroFileRange punches a hole when the platform/filesystem supports it, else
// writes zeros.
func zeroFileRange(out *os.File, off, n int64) error {
	if err := punchHole(out, off, n); err == nil {
		return nil
	}
	return zeroWriterAt(out, off, n)
}

func zeroWriterAt(out io.WriterAt, off, n int64) error {
	for n > 0 {
		c := int64(len(zeroChunk))
		if c > n {
			c = n
		}
		if _, err := out.WriteAt(zeroChunk[:c], off); err != nil {
			return err
		}
		off += c
		n -= c
	}
	return nil
}

func copyBaseRangeAt(base io.ReaderAt, out io.WriterAt, off, n int64) error {
	buf := make([]byte, 1<<20)
	for n > 0 {
		c := int64(len(buf))
		if c > n {
			c = n
		}
		m, err := base.ReadAt(buf[:c], off)
		if m > 0 {
			if _, werr := out.WriteAt(buf[:m], off); werr != nil {
				return werr
			}
		}
		if err != nil {
			if err != io.EOF {
				return err // a real read error must not be masked as zeros
			}
			return zeroWriterAt(out, off+int64(m), n-int64(m))
		}
		off += c
		n -= c
	}
	return nil
}

func parseAll(diffs []io.Reader) ([]*parsedDiff, error) {
	parsed := make([]*parsedDiff, 0, len(diffs))
	for _, r := range diffs {
		p, err := parseDiff(r)
		if err != nil {
			return nil, err
		}
		parsed = append(parsed, p)
	}
	return parsed, nil
}
