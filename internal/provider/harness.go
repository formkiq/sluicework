package provider

// Coding clients authenticate only to this gateway. Their keys never grant
// console/worker access and neither prompts nor tool output enter the audit log.
import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"sluicework/internal/state"
)

var ErrHarnessAuth = errors.New("coding client credential is invalid, expired or revoked")
var ErrHarnessBusy = errors.New("local inference capacity is busy; retry shortly")
var ErrHarnessDisabled = errors.New("coding gateway is paused")

const DefaultHarnessTimeoutSeconds = 30 * 60
const MaxHarnessTimeoutSeconds = DefaultHarnessTimeoutSeconds

const HarnessTimeoutMessage = "Local inference exceeded this coding client's request timeout. Try smaller input/output and review the client timeout (up to 30 minutes) in AI connections."

type HarnessPolicy struct {
	Enabled       bool `json:"enabled"`
	MaxConcurrent int  `json:"maxConcurrent"`
}
type HarnessClientInput struct {
	Name            string   `json:"name"`
	ConnectionID    string   `json:"connectionId"`
	Models          []string `json:"models"`
	ContextLength   int      `json:"contextLength"`
	MaxOutputTokens int      `json:"maxOutputTokens"`
	MaxConcurrent   int      `json:"maxConcurrent"`
	TimeoutSeconds  int      `json:"timeoutSeconds"`
	LifetimeDays    int      `json:"lifetimeDays"`
}
type HarnessClient struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	ConnectionID    string   `json:"connectionId"`
	Models          []string `json:"models"`
	ContextLength   int      `json:"contextLength"`
	MaxOutputTokens int      `json:"maxOutputTokens"`
	MaxConcurrent   int      `json:"maxConcurrent"`
	TimeoutSeconds  int      `json:"timeoutSeconds"`
	Created         string   `json:"created"`
	Expires         string   `json:"expires"`
	Revoked         bool     `json:"revoked"`
}
type HarnessCall struct {
	ID           string `json:"id"`
	ClientID     string `json:"clientId"`
	ClientName   string `json:"clientName"`
	ConnectionID string `json:"connectionId"`
	Model        string `json:"model"`
	Started      string `json:"started"`
	DurationMS   int64  `json:"durationMs"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
	InputTokens  *int   `json:"inputTokens,omitempty"`
	OutputTokens *int   `json:"outputTokens,omitempty"`
}

const harnessSchema = `
CREATE TABLE IF NOT EXISTS harness_policy(id INTEGER PRIMARY KEY CHECK(id=1),enabled INTEGER NOT NULL,max_concurrent INTEGER NOT NULL);
INSERT OR IGNORE INTO harness_policy VALUES(1,0,1);
CREATE TABLE IF NOT EXISTS harness_clients(
 id TEXT PRIMARY KEY,name TEXT NOT NULL,connection_id TEXT NOT NULL,models TEXT NOT NULL,
 context_length INTEGER NOT NULL,max_output_tokens INTEGER NOT NULL,max_concurrent INTEGER NOT NULL,
 timeout_seconds INTEGER NOT NULL,created TEXT NOT NULL,expires TEXT NOT NULL,revoked INTEGER NOT NULL DEFAULT 0,
 digest BLOB NOT NULL UNIQUE);
CREATE TABLE IF NOT EXISTS harness_calls(id TEXT PRIMARY KEY,client_id TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS harness_calls_client ON harness_calls(client_id);
`

func (s *Service) HarnessPolicy(ctx context.Context) (HarnessPolicy, error) {
	var p HarnessPolicy
	e := s.db.QueryRowContext(ctx, `SELECT enabled,max_concurrent FROM harness_policy WHERE id=1`).Scan(&p.Enabled, &p.MaxConcurrent)
	return p, e
}
func (s *Service) SetHarnessPolicy(ctx context.Context, actor string, p HarnessPolicy) error {
	if p.MaxConcurrent < 1 || p.MaxConcurrent > 4 {
		return fmt.Errorf("%w: choose one to four shared local inference slots", ErrInvalid)
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `UPDATE harness_policy SET enabled=?,max_concurrent=? WHERE id=1`, p.Enabled, p.MaxConcurrent); e != nil {
		return e
	}
	if e = state.AuditTx(tx, actor, "harness.policy", fmt.Sprintf("enabled=%t slots=%d", p.Enabled, p.MaxConcurrent)); e != nil {
		return e
	}
	return tx.Commit()
}

const clientColumns = `id,name,connection_id,models,context_length,max_output_tokens,max_concurrent,timeout_seconds,created,expires,revoked`

func scanHarnessClient(row interface{ Scan(...any) error }) (HarnessClient, error) {
	var c HarnessClient
	var models string
	e := row.Scan(&c.ID, &c.Name, &c.ConnectionID, &models, &c.ContextLength, &c.MaxOutputTokens, &c.MaxConcurrent, &c.TimeoutSeconds, &c.Created, &c.Expires, &c.Revoked)
	if e == nil {
		e = json.Unmarshal([]byte(models), &c.Models)
	}
	return c, e
}
func (s *Service) HarnessClients(ctx context.Context) ([]HarnessClient, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT `+clientColumns+` FROM harness_clients ORDER BY created DESC`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []HarnessClient{}
	for rows.Next() {
		c, e := scanHarnessClient(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Service) CreateHarnessClient(ctx context.Context, actor string, in HarnessClientInput) (HarnessClient, string, error) {
	var out HarnessClient
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 128 || strings.ContainsAny(in.Name, "\r\n\x00") || len(in.Models) < 1 || len(in.Models) > 16 || in.ContextLength < 4096 || in.ContextLength > 32768 || in.MaxOutputTokens < 128 || in.MaxOutputTokens > 8192 || in.MaxOutputTokens >= in.ContextLength || in.MaxConcurrent < 1 || in.MaxConcurrent > 4 || in.TimeoutSeconds < 15 || in.TimeoutSeconds > MaxHarnessTimeoutSeconds || in.LifetimeDays < 1 || in.LifetimeDays > 90 {
		return out, "", fmt.Errorf("%w: invalid coding client scope or limits", ErrInvalid)
	}
	conn, e := s.Get(ctx, in.ConnectionID)
	if e != nil {
		return out, "", e
	}
	if conn.Kind != "ollama" || conn.Disabled {
		return out, "", fmt.Errorf("%w: select an enabled local Ollama/Herd connection", ErrInvalid)
	}
	seen := map[string]bool{}
	for _, model := range in.Models {
		if seen[model] {
			return out, "", ErrInvalid
		}
		seen[model] = true
		if e = conn.ValidatePolicy(model, false); e != nil {
			return out, "", e
		}
	}
	raw := make([]byte, 32)
	if _, e = rand.Read(raw); e != nil {
		return out, "", e
	}
	token := "swc_" + base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(token))
	now := time.Now().UTC()
	out = HarnessClient{ID: uuid.NewString(), Name: in.Name, ConnectionID: in.ConnectionID, Models: in.Models, ContextLength: in.ContextLength, MaxOutputTokens: in.MaxOutputTokens, MaxConcurrent: in.MaxConcurrent, TimeoutSeconds: in.TimeoutSeconds, Created: now.Format(time.RFC3339Nano), Expires: now.Add(time.Duration(in.LifetimeDays) * 24 * time.Hour).Format(time.RFC3339Nano)}
	models, _ := json.Marshal(out.Models)
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return out, "", e
	}
	defer tx.Rollback()
	_, e = tx.ExecContext(ctx, `INSERT INTO harness_clients(`+clientColumns+`,digest) VALUES(?,?,?,?,?,?,?,?,?,?,0,?)`, out.ID, out.Name, out.ConnectionID, string(models), out.ContextLength, out.MaxOutputTokens, out.MaxConcurrent, out.TimeoutSeconds, out.Created, out.Expires, digest[:])
	if e != nil {
		return out, "", e
	}
	if e = state.AuditTx(tx, actor, "harness.client.create", out.ID+" "+out.Name); e != nil {
		return out, "", e
	}
	if e = tx.Commit(); e != nil {
		return out, "", e
	}
	return out, token, nil
}
func (s *Service) RevokeHarnessClient(ctx context.Context, actor, id string) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	r, e := tx.ExecContext(ctx, `UPDATE harness_clients SET revoked=1 WHERE id=?`, id)
	if e != nil {
		return e
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	if e = state.AuditTx(tx, actor, "harness.client.revoke", id); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Service) AuthenticateHarness(ctx context.Context, token string) (HarnessClient, error) {
	if len(token) != 47 || !strings.HasPrefix(token, "swc_") {
		return HarnessClient{}, ErrHarnessAuth
	}
	d := sha256.Sum256([]byte(token))
	c, e := scanHarnessClient(s.db.QueryRowContext(ctx, `SELECT `+clientColumns+` FROM harness_clients WHERE digest=?`, d[:]))
	if errors.Is(e, sql.ErrNoRows) {
		return c, ErrHarnessAuth
	}
	if e != nil {
		return c, e
	}
	if c.Revoked {
		return c, ErrHarnessAuth
	}
	expiry, e := time.Parse(time.RFC3339Nano, c.Expires)
	if e != nil || !time.Now().Before(expiry) {
		return c, ErrHarnessAuth
	}
	return c, nil
}
func (s *Service) HarnessModels(ctx context.Context, c HarnessClient) ([]string, error) {
	p, e := s.HarnessPolicy(ctx)
	if e != nil {
		return nil, e
	}
	if !p.Enabled {
		return nil, ErrHarnessDisabled
	}
	conn, e := s.Get(ctx, c.ConnectionID)
	if e != nil {
		return nil, e
	}
	if conn.Kind != "ollama" || conn.Disabled {
		return nil, ErrHarnessDisabled
	}
	models := []string{}
	for _, m := range c.Models {
		if conn.ValidatePolicy(m, false) == nil {
			models = append(models, m)
		}
	}
	return models, nil
}
func (s *Service) HarnessCalls(ctx context.Context) ([]HarnessCall, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT p.attempt_id,c.id,c.name,p.connection_id,p.model,p.started,p.duration_ms,p.status,p.error,p.input_tokens,p.output_tokens FROM provider_calls p JOIN harness_calls h ON h.id=p.attempt_id JOIN harness_clients c ON c.id=h.client_id ORDER BY p.started DESC LIMIT 100`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []HarnessCall{}
	for rows.Next() {
		var c HarnessCall
		if e = rows.Scan(&c.ID, &c.ClientID, &c.ClientName, &c.ConnectionID, &c.Model, &c.Started, &c.DurationMS, &c.Status, &c.Error, &c.InputTokens, &c.OutputTokens); e != nil {
			return nil, e
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// reserveCall is shared by scheduled AI attempts and coding clients. SQLite's
// single write statement makes the capacity check and reservation indivisible.
func (s *Service) reserveCall(ctx context.Context, call Call, clientID string, clientLimit int) error {
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		tx, e := s.db.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		query := `INSERT INTO provider_calls(attempt_id,connection_id,kind,model,started,status) SELECT ?,?,?,?,?,? WHERE NOT EXISTS(SELECT 1 FROM provider_calls WHERE attempt_id=?)`
		args := []any{call.AttemptID, call.ConnectionID, call.Kind, call.Model, call.Started, "running", call.AttemptID}
		if call.Kind == "ollama" {
			query += ` AND (SELECT count(*) FROM provider_calls WHERE kind='ollama' AND status='running') < (SELECT max_concurrent FROM harness_policy WHERE id=1)`
		}
		if clientID != "" {
			query += ` AND (SELECT count(*) FROM provider_calls p JOIN harness_calls h ON h.id=p.attempt_id WHERE h.client_id=? AND p.status='running') < ?`
			args = append(args, clientID, clientLimit)
		}
		r, e := tx.ExecContext(ctx, query, args...)
		if e != nil {
			tx.Rollback()
			return e
		}
		n, _ := r.RowsAffected()
		if n == 1 {
			if clientID != "" {
				if _, e = tx.ExecContext(ctx, `INSERT INTO harness_calls VALUES(?,?)`, call.AttemptID, clientID); e != nil {
					tx.Rollback()
					return e
				}
			}
			return tx.Commit()
		}
		var exists int
		e = tx.QueryRowContext(ctx, `SELECT count(*) FROM provider_calls WHERE attempt_id=?`, call.AttemptID).Scan(&exists)
		tx.Rollback()
		if e != nil {
			return e
		}
		if exists > 0 {
			return ErrConflict
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return ErrHarnessBusy
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func (s *Service) finishHarness(call Call, start time.Time, err error, input, output *int) error {
	status, msg := "succeeded", ""
	if err != nil {
		status = "failed"
		msg = "Local model request failed"
		if errors.Is(err, context.Canceled) {
			status = "cancelled"
			msg = "Request cancelled"
		} else if errors.Is(err, context.DeadlineExceeded) {
			msg = "Request timed out"
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, e := s.db.ExecContext(ctx, `UPDATE provider_calls SET status=?,error=?,duration_ms=?,input_tokens=?,output_tokens=? WHERE attempt_id=?`, status, msg, time.Since(start).Milliseconds(), input, output, call.AttemptID)
	return e
}

// ServeHarnessResponse intentionally supports only stateless Responses requests
// translated into the configured native Ollama API. No client chooses URLs,
// provider credentials, cloud routing, context limits or memory retention.
func (s *Service) ServeHarnessResponse(w http.ResponseWriter, r *http.Request, c HarnessClient, raw []byte) error {
	request, e := parseCodingRequest(raw, c)
	if e != nil {
		return e
	}
	conn, e := s.Get(r.Context(), c.ConnectionID)
	if e != nil {
		return e
	}
	if conn.Kind != "ollama" {
		return ErrInvalid
	}
	if e = conn.ValidatePolicy(request.Model, false); e != nil {
		return e
	}
	permitted := false
	for _, m := range c.Models {
		permitted = permitted || m == request.Model
	}
	if !permitted {
		return fmt.Errorf("%w: model is outside this coding client's scope", ErrInvalid)
	}
	policy, e := s.HarnessPolicy(r.Context())
	if e != nil {
		return e
	}
	if !policy.Enabled {
		return ErrHarnessDisabled
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(c.TimeoutSeconds)*time.Second)
	defer cancel()
	start := time.Now()
	call := Call{AttemptID: "coding-" + uuid.NewString(), ConnectionID: conn.ID, Kind: "ollama", Model: request.Model, Started: start.UTC().Format(time.RFC3339Nano)}
	// Check current policy during admission and streaming, including revocation.
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				var revoked bool
				var expires string
				err := s.db.QueryRowContext(ctx, `SELECT revoked,expires FROM harness_clients WHERE id=?`, c.ID).Scan(&revoked, &expires)
				p, pe := s.HarnessPolicy(ctx)
				current, ce := s.Get(ctx, conn.ID)
				expiry, ee := time.Parse(time.RFC3339Nano, expires)
				if err != nil || pe != nil || ce != nil || ee != nil || revoked || !time.Now().Before(expiry) || !p.Enabled || current.revision != conn.revision {
					cancel()
					return
				}
			}
		}
	}()
	if e = s.reserveCall(ctx, call, c.ID, c.MaxConcurrent); e != nil {
		return e
	}
	// Check the live credential and connection again after admission.
	var revoked bool
	var expiry string
	e = s.db.QueryRowContext(ctx, "SELECT revoked,expires FROM harness_clients WHERE id=?", c.ID).Scan(&revoked, &expiry)
	current, ce := s.Get(ctx, conn.ID)
	policy, pe := s.HarnessPolicy(ctx)
	expires, ee := time.Parse(time.RFC3339Nano, expiry)
	if e != nil || ce != nil || pe != nil || ee != nil || revoked || !time.Now().Before(expires) || !policy.Enabled || current.revision != conn.revision {
		e = ErrHarnessAuth
	}
	if e == nil {
		e = ctx.Err()
	}
	if e != nil {
		_ = s.finishHarness(call, start, e, nil, nil)
		return e
	}
	var input, output *int
	e = s.runCodingResponse(ctx, w, request, conn, c, &input, &output)
	if ctx.Err() != nil {
		e = ctx.Err()
	}
	if saved := s.finishHarness(call, start, e, input, output); saved != nil {
		return saved
	}
	return e
}

// SetHarnessClientTimeout changes subsequent requests without rotating the key
// or changing model permissions, expiry or an already running request.
func (s *Service) SetHarnessClientTimeout(ctx context.Context, actor, id string, seconds int) error {
	if seconds < 15 || seconds > MaxHarnessTimeoutSeconds {
		return fmt.Errorf("%w: choose a timeout between 15 and %d seconds", ErrInvalid, MaxHarnessTimeoutSeconds)
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	result, e := tx.ExecContext(ctx, "UPDATE harness_clients SET timeout_seconds=? WHERE id=? AND revoked=0", seconds, id)
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return ErrNotFound
	}
	if e = state.AuditTx(tx, actor, "harness.client.timeout", fmt.Sprintf("%s seconds=%d", id, seconds)); e != nil {
		return e
	}
	return tx.Commit()
}
