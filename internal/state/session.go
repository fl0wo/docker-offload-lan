package state

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Session records one running `dol mcp` process.
type Session struct {
	ID string `json:"id"`
	// PID and StartTime identify the process. A session is dead when the PID
	// is gone or now belongs to a process with a different start time (PID
	// reuse).
	PID       int       `json:"pid"`
	StartTime string    `json:"start_time"`
	Owner     string    `json:"owner"`
	Project   string    `json:"project"`
	CreatedAt time.Time `json:"created_at"`
}

var sessionIDRe = regexp.MustCompile(`^s-[a-z2-7]{10}$`)

func (s *Store) sessionPath(id string) (string, error) {
	if !sessionIDRe.MatchString(id) {
		return "", fmt.Errorf("invalid session id %q", id)
	}
	return s.Path(SessionsDir, id+".json"), nil
}

// NewSelfSession describes the current process.
func NewSelfSession(id, owner, project string) (Session, error) {
	pid := os.Getpid()
	st, err := ProcessStartTime(pid)
	if err != nil {
		return Session{}, fmt.Errorf("reading own process start time: %w", err)
	}
	return Session{ID: id, PID: pid, StartTime: st, Owner: owner, Project: project, CreatedAt: time.Now().UTC()}, nil
}

// PutSession registers a session.
func (s *Store) PutSession(sess Session) error {
	p, err := s.sessionPath(sess.ID)
	if err != nil {
		return err
	}
	return writeJSON(p, sess)
}

// RemoveSession deletes a session record (missing is fine).
func (s *Store) RemoveSession(id string) error {
	p, err := s.sessionPath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// GetSession reads a session; ok=false if there is no record.
func (s *Store) GetSession(id string) (Session, bool) {
	p, err := s.sessionPath(id)
	if err != nil {
		return Session{}, false
	}
	var sess Session
	if err := readJSON(p, &sess); err != nil {
		return Session{}, false
	}
	return sess, true
}

// ListSessions returns all session records.
func (s *Store) ListSessions() ([]Session, error) {
	ents, err := os.ReadDir(s.Path(SessionsDir))
	if err != nil {
		return nil, err
	}
	var out []Session
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".json") || strings.HasPrefix(n, ".") {
			continue
		}
		var sess Session
		if readJSON(s.Path(SessionsDir, n), &sess) == nil {
			out = append(out, sess)
		}
	}
	return out, nil
}

// Alive reports whether the session's process still runs (same PID and same
// start time).
func (sess Session) Alive() bool {
	if sess.PID <= 0 {
		return false
	}
	if !pidExists(sess.PID) {
		return false
	}
	st, err := ProcessStartTime(sess.PID)
	if err != nil {
		// Process vanished between the checks, or we cannot read it. If it
		// exists but is unreadable, err on the side of "alive": never tear
		// down another live session's resources by mistake.
		return pidExists(sess.PID)
	}
	return st == sess.StartTime
}

// SessionAlive reports whether session id is registered and alive.
func (s *Store) SessionAlive(id string) bool {
	sess, ok := s.GetSession(id)
	return ok && sess.Alive()
}

// PruneDeadSessions removes records of dead sessions and returns them.
func (s *Store) PruneDeadSessions() ([]Session, error) {
	all, err := s.ListSessions()
	if err != nil {
		return nil, err
	}
	var dead []Session
	for _, sess := range all {
		if !sess.Alive() {
			dead = append(dead, sess)
			_ = s.RemoveSession(sess.ID)
		}
	}
	return dead, nil
}

func pidExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
