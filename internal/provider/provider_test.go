package provider

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"sluicework/internal/state"
)

func setup(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := (&state.Store{DB: db}).Init(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(db, filepath.Join(dir, "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func ok(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestCloudContractsAndRotation(t *testing.T) {
	cases := []struct{ kind, host, credHeader, testPath, generatePath, payloadField, metadata, completion string }{
		{"openai", "api.openai.com", "Authorization", "/v1/models/model-1", "/v1/responses", "max_output_tokens", `{"id":"model-1"}`, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"summary"}]}],"usage":{"input_tokens":17,"output_tokens":3}}`},
		{"anthropic", "api.anthropic.com", "x-api-key", "/v1/models/model-1", "/v1/messages", "max_tokens", `{"id":"model-1"}`, `{"content":[{"type":"thinking","thinking":"private"},{"type":"text","text":"summary"}],"usage":{"input_tokens":17,"output_tokens":3}}`},
		{"gemini", "generativelanguage.googleapis.com", "x-goog-api-key", "/v1beta/models/model-1", "/v1beta/models/model-1:generateContent", "generationConfig", `{"name":"models/model-1"}`, `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"private","thought":true},{"text":"summary"}]}}],"usageMetadata":{"promptTokenCount":17,"candidatesTokenCount":3}}`},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			s := setup(t)
			ctx := WithActor(context.Background(), "pilot-admin")
			key := "secret-test-1"
			calls := 0
			s.cloud.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				expect := key
				if tc.kind == "openai" {
					expect = "Bearer " + key
				}
				if r.URL.Scheme != "https" || r.URL.Host != tc.host || r.URL.RawQuery != "" || r.Header.Get(tc.credHeader) != expect {
					t.Error("incorrect cloud destination or authentication")
				}
				if tc.kind == "anthropic" && r.Header.Get("anthropic-version") != "2023-06-01" {
					t.Error("missing API version")
				}
				if r.Method == "GET" && r.URL.Path == tc.testPath {
					return ok(tc.metadata), nil
				}
				if r.Method != "POST" || r.URL.Path != tc.generatePath {
					t.Errorf("unexpected route %s", r.URL.Path)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body[tc.payloadField] == nil {
					t.Error("invalid provider payload")
				}
				if tc.kind == "openai" && body["store"] != false {
					t.Error("Responses storage must be disabled")
				}
				return ok(tc.completion), nil
			})
			config := Config{Name: "project", Kind: tc.kind, Models: []string{"model-1"}, APIKey: key}
			conn, err := s.Create(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			public, _ := json.Marshal(conn)
			if strings.Contains(string(public), key) || strings.Contains(string(public), conn.secretRef) {
				t.Fatal("credential leaked in public DTO")
			}
			req := Request{AttemptID: "try-1", ConnectionID: conn.ID, Model: "model-1", Prompt: "private-test-input", MaxTokens: 128}
			if _, err := s.Generate(ctx, req); !errors.Is(err, ErrInvalid) || calls != 0 {
				t.Fatal("cloud call accepted without explicit cloud policy")
			}
			health, err := s.Test(ctx, conn.ID)
			if err != nil || health.Health != "healthy" {
				t.Fatalf("health %#v %v", health, err)
			}
			path := filepath.Join(s.secretDir, conn.secretRef)
			info, err := os.Lstat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Error("credential file permissions")
			}
			key = "secret-test-2"
			config.APIKey = key
			rotated, err := s.Update(ctx, conn.ID, config)
			if err != nil || rotated.secretRef == conn.secretRef {
				t.Fatal("rotation failed", err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Error("old credential remains")
			}
			req.AllowCloud = true
			res, err := s.Generate(ctx, req)
			if err != nil || res.Text != "summary" {
				t.Fatal(res, err)
			}
			if _, err := s.Generate(ctx, req); !errors.Is(err, ErrConflict) {
				t.Error("accepted duplicate dispatch")
			}
			if calls != 2 {
				t.Errorf("made %d vendor requests", calls)
			}
			tracked, err := s.Calls(ctx, req.AttemptID)
			if err != nil || len(tracked) != 1 || tracked[0].Status != "succeeded" || *tracked[0].InputTokens != 17 || *tracked[0].OutputTokens != 3 {
				t.Fatal(tracked, err)
			}
			meta, _ := json.Marshal(tracked)
			if strings.Contains(string(meta), res.Text) || strings.Contains(string(meta), req.Prompt) {
				t.Error("request contents in diagnostics")
			}
			config.Disabled = true
			config.APIKey = ""
			if _, err := s.Update(ctx, conn.ID, config); err != nil {
				t.Fatal(err)
			}
			req.AttemptID = "try-2"
			if _, err := s.Generate(ctx, req); !errors.Is(err, ErrConflict) || calls != 2 {
				t.Error("disabled connection dispatched")
			}
			var count int
			if err := s.db.QueryRow("SELECT count(*) FROM audit WHERE actor=? AND target=?", "pilot-admin", conn.ID).Scan(&count); err != nil || count != 3 {
				t.Error("audit history incomplete", count, err)
			}
		})
	}
}

func TestLocalNoFallbackAndSafeDiagnostics(t *testing.T) {
	ctx := context.Background()
	s := setup(t)
	var external atomic.Int32
	var redirect atomic.Int32
	s.cloud.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		external.Add(1)
		return nil, errors.New("unexpected cloud request")
	})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirect.Add(1) }))
	defer target.Close()
	var fail atomic.Bool
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			io.WriteString(w, `{"models":[{"name":"llama:latest"}]}`)
			return
		}
		if r.URL.Path != "/api/generate" {
			t.Error("unexpected ollama route", r.URL.Path)
		}
		if fail.Load() {
			w.Header().Set("Location", target.URL)
			w.WriteHeader(http.StatusTemporaryRedirect)
			io.WriteString(w, "sensitive provider error")
			return
		}
		var body struct {
			Model   string
			Prompt  string
			Stream  bool
			Options map[string]int
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Prompt != "summarize selected test failures" || body.Options["num_predict"] != 99 {
			t.Error("incorrect Ollama request")
		}
		io.WriteString(w, `{"response":"two failures","done":true,"done_reason":"length","prompt_eval_count":6,"eval_count":2}`)
	}))
	defer host.Close()
	conn, err := s.Create(ctx, Config{Name: "remote Mac", Kind: "ollama", Endpoint: host.URL, Models: []string{"llama"}})
	if err != nil {
		t.Fatal(err)
	}
	health, err := s.Test(ctx, conn.ID)
	if err != nil || health.Health != "healthy" {
		t.Fatal(health, err)
	}
	r := Request{AttemptID: "job1", ConnectionID: conn.ID, Model: "llama", Prompt: "summarize selected test failures", MaxTokens: 99}
	res, err := s.Generate(ctx, r)
	if err != nil || !res.Truncated || res.Text != "two failures" {
		t.Error("local inference failed", res, err)
	}
	fail.Store(true)
	r.AttemptID = "job2"
	_, err = s.Generate(ctx, r)
	if err == nil || err.Error() != "provider returned HTTP 307" {
		t.Fatal(err)
	}
	health, _ = s.Get(ctx, conn.ID)
	if health.Health != "error" || strings.Contains(health.LastError, "sensitive") {
		t.Error("failure diagnostics", health)
	}
	host.Close()
	r.AttemptID = "job3"
	if _, err := s.Generate(ctx, r); err == nil {
		t.Error("unavailable local connection reported success")
	}
	if external.Load() != 0 || redirect.Load() != 0 {
		t.Error("local request forwarded")
	}
}

func TestRestoreSecretsAndConfigurationValidation(t *testing.T) {
	s := setup(t)
	ctx := context.Background()
	config := Config{Name: "cloud", Kind: "gemini", Models: []string{"gemini-test"}, APIKey: "credential"}
	conn, err := s.Create(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.secretDir, conn.secretRef)); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(s.db, s.secretDir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := restored.Get(ctx, conn.ID)
	if err != nil || h.Health != "error" || !strings.Contains(h.LastError, "rotate") {
		t.Fatal("missing key health", h, err)
	}
	config.APIKey = "replacement"
	if _, err := s.Update(ctx, conn.ID, config); err != nil {
		t.Fatal(err)
	}
	_, key, err := s.load(ctx, conn.ID)
	if err != nil || key != "replacement" {
		t.Fatal("could not recover credential")
	}
	config.Endpoint = "https://outside.example"
	if _, err := s.Update(ctx, conn.ID, config); !errors.Is(err, ErrInvalid) {
		t.Fatal("custom cloud destination accepted")
	}
	for _, tc := range []struct {
		ip    string
		local bool
	}{{"192.168.1.10", true}, {"127.0.0.1", true}, {"fd00::a", true}, {"::1", true}, {"1.2.3.4", false}} {
		if isLAN(netip.MustParseAddr(tc.ip)) != tc.local {
			t.Error(tc.ip)
		}
	}
	if _, err := dialLAN(ctx, "tcp", "1.1.1.1:80"); err == nil {
		t.Error("public destination accepted")
	}
	for _, bad := range []Config{{Kind: "ollama", Endpoint: "http://192.168.50.3:11434", Models: []string{"qwen"}}, {Kind: "ollama", Endpoint: "https://1.1.1.1", Models: []string{"qwen"}}, {Kind: "ollama", Endpoint: "http://u:pass@127.0.0.1", Models: []string{"qwen"}}, {Kind: "ollama", Endpoint: "http://localhost", Models: []string{"gpt-oss:120b-cloud"}}} {
		bad.Name = "bad"
		if _, err := s.Create(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid config accepted for %v", bad.Models)
		}
	}
}

func TestRotateDuringActiveRequest(t *testing.T) {
	s := setup(t)
	ctx := t.Context()
	started := make(chan struct{})
	release := make(chan struct{})
	config := Config{Name: "project", Kind: "openai", APIKey: "old-credential", Models: []string{"model-1"}}
	c, err := s.Create(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	s.cloud.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if requests.Add(1) != 1 {
			return nil, errors.New("unexpected second vendor request")
		}
		close(started)
		<-release
		return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("sensitive auth body"))}, nil
	})
	req := Request{AttemptID: "attempt", ConnectionID: c.ID, Model: "model-1", Prompt: "report", AllowCloud: true}
	done := make(chan error, 1)
	go func() { _, err := s.Generate(ctx, req); done <- err }()
	<-started
	_, duplicateErr := s.Generate(ctx, req)
	config.APIKey = "new-credential"
	_, rotateErr := s.Update(ctx, c.ID, config)
	close(release)
	callErr := <-done
	if !errors.Is(duplicateErr, ErrConflict) || rotateErr != nil || callErr == nil || callErr.Error() != "provider returned HTTP 401" {
		t.Fatal(duplicateErr, rotateErr, callErr)
	}
	updated, err := s.Get(ctx, c.ID)
	if err != nil || updated.Health != "untested" || updated.LastError != "" || requests.Load() != 1 {
		t.Fatal("stale request overwrote rotated connection health", updated, err)
	}
}

func TestInferenceTLSVerifiesRuntimeCertificate(t *testing.T) {
	runtime := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"models":[{"name":"pilot"}]}`)
	}))
	runtime.Config.ErrorLog = log.New(io.Discard, "", 0)
	runtime.StartTLS()
	defer runtime.Close()
	s := setup(t)
	ctx := t.Context()
	conn, err := s.Create(ctx, Config{Kind: "ollama", Name: "Herd router", Endpoint: runtime.URL, Models: []string{"pilot"}})
	if err != nil {
		t.Fatal(err)
	}
	conn, err = s.Test(ctx, conn.ID)
	if err != nil || conn.Health != "error" {
		t.Fatal("untrusted certificate accepted", conn, err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(runtime.Certificate())
	transport := s.local.Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.RootCAs = roots
	s.local.Transport = transport
	conn, err = s.Test(ctx, conn.ID)
	if err != nil || conn.Health != "healthy" {
		t.Fatal("trusted runtime certificate rejected", conn, err)
	}
}
