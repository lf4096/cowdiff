//go:build !linux

package cowdiff

import (
	"errors"
	"os"
)

// errUnsupportedPlatform is returned by the reflink/FIEMAP entry points on
// non-Linux platforms. Content-mode diff, apply, and merge remain available.
var errUnsupportedPlatform = errors.New("cowdiff: reflink and FIEMAP operations require Linux")

func reflinkSegments(base, newFile *os.File) (uint64, []outSeg, error) {
	return 0, nil, errUnsupportedPlatform
}

func checkpoint(srcPath, dstPath string) error { return errUnsupportedPlatform }

func tryReflinkClone(dst, src *os.File) error { return errUnsupportedPlatform }

func punchHole(f *os.File, off, length int64) error { return errUnsupportedPlatform }
