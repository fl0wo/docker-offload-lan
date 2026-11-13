//go:build linux || darwin

package capacity

import "golang.org/x/sys/unix"

// StatfsFree returns the bytes available to unprivileged users on the
// filesystem holding path (statfs f_bavail * f_bsize, like `df`).
func StatfsFree(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil //nolint:gosec // block counts fit int64 on real disks
}
