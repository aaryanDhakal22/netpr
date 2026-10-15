// Package store keeps print job history in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Job states. IPP states are mirrored after submission.
const (
	StateQueued     = "queued"
	StatePreparing  = "preparing" // trimming pages, rendering for the printer
	StateSending    = "sending"
	StatePending    = "pending"
	StateProcessing = "processing"
	StateCompleted  = "completed"
	StateCanceled   = "canceled"
	StateAborted    = "aborted"
	StateFailed     = "failed" // netpr-side error (conversion, IPP transport)
	StateDryRun     = "dry-run"
	StateUnknown    = "unknown" // stopped tracking before the printer reported an outcome
)

// Terminal reports whether a job will not change state again.
func Terminal(state string) bool {
	switch state {
	case StateCompleted, StateCanceled, StateAborted, StateFailed, StateDryRun, StateUnknown:
		return true
	}
	return false
}

type Job struct {
	ID        int64
	CreatedAt time.Time
	UpdatedAt time.Time
	User      string
	Filename  string
	Kind      string
	Pages     int // pages sent, after any page-range trim
	Copies    int
	Duplex    bool
	PageRange string
	Sheets    int
	IPPJobID  int
	State     string
	Message   string
}

func (j Job) Terminal() bool { return Terminal(j.State) }

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

var migrations = []string{
	`CREATE TABLE jobs (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		user       TEXT NOT NULL,
		filename   TEXT NOT NULL,
		kind       TEXT NOT NULL,
		pages      INTEGER NOT NULL,
		copies     INTEGER NOT NULL,
		duplex     INTEGER NOT NULL,
		page_range TEXT NOT NULL DEFAULT '',
		sheets     INTEGER NOT NULL,
		ipp_job_id INTEGER NOT NULL DEFAULT 0,
		state      TEXT NOT NULL,
		message    TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX jobs_created_at ON jobs(created_at DESC);`,
	`CREATE TABLE settings (
		key        TEXT PRIMARY KEY,
		value      TEXT NOT NULL,
		updated_at INTEGER NOT NULL,
		updated_by TEXT NOT NULL
	);`,
}

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Create inserts j and sets its ID and timestamps.
func (s *Store) Create(ctx context.Context, j *Job) error {
	now := time.Now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO jobs
		(created_at, updated_at, user, filename, kind, pages, copies, duplex, page_range, sheets, ipp_job_id, state, message)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		now.UnixMilli(), now.UnixMilli(), j.User, j.Filename, j.Kind, j.Pages, j.Copies, j.Duplex, j.PageRange,
		j.Sheets, j.IPPJobID, j.State, j.Message)
	if err != nil {
		return err
	}
	j.ID, err = res.LastInsertId()
	j.CreatedAt, j.UpdatedAt = now, now
	return err
}

// UpdateState records a state transition and, when non-zero, the IPP job id.
func (s *Store) UpdateState(ctx context.Context, id int64, state, message string, ippJobID int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET state = ?, message = ?, updated_at = ?,
		ipp_job_id = CASE WHEN ? != 0 THEN ? ELSE ipp_job_id END WHERE id = ?`,
		state, message, time.Now().UnixMilli(), ippJobID, ippJobID, id)
	return err
}

var ErrNotFound = errors.New("job not found")

func (s *Store) Get(ctx context.Context, id int64) (Job, error) {
	rows, err := s.db.QueryContext(ctx, selectJobs+` WHERE id = ?`, id)
	if err != nil {
		return Job{}, err
	}
	jobs, err := scanJobs(rows)
	if err != nil {
		return Job{}, err
	}
	if len(jobs) == 0 {
		return Job{}, ErrNotFound
	}
	return jobs[0], nil
}

// List returns jobs newest first. beforeID = 0 starts from the newest.
func (s *Store) List(ctx context.Context, limit int, beforeID int64) ([]Job, error) {
	q, args := selectJobs+` ORDER BY id DESC LIMIT ?`, []any{limit}
	if beforeID > 0 {
		q, args = selectJobs+` WHERE id < ? ORDER BY id DESC LIMIT ?`, []any{beforeID, limit}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return scanJobs(rows)
}

// Unfinished returns jobs that were in flight, e.g. when the process last stopped.
func (s *Store) Unfinished(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, selectJobs+` WHERE state IN (?, ?, ?, ?, ?) ORDER BY id`,
		StateQueued, StatePreparing, StateSending, StatePending, StateProcessing)
	if err != nil {
		return nil, err
	}
	return scanJobs(rows)
}

const selectJobs = `SELECT id, created_at, updated_at, user, filename, kind, pages, copies, duplex,
	page_range, sheets, ipp_job_id, state, message FROM jobs`

func scanJobs(rows *sql.Rows) ([]Job, error) {
	defer rows.Close()
	var jobs []Job
	for rows.Next() {
		var j Job
		var created, updated int64
		if err := rows.Scan(&j.ID, &created, &updated, &j.User, &j.Filename, &j.Kind, &j.Pages, &j.Copies,
			&j.Duplex, &j.PageRange, &j.Sheets, &j.IPPJobID, &j.State, &j.Message); err != nil {
			return nil, err
		}
		j.CreatedAt, j.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// Settings returns every saved setting by key.
func (s *Store) Settings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SaveSettings stores set and deletes del in one transaction.
func (s *Store) SaveSettings(ctx context.Context, user string, set map[string]string, del []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	for k, v := range set {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at, updated_by) VALUES (?, ?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at,
			updated_by = excluded.updated_by`, k, v, now, user); err != nil {
			return err
		}
	}
	for _, k := range del {
		if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, k); err != nil {
			return err
		}
	}
	return tx.Commit()
}
