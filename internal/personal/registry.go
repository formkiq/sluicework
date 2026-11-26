package personal

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"sluicework/internal/state"
)

type Registry struct{ db *sql.DB }

func OpenRegistry(db *sql.DB) (*Registry, error) {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS personal_tickets(id TEXT PRIMARY KEY,owner TEXT NOT NULL,digest BLOB NOT NULL,expires INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS personal_runtimes(id TEXT PRIMARY KEY,owner TEXT NOT NULL,name TEXT NOT NULL,public_key BLOB NOT NULL,last_seen INTEGER NOT NULL DEFAULT 0,report TEXT NOT NULL DEFAULT '{}');
 CREATE TABLE IF NOT EXISTS personal_proposals(id TEXT PRIMARY KEY,runtime_id TEXT NOT NULL,owner TEXT NOT NULL,revision INTEGER NOT NULL,manifest TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS personal_nonces(runtime_id TEXT NOT NULL,nonce TEXT NOT NULL,expires INTEGER NOT NULL,PRIMARY KEY(runtime_id,nonce));`)
	return &Registry{db}, err
}

type Ticket struct {
	ID      string    `json:"id"`
	Code    string    `json:"code"`
	Expires time.Time `json:"expires"`
}

func randomValue() (string, error) {
	b := make([]byte, 24)
	_, err := rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b), err
}
func (r *Registry) Ticket(ctx context.Context, owner string) (Ticket, error) {
	id, err := randomValue()
	if err != nil {
		return Ticket{}, err
	}
	code, err := randomValue()
	if err != nil {
		return Ticket{}, err
	}
	sum := sha256.Sum256([]byte(code))
	expires := time.Now().Add(10 * time.Minute)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Ticket{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM personal_tickets WHERE expires<?`, time.Now().Unix()); err != nil {
		return Ticket{}, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM personal_tickets WHERE owner=?`, owner).Scan(&count); err != nil {
		return Ticket{}, err
	}
	if count >= 8 {
		return Ticket{}, fmt.Errorf("%w: too many pending enrollment tickets", ErrInvalid)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO personal_tickets VALUES(?,?,?,?)`, id, owner, sum[:], expires.Unix())
	if err != nil {
		return Ticket{}, err
	}
	if err = state.AuditTx(tx, owner, "prepare personal agent enrollment", id); err != nil {
		return Ticket{}, err
	}
	if err = tx.Commit(); err != nil {
		return Ticket{}, err
	}
	return Ticket{id, code, expires}, nil
}

type Enrollment struct {
	TicketID  string `json:"ticketId"`
	Code      string `json:"code"`
	Name      string `json:"name"`
	PublicKey string `json:"publicKey"`
	Signature string `json:"signature"`
}

func EnrollmentMessage(ticket string, key ed25519.PublicKey) []byte {
	return []byte("sluicework-personal-enrollment-v1\n" + ticket + "\n" + RuntimeID(key))
}
func (r *Registry) Enroll(ctx context.Context, input Enrollment) (Runtime, error) {
	var empty Runtime
	key, err := base64.RawURLEncoding.DecodeString(input.PublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return empty, ErrDenied
	}
	sig, err := base64.RawURLEncoding.DecodeString(input.Signature)
	if err != nil || !ed25519.Verify(key, EnrollmentMessage(input.TicketID, key), sig) {
		return empty, ErrDenied
	}
	if strings.TrimSpace(input.Name) == "" || len(input.Name) > 128 || strings.ContainsAny(input.Name, "\r\n\x00") {
		return empty, ErrInvalid
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	var owner string
	var digest []byte
	var expires int64
	if err = tx.QueryRowContext(ctx, `SELECT owner,digest,expires FROM personal_tickets WHERE id=?`, input.TicketID).Scan(&owner, &digest, &expires); err != nil {
		return empty, ErrDenied
	}
	sum := sha256.Sum256([]byte(input.Code))
	if expires < time.Now().Unix() || subtle.ConstantTimeCompare(sum[:], digest) != 1 {
		return empty, ErrDenied
	}
	id := RuntimeID(key)
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO personal_runtimes(id,owner,name,public_key) VALUES(?,?,?,?)`, id, owner, input.Name, []byte(key)); err != nil {
		return empty, ErrInvalid
	}
	var registeredOwner string
	if err = tx.QueryRowContext(ctx, `SELECT owner FROM personal_runtimes WHERE id=?`, id).Scan(&registeredOwner); err != nil || registeredOwner != owner {
		return empty, ErrDenied
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM personal_tickets WHERE id=?`, input.TicketID); err != nil {
		return empty, err
	}
	if err = state.AuditTx(tx, owner, "enroll personal agent", id); err != nil {
		return empty, err
	}
	if err = tx.Commit(); err != nil {
		return empty, err
	}
	return Runtime{ID: id, Owner: owner, Name: input.Name}, nil
}
func (r *Registry) Save(ctx context.Context, owner, id, runtimeID string, m Manifest) (Proposal, error) {
	if err := m.Validate(time.Now()); err != nil {
		return Proposal{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Proposal{}, err
	}
	defer tx.Rollback()
	var known string
	if err = tx.QueryRowContext(ctx, `SELECT owner FROM personal_runtimes WHERE id=?`, runtimeID).Scan(&known); err != nil || known != owner {
		return Proposal{}, ErrDenied
	}
	rev := 1
	if id == "" {
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM personal_proposals WHERE runtime_id=?`, runtimeID).Scan(&count); err != nil {
			return Proposal{}, err
		}
		if count >= 100 {
			return Proposal{}, fmt.Errorf("%w: maximum 100 personal task proposals", ErrInvalid)
		}
		id, err = randomValue()
		if err != nil {
			return Proposal{}, err
		}
	} else {
		var priorOwner, priorRuntime string
		if err = tx.QueryRowContext(ctx, `SELECT owner,runtime_id,revision FROM personal_proposals WHERE id=?`, id).Scan(&priorOwner, &priorRuntime, &rev); err != nil || priorOwner != owner || priorRuntime != runtimeID {
			return Proposal{}, ErrDenied
		}
		rev++
	}
	data, _ := json.Marshal(m)
	var total int
	if err = tx.QueryRowContext(ctx, `SELECT coalesce(sum(length(CAST(manifest AS BLOB))),0) FROM personal_proposals WHERE runtime_id=? AND id<>?`, runtimeID, id).Scan(&total); err != nil {
		return Proposal{}, err
	}
	if total+len(data) > 1<<20 {
		return Proposal{}, fmt.Errorf("%w: personal task proposals for one runtime exceed the 1 MiB synchronization budget", ErrInvalid)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO personal_proposals VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET revision=excluded.revision,manifest=excluded.manifest`, id, runtimeID, owner, rev, string(data))
	if err != nil {
		return Proposal{}, err
	}
	if err = state.AuditTx(tx, owner, "save inert personal task proposal", id); err != nil {
		return Proposal{}, err
	}
	if err = tx.Commit(); err != nil {
		return Proposal{}, err
	}
	return Proposal{id, rev, runtimeID, owner, m}, nil
}
func (r *Registry) proposals(ctx context.Context, owner, runtimeID string) ([]Proposal, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,revision,runtime_id,owner,manifest FROM personal_proposals WHERE owner=? AND (?='' OR runtime_id=?) ORDER BY id`, owner, runtimeID, runtimeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Proposal{}
	for rows.Next() {
		var p Proposal
		var raw string
		if err = rows.Scan(&p.ID, &p.Revision, &p.RuntimeID, &p.Owner, &raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &p.Manifest); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (r *Registry) Proposals(ctx context.Context, owner string) ([]Proposal, error) {
	return r.proposals(ctx, owner, "")
}
func (r *Registry) Runtimes(ctx context.Context, owner string) ([]Runtime, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,owner,name,last_seen,report FROM personal_runtimes WHERE owner=? ORDER BY name`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Runtime{}
	for rows.Next() {
		var v Runtime
		var seen int64
		var report string
		if err = rows.Scan(&v.ID, &v.Owner, &v.Name, &seen, &report); err != nil {
			return nil, err
		}
		if seen != 0 {
			v.LastSeen = time.Unix(seen, 0).UTC()
		}
		_ = json.Unmarshal([]byte(report), &v.Report)
		out = append(out, v)
	}
	return out, rows.Err()
}
func (r *Registry) Sync(ctx context.Context, e Envelope) ([]Proposal, error) {
	var key []byte
	var owner string
	if err := r.db.QueryRowContext(ctx, `SELECT owner,public_key FROM personal_runtimes WHERE id=?`, e.RuntimeID).Scan(&owner, &key); err != nil || len(key) != 32 || !e.Verify(key, time.Now()) {
		return nil, ErrDenied
	}
	var report Report
	decoder := json.NewDecoder(bytes.NewReader(e.Payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&report) != nil || decoder.Decode(&struct{}{}) != io.EOF || len(report.Models) > 128 || len(report.Approvals) > 100 || len(report.Runs) > 10 {
		return nil, ErrInvalid
	}
	// Nested unknown fields are rejected too; OAuth tokens have no place in this contract.
	data, _ := json.Marshal(report)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM personal_nonces WHERE expires<?`, time.Now().Unix()); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO personal_nonces VALUES(?,?,?)`, e.RuntimeID, e.Nonce, time.Now().Add(5*time.Minute).Unix()); err != nil {
		return nil, ErrDenied
	}
	if _, err = tx.ExecContext(ctx, `UPDATE personal_runtimes SET last_seen=?,report=? WHERE id=?`, time.Now().Unix(), string(data), e.RuntimeID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return r.proposals(ctx, owner, e.RuntimeID)
}
