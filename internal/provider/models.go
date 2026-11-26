package provider

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"sluicework/internal/state"
)

const modelSchema = `
ALTER TABLE provider_connections ADD COLUMN context_length INTEGER NOT NULL DEFAULT 0;
ALTER TABLE provider_connections ADD COLUMN keep_alive_seconds INTEGER NOT NULL DEFAULT 0;
ALTER TABLE provider_connections ADD COLUMN thinking INTEGER NOT NULL DEFAULT 0;
CREATE TABLE model_installations (
 id TEXT PRIMARY KEY, connection_id TEXT NOT NULL REFERENCES provider_connections(id),
 endpoint TEXT NOT NULL, model TEXT NOT NULL, status TEXT NOT NULL,
 message TEXT NOT NULL, total INTEGER NOT NULL DEFAULT 0, completed INTEGER NOT NULL DEFAULT 0,
 created TEXT NOT NULL
);
CREATE UNIQUE INDEX model_installations_active ON model_installations(endpoint) WHERE status='running';
INSERT INTO provider_migrations(version) VALUES(2);`

type ModelPreset struct {
	Model       string `json:"model"`
	Name        string `json:"name"`
	Download    string `json:"download"`
	Description string `json:"description"`
	MinMemoryGB int    `json:"minMemoryGB"`
	Optional    bool   `json:"optional"`
	Installable bool   `json:"installable"`
	Source      string `json:"source"`
}

// Sizes are approximate download sizes, not total inference memory requirements.
func ModelCatalog() []ModelPreset {
	return []ModelPreset{
		{"qwen3.5:4b", "Lightweight", "3.3–4.0 GB", "For a 16 GB Mac sharing inference with CI. Build-log summaries, classification and simple extraction.", 16, false, true, "https://ollama.com/library/qwen3.5:4b"},
		{"qwen3.5:9b", "General purpose", "6.6–7.6 GB", "For 24 GB and up. Scheduled AI tasks, more involved extraction and light coding assistance.", 24, false, true, "https://ollama.com/library/qwen3.5:9b"},
		{"gemma4:12b", "General-purpose alternative", "7.7–8.0 GB", "On demand for document tasks. Benchmark against Qwen 9B with your own inputs before switching. Screenshot input is not supported by Sluicework yet.", 24, true, true, "https://ollama.com/library/gemma4/tags"},
		{"qwen3.6:27b-coding", "Coding", "18–19 GB", "For a dedicated 32 GB inference Mac. Prefer 48 GB when sharing with CI or using longer contexts.", 32, true, true, "https://ollama.com/library/qwen3.6/tags"},
		{"qwen3-embedding:0.6b", "Embeddings", "639 MB", "Reserved for future semantic search and retrieval. Scheduled summaries and extraction do not need embeddings.", 0, true, false, "https://ollama.com/library/qwen3-embedding/tags"},
	}
}

type InstalledModel struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}
type Installation struct {
	ID           string `json:"id"`
	ConnectionID string `json:"connectionId"`
	Model        string `json:"model"`
	Status       string `json:"status"`
	Message      string `json:"message"`
	Total        int64  `json:"total"`
	Completed    int64  `json:"completed"`
	Created      string `json:"created"`
}
type ModelInventory struct {
	Catalog       []ModelPreset    `json:"catalog"`
	Installed     []InstalledModel `json:"installed"`
	Installations []Installation   `json:"installations"`
	RuntimeError  string           `json:"runtimeError,omitempty"`
}

func localConnection(c Connection) error {
	if c.Kind != "ollama" {
		return fmt.Errorf("%w: model installation requires an Ollama connection", ErrInvalid)
	}
	if c.Disabled {
		return fmt.Errorf("%w: connection disabled", ErrConflict)
	}
	return nil
}

func (s *Service) installed(ctx context.Context, c Connection) ([]InstalledModel, error) {
	var result struct {
		Models []InstalledModel `json:"models"`
	}
	err := s.request(ctx, c, "", http.MethodGet, "/api/tags", nil, &result)
	if result.Models == nil {
		result.Models = []InstalledModel{}
		if err == nil {
			err = errors.New("runtime did not return a model inventory")
		}
	}
	return result.Models, err
}

func (s *Service) Models(ctx context.Context, connID string) (ModelInventory, error) {
	out := ModelInventory{Catalog: ModelCatalog(), Installed: []InstalledModel{}, Installations: []Installation{}}
	c, err := s.Get(ctx, connID)
	if err != nil {
		return out, err
	}
	if err = localConnection(c); err != nil {
		return out, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,connection_id,model,status,message,total,completed,created FROM model_installations WHERE connection_id=? ORDER BY rowid DESC LIMIT 20`, connID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var v Installation
		if err = rows.Scan(&v.ID, &v.ConnectionID, &v.Model, &v.Status, &v.Message, &v.Total, &v.Completed, &v.Created); err != nil {
			rows.Close()
			return out, err
		}
		out.Installations = append(out.Installations, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	probe, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out.Installed, err = s.installed(probe, c)
	if err != nil {
		out.RuntimeError = "Cannot reach Ollama. Start it on the endpoint Mac and check its URL, TLS and model-management support."
	}
	return out, nil
}

// InstallModel runs independently of browser requests. Only curated local tags
// can be pulled; runtime URLs use the same TLS, private-address and redirect
// policy as inference. The database survives refreshes and records interruption.
func (s *Service) InstallModel(ctx context.Context, connID, model string) (Installation, error) {
	var out Installation
	allowed := false
	for _, preset := range ModelCatalog() {
		if preset.Model == model && preset.Installable {
			allowed = true
		}
	}
	if !allowed {
		return out, fmt.Errorf("%w: select an installable generation model from the catalog", ErrInvalid)
	}
	c, err := s.Get(ctx, connID)
	if err != nil {
		return out, err
	}
	if err = localConnection(c); err != nil {
		return out, err
	}
	// Reserve while holding the lifecycle lock so Close cannot race with Add.
	s.installMu.Lock()
	defer s.installMu.Unlock()
	if s.closed {
		return out, fmt.Errorf("%w: server is shutting down", ErrConflict)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var active int
	if err = tx.QueryRow(`SELECT count(*) FROM model_installations WHERE status='running'`).Scan(&active); err != nil {
		return out, err
	}
	if active >= 4 {
		return out, fmt.Errorf("%w: four model downloads are already running", ErrConflict)
	}
	var busy bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM model_installations WHERE endpoint=? AND status='running')`, c.Endpoint).Scan(&busy); err != nil {
		return out, err
	}
	if busy {
		return out, fmt.Errorf("%w: this endpoint already has a model download in progress", ErrConflict)
	}
	out.ID, err = id()
	if err != nil {
		return out, err
	}
	out.ConnectionID, out.Model, out.Status, out.Message = connID, model, "running", "Connecting to Ollama"
	out.Created = time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.Exec(`INSERT INTO model_installations(id,connection_id,endpoint,model,status,message,created) VALUES(?,?,?,?,?,?,?)`, out.ID, connID, c.Endpoint, model, out.Status, out.Message, out.Created); err != nil {
		return Installation{}, err
	}
	actor, _ := ctx.Value(actorKey{}).(string)
	if actor == "" {
		actor = "system"
	}
	if err = state.AuditTx(tx, actor, "install local model", connID+":"+model); err != nil {
		return Installation{}, err
	}
	if err = tx.Commit(); err != nil {
		return Installation{}, err
	}
	background, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	s.installs[out.ID] = cancel
	s.installWG.Add(1)
	go func() {
		defer s.installWG.Done()
		defer func() { cancel(); s.installMu.Lock(); delete(s.installs, out.ID); s.installMu.Unlock() }()
		s.pullModel(background, c, out, actor)
	}()
	return out, nil
}

func (s *Service) CancelModelInstall(ctx context.Context, connID, installID string) error {
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM model_installations WHERE id=? AND connection_id=?`, installID, connID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if status != "running" {
		return nil
	}
	actor, _ := ctx.Value(actorKey{}).(string)
	if actor == "" {
		actor = "system"
	}
	if err := (&state.Store{DB: s.db}).Audit(actor, "cancel model download", installID); err != nil {
		return err
	}
	s.installMu.Lock()
	defer s.installMu.Unlock()
	if cancel := s.installs[installID]; cancel != nil {
		cancel()
	}
	return nil
}

// Close cancels downloads and waits for their final database writes before DB shutdown.
func (s *Service) Close() {
	s.closePlanSessions()
	s.installMu.Lock()
	s.closed = true
	for _, cancel := range s.installs {
		cancel()
	}
	s.installMu.Unlock()
	s.installWG.Wait()
}

func (s *Service) pullModel(ctx context.Context, c Connection, install Installation, actor string) {
	err := s.pull(ctx, c, install)
	status, message := "failed", "Download failed. Check Ollama's version, network access, free disk space and model availability, then retry."
	if ctx.Err() != nil {
		status, message = "cancelled", "Download stopped. Retry to resume; Ollama may retain partial layers."
	} else if err == nil {
		err = s.finishInstall(ctx, c, install, actor)
		if err == nil {
			return
		}
		message = "Model downloaded, but connection settings changed. Retry to verify and permit the model."
	}
	// Raw runtime diagnostics may contain private paths or remote response data.
	_, _ = s.db.Exec(`UPDATE model_installations SET status=?,message=? WHERE id=? AND status='running'`, status, message, install.ID)
}

func (s *Service) pull(ctx context.Context, c Connection, install Installation) error {
	body, _ := json.Marshal(payload{"model": install.Model, "stream": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := *s.local
	client.Timeout = 0 // The operation context bounds large, multi-gigabyte pulls.
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return errors.New("runtime rejected pull")
	}
	scanner := bufio.NewScanner(io.LimitReader(res.Body, 32<<20))
	scanner.Buffer(make([]byte, 4096), 64<<10)
	var lastSave time.Time
	for scanner.Scan() {
		var event struct {
			Status, Error    string
			Total, Completed int64
		}
		if err = json.Unmarshal(scanner.Bytes(), &event); err != nil || event.Error != "" {
			return errors.New("invalid pull response")
		}
		if event.Status == "success" {
			models, err := s.installed(ctx, c)
			if err != nil {
				return err
			}
			if !slices.ContainsFunc(models, func(m InstalledModel) bool { return m.Name == install.Model }) {
				return errors.New("model missing after download")
			}
			return nil
		}
		if event.Total < 0 || event.Completed < 0 || event.Completed > event.Total {
			return errors.New("invalid pull progress")
		}
		if !lastSave.IsZero() && time.Since(lastSave) < 250*time.Millisecond {
			continue
		}
		message := "Preparing and verifying model"
		if event.Total > 0 {
			message = "Downloading model layer"
		}
		if _, err = s.db.ExecContext(ctx, `UPDATE model_installations SET message=?,total=?,completed=? WHERE id=? AND status='running'`, message, event.Total, event.Completed, install.ID); err != nil {
			return err
		}
		lastSave = time.Now()
	}
	return errors.New("pull ended before success")
}

func (s *Service) finishInstall(ctx context.Context, original Connection, install Installation, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.Get(ctx, original.ID)
	if err != nil {
		return err
	}
	if c.revision != original.revision || c.Disabled {
		return ErrConflict
	}
	if !slices.Contains(c.Models, install.Model) {
		c.Models = append(c.Models, install.Model)
	}
	if len(c.Models) > 128 {
		return ErrConflict
	}
	models, _ := json.Marshal(c.Models)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE provider_connections SET models=?,revision=revision+1,health='untested',last_checked='',last_error='' WHERE id=?`, string(models), c.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE model_installations SET status='succeeded',message='Installed and permitted for AI templates',completed=total WHERE id=?`, install.ID); err != nil {
		return err
	}
	if err = state.AuditTx(tx, actor, "local model installed", c.ID+":"+install.Model); err != nil {
		return err
	}
	return tx.Commit()
}
