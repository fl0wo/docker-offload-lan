package node

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// SyncBase returns <sync_root>/<lease id>, the only tree a lease's sync dirs
// may live in (ADR-0011).
func SyncBase(root, leaseID string) string {
	return filepath.Join(filepath.Clean(root), leaseID)
}

// ValidateSyncPath checks that p is equal to or below <root>/<leaseID> after
// cleaning and returns the cleaned path. It does not touch the filesystem.
func ValidateSyncPath(root, leaseID, p string) (string, error) {
	bad := func(format string, a ...any) error {
		return v1.Errorf(v1.CodeBadRequest,
			fmt.Sprintf("sync into %s/ (the node's sync_root must match the client's sync.remote_root)", SyncBase(root, leaseID)),
			format, a...)
	}
	if !v1.ValidLeaseID(leaseID) {
		return "", bad("invalid lease id %q", leaseID)
	}
	if p == "" || !filepath.IsAbs(p) {
		return "", bad("sync path %q must be an absolute path", p)
	}
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == ".." {
			return "", bad("sync path %q must not contain '..'", p)
		}
	}
	c := filepath.Clean(p)
	base := SyncBase(root, leaseID)
	rel, err := filepath.Rel(base, c)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", bad("sync path %q is outside %s", p, base)
	}
	return c, nil
}

// CheckNoSymlinks walks from root down to p (which must be below root) and
// fails if any existing component below root is a symlink: a symlink could
// make RemoveAll escape the lease's tree. Missing components are fine.
func CheckNoSymlinks(root, p string) error {
	root = filepath.Clean(root)
	rel, err := filepath.Rel(root, filepath.Clean(p))
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("%s is not below %s", p, root)
	}
	cur := root
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		if seg == "." || seg == "" {
			continue
		}
		cur = filepath.Join(cur, seg)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return v1.Errorf(v1.CodeBadRequest, "sync directories must be real directories, not symlinks",
				"refusing sync path %s: %s is a symlink", p, cur)
		}
	}
	return nil
}

// RemoveSyncDir deletes a registered sync dir after re-validating it.
// Missing directories are not an error (idempotent teardown).
func RemoveSyncDir(root, leaseID, p string) error {
	c, err := ValidateSyncPath(root, leaseID, p)
	if err != nil {
		return err
	}
	if err := CheckNoSymlinks(root, c); err != nil {
		return err
	}
	if err := os.RemoveAll(c); err != nil {
		return err
	}
	// Remove the per-lease base too once it is empty.
	if base := SyncBase(root, leaseID); c != base {
		if err := os.Remove(base); err != nil && !errors.Is(err, fs.ErrNotExist) && !isNotEmpty(err) {
			return err
		}
	}
	return nil
}

func isNotEmpty(err error) bool {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		s := pe.Err.Error()
		return strings.Contains(s, "not empty") || strings.Contains(s, "exists")
	}
	return false
}
