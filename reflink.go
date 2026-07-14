package cowdiff

// Checkpoint creates dstPath as a reflink clone of srcPath and verifies the two
// share physical extents; it errors instead of silently producing a full copy.
// The clone is a plain file that serves as a base (baseline) for later diffs.
//
// Reflink cloning requires a copy-on-write filesystem (XFS, Btrfs, bcachefs)
// and is only supported on Linux.
func Checkpoint(srcPath, dstPath string) error {
	return checkpoint(srcPath, dstPath)
}
