package state

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const SchemaVersion = 3

const schema = `
CREATE TABLE IF NOT EXISTS state_migrations (version INTEGER PRIMARY KEY);
CREATE TABLE enrollment_tokens (token_hash BLOB PRIMARY KEY, expires INTEGER NOT NULL);
CREATE TABLE machines (
 id TEXT PRIMARY KEY, credential_hash BLOB UNIQUE, info TEXT NOT NULL, labels TEXT NOT NULL DEFAULT '[]',
 slots INTEGER NOT NULL DEFAULT 1, role_limits TEXT NOT NULL DEFAULT '{}', runner TEXT NOT NULL DEFAULT 'null',
 heartbeat INTEGER NOT NULL, draining INTEGER NOT NULL DEFAULT 0, revoked INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE jobs (id TEXT PRIMARY KEY, revision INTEGER NOT NULL);
CREATE TABLE job_revisions (job_id TEXT NOT NULL REFERENCES jobs(id), revision INTEGER NOT NULL, definition TEXT NOT NULL, PRIMARY KEY(job_id,revision));
CREATE TABLE runs (
 id TEXT PRIMARY KEY, job_id TEXT NOT NULL REFERENCES jobs(id), revision INTEGER NOT NULL, inputs TEXT NOT NULL,
 trigger TEXT NOT NULL, schedule_id TEXT NOT NULL, created INTEGER NOT NULL, status TEXT NOT NULL,
 FOREIGN KEY(job_id, revision) REFERENCES job_revisions(job_id,revision)
);
CREATE TABLE attempts (
 id TEXT PRIMARY KEY, run_id TEXT NOT NULL REFERENCES runs(id), number INTEGER NOT NULL, status TEXT NOT NULL,
 machine_id TEXT REFERENCES machines(id), queued INTEGER NOT NULL, started INTEGER, ended INTEGER, lease_until INTEGER,
 cancel_requested INTEGER NOT NULL DEFAULT 0, outcome_unknown INTEGER NOT NULL DEFAULT 0,
 output TEXT NOT NULL DEFAULT '', output_truncated INTEGER NOT NULL DEFAULT 0, logs_truncated INTEGER NOT NULL DEFAULT 0,
 log_bytes INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '', UNIQUE(run_id, number)
);
CREATE INDEX attempts_queue ON attempts(status,queued);
CREATE INDEX attempts_placement ON attempts(machine_id,status);
CREATE INDEX runs_schedule ON runs(schedule_id,status);
CREATE INDEX runs_created ON runs(created);
CREATE TABLE logs (id INTEGER PRIMARY KEY AUTOINCREMENT, attempt_id TEXT NOT NULL REFERENCES attempts(id), at INTEGER NOT NULL, text TEXT NOT NULL, truncated INTEGER NOT NULL DEFAULT 0);
CREATE INDEX logs_attempt ON logs(attempt_id,id);
CREATE TABLE audit (id INTEGER PRIMARY KEY, at INTEGER NOT NULL, actor TEXT NOT NULL, action TEXT NOT NULL, target TEXT NOT NULL);
INSERT INTO state_migrations(version) VALUES(1);
`

func (s *Store) Init() error {
	return transact(s.DB, func(tx *sql.Tx) error {
		if _, err := tx.Exec("CREATE TABLE IF NOT EXISTS state_migrations (version INTEGER PRIMARY KEY)"); err != nil {
			return err
		}
		var version int
		if err := tx.QueryRow("SELECT COALESCE(MAX(version),0) FROM state_migrations").Scan(&version); err != nil {
			return err
		}
		if version > SchemaVersion {
			return fmt.Errorf("unsupported state migration %d", version)
		}
		if version < 1 {
			if _, err := tx.Exec(schema); err != nil {
				return err
			}
		}
		if version < 2 {
			if _, err := tx.Exec(`ALTER TABLE attempts ADD COLUMN automatic INTEGER NOT NULL DEFAULT 0;
INSERT INTO state_migrations(version) VALUES(2)`); err != nil {
				return err
			}
		}
		if version < 3 {
			_, err := tx.Exec(`ALTER TABLE attempts ADD COLUMN log_sequence INTEGER NOT NULL DEFAULT 0;
DROP INDEX runs_created;
CREATE INDEX runs_created ON runs(created,id);
INSERT INTO state_migrations(version) VALUES(3)`)
			return err
		}
		return nil
	})
}

type queryer interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

func transact(db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func AuditTx(tx *sql.Tx, actor, action, target string) error {
	_, err := tx.Exec("INSERT INTO audit(at,actor,action,target) VALUES(?,?,?,?)", time.Now().UnixMilli(), actor, action, target)
	return err
}
func (s *Store) Audit(actor, action, target string) error {
	return transact(s.DB, func(tx *sql.Tx) error { return AuditTx(tx, actor, action, target) })
}

func randomID() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func hash(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }
func encode(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
func missing(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
func invalid(detail string) error { return fmt.Errorf("%w: %s", ErrInvalid, detail) }
func uniqueNames(vals []string) bool {
	seen := make(map[string]bool, len(vals))
	if len(vals) > 64 {
		return false
	}
	for _, v := range vals {
		if len(v) == 0 || len(v) > 128 || strings.TrimSpace(v) != v || seen[v] {
			return false
		}
		seen[v] = true
	}
	return true
}

func (s *Store) leaseTTL() time.Duration {
	if s.LeaseTTL > 0 {
		return s.LeaseTTL
	}
	return 30 * time.Second
}
func (s *Store) offlineAfter() time.Duration {
	if s.OfflineAfter > 0 {
		return s.OfflineAfter
	}
	return 45 * time.Second
}
func (s *Store) logLimit() int {
	if s.LogLimitPolicy != nil {
		return s.LogLimitPolicy()
	}
	if s.LogLimit > 0 {
		return s.LogLimit
	}
	return 1 << 20
}
func fromMillis(t int64) time.Time { return time.UnixMilli(t).UTC() }
