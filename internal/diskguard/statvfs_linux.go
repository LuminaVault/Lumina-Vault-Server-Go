package diskguard

import "syscall"

// statvfs reads what glibc's statvfs(3) reports: f_frsize, falling back to
// f_bsize as glibc does, f_bavail and f_blocks.
func statvfs(path string) (fragment, available, blocks uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, 0, err
	}
	size := st.Frsize
	if size == 0 {
		size = st.Bsize
	}
	return uint64(size), st.Bavail, st.Blocks, nil //nolint:gosec // block sizes are positive
}
