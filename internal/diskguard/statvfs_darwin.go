package diskguard

import "syscall"

// statvfs is for local development; production runs statvfs_linux.go.
func statvfs(path string) (fragment, available, blocks uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, 0, err
	}
	return uint64(st.Bsize), st.Bavail, st.Blocks, nil
}
