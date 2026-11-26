package spot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	db      *sql.DB
	backend string
}

func OpenSQLite(path, backend string) (*SQLiteStore, error) {
	if !namePattern.MatchString(backend) {
		return nil, errors.New("invalid Spot backend")
	}
	db, e := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	if _, e = db.Exec(`CREATE TABLE IF NOT EXISTS spot_fleets(backend TEXT PRIMARY KEY, version INTEGER NOT NULL, body TEXT NOT NULL)`); e != nil {
		db.Close()
		return nil, e
	}
	return &SQLiteStore{db, backend}, nil
}
func (s *SQLiteStore) Close() error { return s.db.Close() }
func (s *SQLiteStore) Load(ctx context.Context) (State, error) {
	state := State{Allocations: map[string]Allocation{}}
	var body string
	var version int64
	e := s.db.QueryRowContext(ctx, "SELECT version,body FROM spot_fleets WHERE backend=?", s.backend).Scan(&version, &body)
	if errors.Is(e, sql.ErrNoRows) {
		return state, nil
	}
	if e != nil {
		return state, e
	}
	if len(body) > 300000 || json.Unmarshal([]byte(body), &state) != nil || state.Version != version || len(state.Allocations) > HardLimit {
		return State{}, errors.New("invalid persisted Spot fleet")
	}
	return state, nil
}
func (s *SQLiteStore) Save(ctx context.Context, state *State) error {
	if len(state.Allocations) > HardLimit {
		return errors.New("Spot fleet exceeds its hard limit")
	}
	next := cloneState(*state)
	next.Version++
	body, e := json.Marshal(next)
	if e != nil {
		return e
	}
	if len(body) > 300000 {
		return errors.New("Spot fleet exceeds its storage limit")
	}
	var r sql.Result
	if state.Version == 0 {
		r, e = s.db.ExecContext(ctx, "INSERT INTO spot_fleets(backend,version,body) VALUES(?,?,?) ON CONFLICT(backend) DO NOTHING", s.backend, next.Version, string(body))
	} else {
		r, e = s.db.ExecContext(ctx, "UPDATE spot_fleets SET version=?,body=? WHERE backend=? AND version=?", next.Version, string(body), s.backend, state.Version)
	}
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return ErrConflict
	}
	state.Version = next.Version
	return nil
}
