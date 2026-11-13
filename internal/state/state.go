// Package state manages the client state directory (ADR-0010):
//
//	$XDG_STATE_HOME/docker-offload-lan/
//	  state.lock              flock for cross-process read-modify-write
//	  leases/<id>.json        one v1.ClientLease per file
//	  sessions/<session>.json one Session per running `dol mcp`
//	  kube/<id>-<cluster>.yaml, compose/<id>-<project>.yaml
//
// Every file is written atomically (temp file + fsync + rename + dir fsync),
// so readers never need the lock: they see either the old or the new file.
// Read-modify-write sequences (Update*) take an in-process mutex plus an
// exclusive flock on state.lock, which makes them safe across the many
// concurrent `dol mcp` processes (one per Claude Code session).
package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"

	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// Sub-directories of the state dir.
const (
	LeasesDir   = "leases"
	SessionsDir = "sessions"
	KubeDir     = "kube"
	ComposeDir  = "compose"
	lockFile    = "state.lock"
)

// LockTimeout bounds how long Update waits for the cross-process lock.
var LockTimeout = 30 * time.Second

// Store is a handle on one state directory. It is safe for concurrent use.
type Store struct {
	dir string
	mu  sync.Mutex
	fl  *flock.Flock
}

// Open creates (if needed) and opens the state dir.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("state: empty state dir")
	}
	for _, sub := range []string{"", LeasesDir, SessionsDir, KubeDir, ComposeDir} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, fmt.Errorf("creating state dir %s: %w (hint: check permissions or set XDG_STATE_HOME)", filepath.Join(dir, sub), err)
		}
	}
	return &Store{dir: dir, fl: flock.New(filepath.Join(dir, lockFile))}, nil
}

// Dir returns the state dir.
func (s *Store) Dir() string { return s.dir }

// Path joins elems below the state dir.
func (s *Store) Path(elem ...string) string {
	return filepath.Join(append([]string{s.dir}, elem...)...)
}

// Contains reports whether p is inside the state dir (after cleaning). Used
// before deleting files whose path came from a state record.
func (s *Store) Contains(p string) bool {
	rel, err := filepath.Rel(s.dir, filepath.Clean(p))
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

// WithLock runs fn while holding the in-process mutex and the cross-process
// flock. fn must not call WithLock (not reentrant).
func (s *Store) WithLock(ctx context.Context, fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	lctx, cancel := context.WithTimeout(ctx, LockTimeout)
	defer cancel()
	ok, err := s.fl.TryLockContext(lctx, 5*time.Millisecond)
	if err != nil || !ok {
		if err == nil {
			err = lctx.Err()
		}
		return fmt.Errorf("locking %s: %w (hint: another dol process may be stuck; check `ps aux | grep 'dol mcp'`)", s.fl.Path(), err)
	}
	defer func() { _ = s.fl.Unlock() }()
	return fn()
}

// WriteFileAtomic writes data to path via a temp file in the same dir,
// fsyncs it, renames it into place and fsyncs the directory.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Chmod(perm); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(b, '\n'), 0o600)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("corrupt state file %s: %w (hint: delete it; dol rewrites state as needed)", path, err)
	}
	return nil
}

// ---- leases ----

func (s *Store) leasePath(id string) (string, error) {
	if !v1.ValidLeaseID(id) {
		return "", v1.Errorf(v1.CodeBadRequest, "lease ids look like l-abcdefghij; list them with dol_list or `dol leases`", "invalid lease id %q", id)
	}
	return s.Path(LeasesDir, id+".json"), nil
}

// PutLease writes a lease record atomically.
func (s *Store) PutLease(l v1.ClientLease) error {
	p, err := s.leasePath(l.ID)
	if err != nil {
		return err
	}
	if l.Labels == nil {
		l.Labels = map[string]string{}
	}
	return writeJSON(p, l)
}

// GetLease reads one lease. A missing lease is a CodeNotFound *v1.Error.
func (s *Store) GetLease(id string) (*v1.ClientLease, error) {
	p, err := s.leasePath(id)
	if err != nil {
		return nil, err
	}
	var l v1.ClientLease
	if err := readJSON(p, &l); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, v1.Errorf(v1.CodeNotFound, "list your leases with dol_list or `dol leases`; acquire a new one with dol_acquire",
				"lease %s is not known on this machine (already released, or created elsewhere)", id)
		}
		return nil, err
	}
	return &l, nil
}

// DeleteLease removes a lease record. Missing records are not an error.
func (s *Store) DeleteLease(id string) error {
	p, err := s.leasePath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ListLeases returns every lease record, sorted by creation time then ID.
// Unreadable files are skipped and reported in the returned error slice.
func (s *Store) ListLeases() ([]v1.ClientLease, []error) {
	ents, err := os.ReadDir(s.Path(LeasesDir))
	if err != nil {
		return nil, []error{err}
	}
	var out []v1.ClientLease
	var errs []error
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".json") || strings.HasPrefix(n, ".") {
			continue
		}
		var l v1.ClientLease
		if err := readJSON(s.Path(LeasesDir, n), &l); err != nil {
			if !errors.Is(err, os.ErrNotExist) { // deleted between ReadDir and read
				errs = append(errs, err)
			}
			continue
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out, errs
}

// UpdateLease performs a locked read-modify-write of one lease. If fn returns
// an error nothing is written.
func (s *Store) UpdateLease(ctx context.Context, id string, fn func(l *v1.ClientLease) error) (*v1.ClientLease, error) {
	var out *v1.ClientLease
	err := s.WithLock(ctx, func() error {
		l, err := s.GetLease(id)
		if err != nil {
			return err
		}
		if err := fn(l); err != nil {
			return err
		}
		out = l
		return s.PutLease(*l)
	})
	return out, err
}

// ---- files under kube/ and compose/ ----

// RemoveLeaseFiles deletes kube/<id>-* and compose/<id>-* and returns the
// removed paths.
func (s *Store) RemoveLeaseFiles(id string) ([]string, error) {
	if !v1.ValidLeaseID(id) {
		return nil, fmt.Errorf("invalid lease id %q", id)
	}
	var removed []string
	var firstErr error
	for _, sub := range []string{KubeDir, ComposeDir} {
		matches, _ := filepath.Glob(s.Path(sub, id+"-*"))
		for _, m := range matches {
			if err := os.Remove(m); err != nil && !errors.Is(err, os.ErrNotExist) {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			removed = append(removed, m)
		}
	}
	return removed, firstErr
}
