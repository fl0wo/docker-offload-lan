package node

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
	_ "modernc.org/sqlite" // pure-Go SQLite driver (ADR-0008)
)

// ErrNotFound is returned by Store.Get for unknown lease ids.
var ErrNotFound = errors.New("lease not found")

// StoreFile is the database file name inside data_dir.
const StoreFile = "node.db"

// migrations are applied in order; PRAGMA user_version records progress.
// Append only: never edit a released migration.
var migrations = []string{
	// 1: initial schema.
	`CREATE TABLE leases (
		id                TEXT PRIMARY KEY,
		owner             TEXT    NOT NULL,
		session           TEXT    NOT NULL,
		project           TEXT    NOT NULL,
		workload          TEXT    NOT NULL,
		est_memory_bytes  INTEGER NOT NULL,
		est_disk_bytes    INTEGER NOT NULL,
		ttl_s             INTEGER NOT NULL,
		pinned            INTEGER NOT NULL,
		client_version    TEXT    NOT NULL,
		state             TEXT    NOT NULL,
		created_at        INTEGER NOT NULL,
		last_heartbeat_at INTEGER NOT NULL,
		expires_at        INTEGER NOT NULL,
		ssh_user          TEXT    NOT NULL,
		labels            TEXT    NOT NULL,
		resources         TEXT    NOT NULL,
		last_error        TEXT    NOT NULL,
		updated_at        INTEGER NOT NULL
	);
	CREATE INDEX leases_state ON leases(state);
	CREATE TABLE kind_ports (
		port     INTEGER PRIMARY KEY,
		lease_id TEXT NOT NULL,
		cluster  TEXT NOT NULL UNIQUE
	);
	CREATE TABLE meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);`,
}

// Store persists leases and kind port allocations in SQLite.
type Store struct {
	db   *sql.DB
	path string
}

// OpenStore opens (creating if needed) the database in dataDir and applies
// pending migrations.
func OpenStore(ctx context.Context, dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("creating data_dir %s: %w (run `sudo dol node init` or fix permissions)", dataDir, err)
	}
	path := filepath.Join(dataDir, StoreFile)
	q := url.Values{}
	for _, p := range []string{"busy_timeout(10000)", "journal_mode(WAL)", "synchronous(NORMAL)", "foreign_keys(1)"} {
		q.Add("_pragma", p)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// One connection: SQLite serializes writers anyway, and this avoids
	// SQLITE_BUSY between our own goroutines.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("lease store %s: %w", path, err)
	}
	return s, nil
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	var ver int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&ver); err != nil {
		return err
	}
	if ver > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this dol (%d): upgrade dol", ver, len(migrations))
	}
	for i := ver; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// SchemaVersion returns the applied migration count.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

func nanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromNanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

const leaseCols = `id, owner, session, project, workload, est_memory_bytes, est_disk_bytes, ttl_s, pinned,
	client_version, state, created_at, last_heartbeat_at, expires_at, ssh_user, labels, resources, last_error`

func leaseArgs(l *v1.Lease) ([]any, error) {
	labels, err := json.Marshal(l.Labels)
	if err != nil {
		return nil, err
	}
	res, err := json.Marshal(l.Resources)
	if err != nil {
		return nil, err
	}
	return []any{l.ID, l.Owner, l.Session, l.Project, string(l.Workload), l.EstMemoryBytes, l.EstDiskBytes,
		l.TTLSeconds, l.Pinned, l.ClientVersion, string(l.State), nanos(l.CreatedAt), nanos(l.LastHeartbeatAt),
		nanos(l.ExpiresAt), l.SSHUser, string(labels), string(res), l.LastError}, nil
}

// Insert stores a new lease.
func (s *Store) Insert(ctx context.Context, l *v1.Lease) error {
	args, err := leaseArgs(l)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO leases (`+leaseCols+`, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, append(args, time.Now().UnixNano())...)
	return err
}

// Update overwrites every mutable column of an existing lease.
func (s *Store) Update(ctx context.Context, l *v1.Lease) error {
	args, err := leaseArgs(l)
	if err != nil {
		return err
	}
	cols := strings.Split(leaseCols, ",")
	set := make([]string, 0, len(cols))
	for _, c := range cols[1:] {
		set = append(set, strings.TrimSpace(c)+" = ?")
	}
	q := `UPDATE leases SET ` + strings.Join(set, ", ") + `, updated_at = ? WHERE id = ?`
	r, err := s.db.ExecContext(ctx, q, append(append(args[1:], time.Now().UnixNano()), l.ID)...)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

type scanner interface{ Scan(...any) error }

func scanLease(sc scanner) (*v1.Lease, error) {
	var l v1.Lease
	var workload, state, labels, res string
	var created, hb, exp int64
	if err := sc.Scan(&l.ID, &l.Owner, &l.Session, &l.Project, &workload, &l.EstMemoryBytes, &l.EstDiskBytes,
		&l.TTLSeconds, &l.Pinned, &l.ClientVersion, &state, &created, &hb, &exp, &l.SSHUser, &labels, &res, &l.LastError); err != nil {
		return nil, err
	}
	l.Workload, l.State = v1.Workload(workload), v1.LeaseState(state)
	l.CreatedAt, l.LastHeartbeatAt, l.ExpiresAt = fromNanos(created), fromNanos(hb), fromNanos(exp)
	if err := json.Unmarshal([]byte(labels), &l.Labels); err != nil {
		return nil, fmt.Errorf("lease %s labels: %w", l.ID, err)
	}
	if err := json.Unmarshal([]byte(res), &l.Resources); err != nil {
		return nil, fmt.Errorf("lease %s resources: %w", l.ID, err)
	}
	normalizeResources(&l.Resources)
	return &l, nil
}

// normalizeResources turns nil slices into empty ones so JSON shows [].
func normalizeResources(r *v1.LeaseResources) {
	if r.KindClusters == nil {
		r.KindClusters = []v1.KindCluster{}
	}
	if r.ComposeProjects == nil {
		r.ComposeProjects = []string{}
	}
	if r.SyncDirs == nil {
		r.SyncDirs = []string{}
	}
}

// Get returns one lease or ErrNotFound.
func (s *Store) Get(ctx context.Context, id string) (*v1.Lease, error) {
	l, err := scanLease(s.db.QueryRowContext(ctx, `SELECT `+leaseCols+` FROM leases WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return l, err
}

// List returns leases filtered by owner (if non-empty) and states (if any),
// oldest first.
func (s *Store) List(ctx context.Context, owner string, states ...v1.LeaseState) ([]v1.Lease, error) {
	q := `SELECT ` + leaseCols + ` FROM leases WHERE 1=1`
	var args []any
	if owner != "" {
		q += ` AND owner = ?`
		args = append(args, owner)
	}
	if len(states) > 0 {
		q += ` AND state IN (?` + strings.Repeat(",?", len(states)-1) + `)`
		for _, st := range states {
			args = append(args, string(st))
		}
	}
	q += ` ORDER BY created_at, id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []v1.Lease{}
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *l)
	}
	return out, rows.Err()
}

// Committed returns the summed memory estimate and count of active leases,
// optionally excluding one lease (for resize).
func (s *Store) Committed(ctx context.Context, excludeID string) (bytes int64, count int, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(est_memory_bytes), 0), COUNT(*) FROM leases WHERE state = ? AND id != ?`,
		string(v1.StateActive), excludeID).Scan(&bytes, &count)
	return
}

// UsedPorts returns every allocated kind API server port.
func (s *Store) UsedPorts(ctx context.Context) (map[int]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT port FROM kind_ports`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	used := map[int]bool{}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		used[p] = true
	}
	return used, rows.Err()
}

// AddPort records a port allocation for a cluster.
func (s *Store) AddPort(ctx context.Context, port int, leaseID, cluster string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kind_ports (port, lease_id, cluster) VALUES (?, ?, ?)`, port, leaseID, cluster)
	return err
}

// FreePort releases the allocation of a cluster (no-op if none).
func (s *Store) FreePort(ctx context.Context, cluster string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM kind_ports WHERE cluster = ?`, cluster)
	return err
}

// GetMeta reads a key from the meta table ("" if missing).
func (s *Store) GetMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetMeta writes a key in the meta table.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
