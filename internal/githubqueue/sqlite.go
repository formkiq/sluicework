package githubqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type SQLiteStore struct{ DB *sql.DB }

func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS github_queue_requests (id TEXT PRIMARY KEY, version INTEGER NOT NULL, active INTEGER NOT NULL, body TEXT NOT NULL); CREATE TABLE IF NOT EXISTS github_queue_lease (id INTEGER PRIMARY KEY CHECK(id=1), owner TEXT NOT NULL, expires INTEGER NOT NULL);`)
	if err != nil {
		return nil, err
	}
	return &SQLiteStore{DB: db}, nil
}
func (s *SQLiteStore) Lease(ctx context.Context, owner string, now time.Time, duration time.Duration) (bool, error) {
	result, err := s.DB.ExecContext(ctx, `INSERT INTO github_queue_lease(id,owner,expires) VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET owner=excluded.owner, expires=excluded.expires WHERE github_queue_lease.expires<=?`, owner, now.Add(duration).UnixMilli(), now.UnixMilli())
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
func (s *SQLiteStore) Release(ctx context.Context, owner string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM github_queue_lease WHERE id=1 AND owner=?`, owner)
	return err
}
func (s *SQLiteStore) Active(ctx context.Context) ([]Request, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT body FROM github_queue_requests WHERE active=1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Request{}
	for rows.Next() {
		var body []byte
		var r Request
		if err = rows.Scan(&body); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(body, &r); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
func (s *SQLiteStore) Save(ctx context.Context, r *Request) error {
	next := *r
	next.Version++
	body, err := json.Marshal(next)
	if err != nil {
		return err
	}
	active := r.Active()
	if r.Version == 0 {
		result, err := s.DB.ExecContext(ctx, `INSERT INTO github_queue_requests(id,version,active,body) VALUES(?,?,?,?) ON CONFLICT(id) DO NOTHING`, r.Key(), next.Version, active, body)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrExists
		}
	} else {
		result, err := s.DB.ExecContext(ctx, `UPDATE github_queue_requests SET version=?,active=?,body=? WHERE id=? AND version=?`, next.Version, active, body, r.Key(), r.Version)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrConflict
		}
	}
	r.Version = next.Version
	return nil
}
func (s *SQLiteStore) Get(ctx context.Context, key string) (Request, error) {
	var body []byte
	var r Request
	err := s.DB.QueryRowContext(ctx, `SELECT body FROM github_queue_requests WHERE id=?`, key).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err == nil {
		err = json.Unmarshal(body, &r)
	}
	return r, err
}
