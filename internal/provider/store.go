// Package provider brokers named inference connections on the orchestrator.
package provider

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"sluicework/internal/state"
)

var (
	ErrInvalid  = errors.New("invalid provider configuration")
	ErrNotFound = errors.New("connection not found")
	ErrConflict = errors.New("provider operation conflicts with existing state")
)

type actorKey struct{}

// WithActor attaches the authenticated administrator identity to configuration changes.
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// Config is write-only. Public responses use Connection, which contains no credential fields.
type Config struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	Endpoint string   `json:"endpoint"`
	Models   []string `json:"models"`
	Disabled bool     `json:"disabled"`
	APIKey   string   `json:"api_key,omitempty"`
	LocalOptions
}

// LocalOptions apply only to Ollama generation, not to the runtime's global settings.
type LocalOptions struct {
	ContextLength    int  `json:"contextLength,omitempty"`
	KeepAliveSeconds int  `json:"keepAliveSeconds,omitempty"`
	Thinking         bool `json:"thinking,omitempty"`
}

type Connection struct {
	ID          string   `json:"id"`
	Owner       string   `json:"owner,omitempty"` // Empty for shared API-key/local connections.
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Endpoint    string   `json:"endpoint"`
	Models      []string `json:"models"`
	Disabled    bool     `json:"disabled"`
	Health      string   `json:"health"`
	LastChecked string   `json:"last_checked,omitempty"`
	LastError   string   `json:"last_error,omitempty"`
	LocalOptions
	secretRef string
	revision  int64
}

type Service struct {
	db        *sql.DB
	secretDir string
	// Serialize credential rotation and loading, but release before any provider I/O.
	mu            sync.Mutex
	local, cloud  *http.Client
	installMu     sync.Mutex
	installs      map[string]context.CancelFunc
	installWG     sync.WaitGroup
	closed        bool
	planMu        sync.Mutex
	planAttempts  map[string]*planAttempt
	planCalls     map[string]map[string]context.CancelFunc
	planClosed    bool
	personalOwner string
}

const schema = `
CREATE TABLE IF NOT EXISTS provider_connections (
 id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, kind TEXT NOT NULL, endpoint TEXT NOT NULL,
 models TEXT NOT NULL, disabled INTEGER NOT NULL, secret_ref TEXT NOT NULL,
 revision INTEGER NOT NULL DEFAULT 1, health TEXT NOT NULL DEFAULT 'untested',
 last_checked TEXT NOT NULL DEFAULT '', last_error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS provider_calls (
 attempt_id TEXT PRIMARY KEY, connection_id TEXT NOT NULL, kind TEXT NOT NULL, model TEXT NOT NULL,
 started TEXT NOT NULL, duration_ms INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL,
 error TEXT NOT NULL DEFAULT '', input_tokens INTEGER, output_tokens INTEGER
);
INSERT INTO provider_migrations(version) VALUES(1);`

func Open(db *sql.DB, secretDir string) (*Service, error) {
	return openService(db, secretDir, true, "")
}

// OpenForConfiguration allows local administrator provisioning while the server
// is running. Only server startup may reconcile active calls and downloads.
func OpenForConfiguration(db *sql.DB, secretDir string) (*Service, error) {
	return openService(db, secretDir, false, "")
}

func openService(db *sql.DB, secretDir string, recover bool, personalOwner string) (*Service, error) {
	if err := os.MkdirAll(secretDir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(secretDir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("provider secret directory must be private (0700)")
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`CREATE TABLE IF NOT EXISTS provider_migrations(version INTEGER PRIMARY KEY)`); err != nil {
		return nil, err
	}
	var version int
	if err = tx.QueryRow(`SELECT coalesce(max(version),0) FROM provider_migrations`).Scan(&version); err != nil {
		return nil, err
	}
	switch version {
	case 0:
		if _, err = tx.Exec(schema); err != nil {
			return nil, err
		}
	case 1, 2:
	default:
		return nil, fmt.Errorf("unsupported provider schema version %d", version)
	}
	if version < 2 {
		if _, err = tx.Exec(modelSchema); err != nil {
			return nil, err
		}
	}
	if recover {
		if _, err = tx.Exec(`UPDATE model_installations SET status='interrupted',message='Server restarted. Retry to resume the download.' WHERE status='running'`); err != nil {
			return nil, err
		}
		// There is one orchestrator. No HTTP call survives its restart.
		if _, err = tx.Exec(`UPDATE provider_calls SET status='interrupted',error='orchestrator restarted; provider outcome unknown' WHERE status='running'`); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if _, err = db.Exec(harnessSchema); err != nil {
		return nil, err
	}
	if _, err = db.Exec(chatGPTSchema); err != nil {
		return nil, err
	}
	s := &Service{personalOwner: personalOwner, db: db, secretDir: secretDir, local: newClient(true), cloud: newClient(false), installs: map[string]context.CancelFunc{}}
	if !recover {
		return s, nil
	}
	// Backups deliberately omit credentials. Reflect missing keys as soon as the restored cluster opens.
	ctx := context.Background()
	connections, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range connections {
		if c.Kind == "chatgpt" && personalOwner == "" {
			_, err := db.Exec(`UPDATE provider_connections SET disabled=1,health='error',last_error='Sign in from your private personal agent; shared brokers cannot use subscription credentials' WHERE id=?`, c.ID)
			if err != nil {
				return nil, err
			}
			continue
		}
		if c.Kind == "ollama" || c.Disabled {
			continue
		}
		if _, keyErr := s.key(c.secretRef); keyErr != nil {
			if err := s.recordHealth(ctx, c, keyErr); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

var endpoints = map[string]string{"openai": "https://api.openai.com", "chatgpt": "https://api.openai.com", "anthropic": "https://api.anthropic.com", "gemini": "https://generativelanguage.googleapis.com"}
var modelPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:/-]{0,190}$`)

func normalize(c *Config) error {
	invalid := func(msg string) error { return fmt.Errorf("%w: %s", ErrInvalid, msg) }
	if c.ContextLength < 0 || c.ContextLength > 32768 || (c.ContextLength > 0 && c.ContextLength < 512) || c.KeepAliveSeconds < 0 || c.KeepAliveSeconds > 300 {
		return invalid("local context must be 512–32768 tokens (or 0 for runtime default), and keep-alive 0–300 seconds")
	}
	if c.Kind != "ollama" && (c.ContextLength != 0 || c.KeepAliveSeconds != 0 || c.Thinking) {
		return invalid("local runtime options require an Ollama connection")
	}
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" || len(c.Name) > 128 || strings.ContainsAny(c.Name, "\r\n\x00") {
		return invalid("provide a name of up to 128 bytes")
	}
	if len(c.Models) == 0 || len(c.Models) > 128 {
		return invalid("provide one to 128 allowed models")
	}
	seen := map[string]bool{}
	for _, m := range c.Models {
		if !modelPattern.MatchString(m) || seen[m] || strings.Contains(m, "..") || strings.Contains(m, "//") {
			return invalid("invalid or duplicate model name")
		}
		if c.Kind != "ollama" && strings.Contains(m, "/") {
			return invalid("use the model ID without a URL or resource prefix")
		}
		if c.Kind == "ollama" && isCloudModel(m) {
			return invalid("select a model served by the local runtime")
		}
		seen[m] = true
	}
	if c.Kind == "ollama" {
		u, err := url.Parse(strings.TrimRight(c.Endpoint, "/"))
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
			return invalid("provide a local HTTP(S) base URL without credentials or query parameters")
		}
		host := u.Hostname()
		ip, _ := netip.ParseAddr(host)
		if u.Scheme == "http" && !loopbackHost(host) {
			return invalid("LAN inference endpoints require HTTPS")
		}
		if ip.IsValid() && !isLAN(ip.Unmap()) {
			return invalid("inference endpoint must resolve to a loopback or private LAN address")
		}
		if c.APIKey != "" {
			return invalid("local connections do not accept cloud API keys")
		}
		c.Endpoint = u.String()
	} else {
		ep, ok := endpoints[c.Kind]
		if !ok {
			return invalid("unknown provider kind")
		}
		if c.Endpoint != "" && strings.TrimRight(c.Endpoint, "/") != ep {
			return invalid("cloud connections use the official vendor endpoint")
		}
		c.Endpoint = ep
	}
	if len(c.APIKey) > 8192 || strings.ContainsAny(c.APIKey, "\r\n\x00") || c.APIKey != strings.TrimSpace(c.APIKey) {
		return invalid("invalid API key")
	}
	if c.Kind == "chatgpt" && c.APIKey != "" {
		return invalid("ChatGPT plan connections require Continue with ChatGPT, not pasted credentials")
	}
	return nil
}

func loopbackHost(host string) bool {
	ip, _ := netip.ParseAddr(host)
	return strings.EqualFold(host, "localhost") || ip.Unmap().IsLoopback()
}

func isCloudModel(model string) bool {
	m := strings.ToLower(model)
	return strings.HasSuffix(m, ":cloud") || strings.HasSuffix(m, "-cloud")
}

func id() (string, error) {
	var b [16]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}

// A new random reference on each rotation leaves the previous credential usable until commit.
func (s *Service) writeSecret(key string) (string, error) {
	ref, err := id()
	if err != nil {
		return "", err
	}
	path := filepath.Join(s.secretDir, ref)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", errors.New("cannot create provider credential")
	}
	_, err = f.WriteString(key)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		dir, openErr := os.Open(s.secretDir)
		if openErr != nil {
			err = openErr
		} else {
			err = dir.Sync()
			dir.Close()
		}
	}
	if err != nil {
		os.Remove(path)
		return "", errors.New("cannot persist provider credential")
	}
	return ref, nil
}
func (s *Service) key(ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	if decoded, err := hex.DecodeString(ref); err != nil || len(decoded) != 16 {
		return "", errors.New("invalid stored credential reference")
	}
	path := filepath.Join(s.secretDir, ref)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("provider credential unavailable; rotate the connection key")
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return "", errors.New("provider credential unavailable; rotate the connection key")
	}
	return string(b), nil
}

const columns = `id,name,kind,endpoint,models,disabled,health,last_checked,last_error,secret_ref,revision,context_length,keep_alive_seconds,thinking,coalesce((SELECT owner FROM chatgpt_accounts WHERE connection_id=provider_connections.id),'')`

func scanConnection(row interface{ Scan(...any) error }) (Connection, error) {
	var c Connection
	var models string
	err := row.Scan(&c.ID, &c.Name, &c.Kind, &c.Endpoint, &models, &c.Disabled, &c.Health, &c.LastChecked, &c.LastError, &c.secretRef, &c.revision, &c.ContextLength, &c.KeepAliveSeconds, &c.Thinking, &c.Owner)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	err = json.Unmarshal([]byte(models), &c.Models)
	return c, err
}
func (s *Service) Get(ctx context.Context, id string) (Connection, error) {
	return scanConnection(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM provider_connections WHERE id=?`, id))
}
func (s *Service) List(ctx context.Context) ([]Connection, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM provider_connections ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Connection{}
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Service) Create(ctx context.Context, c Config) (Connection, error) {
	return s.save(ctx, "", c)
}
func (s *Service) Update(ctx context.Context, id string, c Config) (Connection, error) {
	if id == "" {
		return Connection{}, ErrNotFound
	}
	return s.save(ctx, id, c)
}
func (s *Service) save(ctx context.Context, connID string, c Config) (Connection, error) {
	var empty Connection
	if err := normalize(&c); err != nil {
		return empty, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var old Connection
	var err error
	create := connID == ""
	if create && c.Kind == "chatgpt" {
		return empty, fmt.Errorf("%w: use Continue with ChatGPT to create a plan connection", ErrInvalid)
	}
	if create {
		connID, err = id()
	} else {
		old, err = s.Get(ctx, connID)
	}
	if err != nil {
		return empty, err
	}
	if !create && old.Kind != c.Kind {
		return empty, fmt.Errorf("%w: a connection's provider kind is fixed", ErrInvalid)
	}
	if create && c.Kind != "ollama" && c.APIKey == "" {
		return empty, fmt.Errorf("%w: provide an API key", ErrInvalid)
	}
	var taken bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM provider_connections WHERE name=? AND id<>?)`, c.Name, connID).Scan(&taken)
	if err != nil {
		return empty, err
	}
	if taken {
		return empty, fmt.Errorf("%w: name is already used", ErrConflict)
	}
	ref := old.secretRef
	if c.APIKey != "" {
		ref, err = s.writeSecret(c.APIKey)
		if err != nil {
			return empty, err
		}
	}
	models, _ := json.Marshal(c.Models)
	tx, err := s.db.BeginTx(ctx, nil)
	// The candidate credential becomes live only after both the config and audit entry commit.
	committed := false
	if ref != old.secretRef {
		defer func() {
			if !committed {
				os.Remove(filepath.Join(s.secretDir, ref))
			}
		}()
	}
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	action := "create AI connection"
	if create {
		_, err = tx.ExecContext(ctx, `INSERT INTO provider_connections(id,name,kind,endpoint,models,disabled,secret_ref,context_length,keep_alive_seconds,thinking) VALUES(?,?,?,?,?,?,?,?,?,?)`, connID, c.Name, c.Kind, c.Endpoint, string(models), c.Disabled, ref, c.ContextLength, c.KeepAliveSeconds, c.Thinking)
	} else {
		action = "update AI connection"
		if c.APIKey != "" {
			action = "rotate AI credential"
		} else if c.Disabled && !old.Disabled {
			action = "disable AI connection"
		}
		_, err = tx.ExecContext(ctx, `UPDATE provider_connections SET name=?,endpoint=?,models=?,disabled=?,secret_ref=?,context_length=?,keep_alive_seconds=?,thinking=?,revision=revision+1,health='untested',last_checked='',last_error='' WHERE id=?`, c.Name, c.Endpoint, string(models), c.Disabled, ref, c.ContextLength, c.KeepAliveSeconds, c.Thinking, connID)
	}
	if err != nil {
		return empty, err
	}
	actor, _ := ctx.Value(actorKey{}).(string)
	if actor == "" {
		actor = "system"
	}
	if err = state.AuditTx(tx, actor, action, connID); err != nil {
		return empty, err
	}
	if err = tx.Commit(); err != nil {
		return empty, err
	}
	committed = true
	if old.secretRef != "" && old.secretRef != ref {
		os.Remove(filepath.Join(s.secretDir, old.secretRef))
	}
	return s.Get(ctx, connID)
}

func (s *Service) load(ctx context.Context, connID string) (Connection, string, error) {
	s.mu.Lock()
	c, err := s.Get(ctx, connID)
	if err != nil || c.Disabled {
		s.mu.Unlock()
		if err == nil {
			err = fmt.Errorf("%w: connection disabled", ErrConflict)
		}
		return c, "", err
	}
	if c.Kind == "chatgpt" {
		s.mu.Unlock()
		return s.chatGPTAccess(ctx, c)
	}
	defer s.mu.Unlock()
	key, err := s.key(c.secretRef)
	return c, key, err
}
func (s *Service) recordHealth(ctx context.Context, c Connection, cause error) error {
	health, msg := "healthy", ""
	if cause != nil {
		health, msg = "error", cause.Error()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE provider_connections SET health=?,last_error=?,last_checked=? WHERE id=? AND revision=?`, health, msg, time.Now().UTC().Format(time.RFC3339Nano), c.ID, c.revision)
	return err
}
func (s *Service) Test(ctx context.Context, connID string) (Connection, error) {
	c, key, err := s.load(ctx, connID)
	if c.ID == "" || c.Disabled {
		return c, err
	}
	if err == nil {
		err = s.testModel(ctx, c, key)
	}
	if saveErr := s.recordHealth(ctx, c, err); saveErr != nil {
		return Connection{}, saveErr
	}
	return s.Get(ctx, connID)
}

// ValidatePolicy checks the template's named route. Repeat at dispatch as an admin may disable a connection or remove a model.
func (c Connection) ValidatePolicy(model string, allowCloud bool) error {
	if c.Disabled {
		return fmt.Errorf("%w: connection disabled", ErrConflict)
	}
	if !slices.Contains(c.Models, model) {
		return fmt.Errorf("%w: model is not permitted on this connection", ErrInvalid)
	}
	if c.Kind != "ollama" && !allowCloud {
		return fmt.Errorf("%w: this task requires local processing", ErrInvalid)
	}
	return nil
}

// OpenPersonal is used only inside a private, OS-user-owned agent runtime.
// The shared application never calls this constructor.
func OpenPersonal(db *sql.DB, secretDir, owner string) (*Service, error) {
	if owner == "" {
		return nil, ErrInvalid
	}
	s, err := openService(db, secretDir, true, owner)
	if err != nil {
		return nil, err
	}
	if err = s.planOwner(context.Background(), owner); err != nil {
		s.Close()
		return nil, err
	}
	s.cloud.Timeout = 30 * time.Minute
	return s, nil
}
