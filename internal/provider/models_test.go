package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sluicework/internal/state"
)

func awaitInstallation(t *testing.T, s *Service, id, status string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var got string
		if err := s.db.QueryRow("SELECT status FROM model_installations WHERE id=?", id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got == status {
			return
		}
		if got != "running" {
			t.Fatalf("installation status %q, expected %q", got, status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("installation did not finish")
}

func TestModelInstallLifecycle(t *testing.T) {
	s := setup(t)
	ctx := WithActor(context.Background(), "model-admin")
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("model pull must not receive credentials")
		}
		switch r.URL.Path {
		case "/api/pull":
			var body struct {
				Model    string
				Stream   bool
				Insecure bool
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Model != "qwen3.5:9b" || !body.Stream || body.Insecure {
				t.Error("wrong pull request")
			}
			fmt.Fprintln(w, `{"status":"pulling layer","total":1000,"completed":250}`)
			w.(http.Flusher).Flush()
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			fmt.Fprintln(w, `{"status":"success"}`)
		case "/api/tags":
			fmt.Fprint(w, `{"models":[{"name":"qwen3.5:9b","size":7600000000}]}`)
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c, err := s.Create(ctx, Config{Name: "local", Kind: "ollama", Endpoint: server.URL, Models: []string{"qwen3.5:4b"}, LocalOptions: LocalOptions{ContextLength: 4096}})
	if err != nil {
		t.Fatal(err)
	}
	install, err := s.InstallModel(ctx, c.ID, "qwen3.5:9b")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err = s.InstallModel(ctx, c.ID, "qwen3.5:9b"); !errors.Is(err, ErrConflict) {
		t.Fatal("duplicate installation accepted", err)
	}
	view, err := s.Models(ctx, c.ID)
	if err != nil || len(view.Installations) != 1 || view.Installations[0].Status != "running" {
		t.Fatalf("refresh lost active download: %+v %v", view, err)
	}
	// A connection's existing permission set is not overwritten by completion.
	close(release)
	awaitInstallation(t, s, install.ID, "succeeded")
	updated, err := s.Get(ctx, c.ID)
	if err != nil || len(updated.Models) != 2 || updated.Models[0] != "qwen3.5:4b" || updated.Models[1] != "qwen3.5:9b" || updated.ContextLength != 4096 {
		t.Fatalf("configuration after install: %+v %v", updated, err)
	}
	var audits int
	if err = s.db.QueryRow("SELECT count(*) FROM audit WHERE actor='model-admin' AND action IN ('install local model','local model installed')").Scan(&audits); err != nil || audits != 2 {
		t.Fatal("missing installation audit", audits, err)
	}
}

func TestModelInstallFailureAndCancellation(t *testing.T) {
	for _, mode := range []string{"cancel", "shutdown", "error", "truncated", "missing", "edited", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			s := setup(t)
			ctx := context.Background()
			started, release := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/tags" {
					if mode == "edited" {
						fmt.Fprint(w, `{"models":[{"name":"qwen3.5:9b"}]}`)
						return
					}
					fmt.Fprint(w, `{"models":[]}`)
					return
				}
				if r.URL.Path != "/api/pull" {
					t.Errorf("unexpected route %s", r.URL.Path)
					return
				}
				if mode == "redirect" {
					http.Redirect(w, r, "/must-not-follow", 307)
					return
				}
				fmt.Fprintln(w, `{"status":"pulling","total":100,"completed":10}`)
				w.(http.Flusher).Flush()
				if mode == "cancel" || mode == "shutdown" {
					close(started)
					<-r.Context().Done()
					return
				}
				if mode == "edited" {
					close(started)
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				if mode == "error" {
					fmt.Fprintln(w, `{"error":"private-token-and-path"}`)
					return
				}
				if mode != "truncated" {
					fmt.Fprintln(w, `{"status":"success"}`)
				}
			}))
			defer server.Close()
			c, err := s.Create(ctx, Config{Name: "local", Kind: "ollama", Endpoint: server.URL, Models: []string{"qwen3.5:4b"}})
			if err != nil {
				t.Fatal(err)
			}
			install, err := s.InstallModel(ctx, c.ID, "qwen3.5:9b")
			if err != nil {
				t.Fatal(err)
			}
			want := "failed"
			if mode == "cancel" || mode == "shutdown" {
				<-started
				if mode == "shutdown" {
					s.Close()
				} else if err = s.CancelModelInstall(ctx, c.ID, install.ID); err != nil {
					t.Fatal(err)
				}
				want = "cancelled"
			}
			if mode == "edited" {
				<-started
				if _, err = s.Update(ctx, c.ID, Config{Name: "edited", Kind: "ollama", Endpoint: server.URL, Models: c.Models}); err != nil {
					t.Fatal(err)
				}
				close(release)
			}
			awaitInstallation(t, s, install.ID, want)
			var message string
			s.db.QueryRow("SELECT message FROM model_installations WHERE id=?", install.ID).Scan(&message)
			if strings.Contains(message, "private-token") {
				t.Fatal("raw provider error exposed")
			}
			current, _ := s.Get(ctx, c.ID)
			if len(current.Models) != 1 {
				t.Fatal("failed model was permitted")
			}
		})
	}
}

func TestModelSchemaUpgrade(t *testing.T) {
	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = (&state.Store{DB: db}).Init(); err != nil {
		t.Fatal(err)
	}
	// Reproduce an existing v1 provider database without the new columns.
	if _, err = db.Exec(`CREATE TABLE provider_migrations(version INTEGER PRIMARY KEY);` + schema); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO provider_connections(id,name,kind,endpoint,models,disabled,secret_ref) VALUES('existing','Old local AI','ollama','http://localhost:11434','["custom:tag"]',0,'')`); err != nil {
		t.Fatal(err)
	}
	s, err := Open(db, filepath.Join(dir, "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.Get(context.Background(), "existing")
	if err != nil || c.Models[0] != "custom:tag" || c.ContextLength != 0 || c.KeepAliveSeconds != 0 {
		t.Fatalf("upgrade lost connection: %+v %v", c, err)
	}
	var version int
	if err = db.QueryRow(`SELECT max(version) FROM provider_migrations`).Scan(&version); err != nil || version != 2 {
		t.Fatal("migration version", version, err)
	}
}

func TestModelInstallPolicyAndRuntimeOptions(t *testing.T) {
	s := setup(t)
	ctx := context.Background()
	c, err := s.Create(ctx, Config{Name: "local", Kind: "ollama", Endpoint: "http://127.0.0.1:11434", Models: []string{"qwen3.5:4b"}, LocalOptions: LocalOptions{ContextLength: 4096, KeepAliveSeconds: 0}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"qwen3-embedding:0.6b", "gemma4:cloud", "qwen3.5:4b;anything", "arbitrary:latest"} {
		if _, err = s.InstallModel(ctx, c.ID, tag); !errors.Is(err, ErrInvalid) {
			t.Errorf("unsupported model accepted %s: %v", tag, err)
		}
	}
	thinking := false
	s.local.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		var body struct {
			KeepAlive string `json:"keep_alive"`
			Think     *bool  `json:"think"`
			Options   struct {
				Context int `json:"num_ctx"`
				Predict int `json:"num_predict"`
			}
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.KeepAlive != "0s" || body.Options.Context != 4096 || body.Options.Predict != 128 || body.Think == nil || *body.Think != thinking {
			t.Errorf("runtime settings not applied: %+v", body)
		}
		return ok(`{"response":"summary","done":true}`), nil
	})
	if _, err = s.Generate(ctx, Request{AttemptID: "with-options", ConnectionID: c.ID, Model: c.Models[0], Prompt: "summarize", MaxTokens: 128}); err != nil {
		t.Fatal(err)
	}
	thinking = true
	if _, err = s.Update(ctx, c.ID, Config{Name: c.Name, Kind: c.Kind, Endpoint: c.Endpoint, Models: c.Models, LocalOptions: LocalOptions{ContextLength: 4096, Thinking: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Generate(ctx, Request{AttemptID: "thinking-options", ConnectionID: c.ID, Model: c.Models[0], Prompt: "summarize", MaxTokens: 128}); err != nil {
		t.Fatal(err)
	}
	for _, options := range []LocalOptions{{ContextLength: -1}, {ContextLength: 100}, {ContextLength: 32769}, {KeepAliveSeconds: -1}, {KeepAliveSeconds: 301}} {
		if _, err = s.Update(ctx, c.ID, Config{Name: "local", Kind: "ollama", Endpoint: c.Endpoint, Models: c.Models, LocalOptions: options}); !errors.Is(err, ErrInvalid) {
			t.Errorf("bad runtime options accepted: %+v", options)
		}
	}
	cloud, err := s.Create(ctx, Config{Name: "cloud", Kind: "openai", Models: []string{"a-model"}, APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.InstallModel(ctx, cloud.ID, "qwen3.5:4b"); !errors.Is(err, ErrInvalid) {
		t.Fatal("cloud install accepted", err)
	}
	if _, err = s.db.Exec(`INSERT INTO model_installations VALUES('restart',?,?,?,'running','',0,0,'')`, c.ID, c.Endpoint, "qwen3.5:4b"); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.db, s.secretDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	awaitInstallation(t, reopened, "restart", "interrupted")
}
