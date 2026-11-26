package provider

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeProbeProtocolsAndTLS(t *testing.T) {
	for _, mode := range []string{"ollama", "herd", "invalid", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("probe changed runtime state")
				}
				if mode == "redirect" {
					http.Redirect(w, r, "/elsewhere", 302)
					return
				}
				if mode == "invalid" {
					fmt.Fprint(w, `{}`)
					return
				}
				if r.URL.Path == "/api/tags" {
					fmt.Fprint(w, `{"models":[]}`)
					return
				}
				if r.URL.Path == "/api/version" && mode == "ollama" {
					fmt.Fprint(w, `{"version":"0.40.16"}`)
					return
				}
				w.WriteHeader(404)
			}))
			defer server.Close()
			info, err := ProbeLocal(context.Background(), server.URL)
			if mode == "invalid" || mode == "redirect" {
				if err == nil {
					t.Fatal("non-runtime accepted")
				}
				return
			}
			if err != nil || info.Endpoint != server.URL {
				t.Fatal(info, err)
			}
			if (mode == "ollama" && info.Version != "0.40.16") || (mode == "herd" && info.Version != "unknown") {
				t.Fatal(info)
			}
		})
	}
	for _, endpoint := range []string{"http://192.168.1.20:11434", "https://8.8.8.8:11434", "http://admin:secret@localhost:11434"} {
		if _, err := ProbeLocal(context.Background(), endpoint); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unsafe origin accepted: %s %v", endpoint, err)
		}
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"models":[]}`) }))
	defer server.Close()
	if _, err := ProbeLocal(context.Background(), server.URL); err == nil {
		t.Fatal("untrusted TLS certificate accepted")
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ProbeLocalWithCA(context.Background(), server.URL, ca); err != nil {
		t.Fatalf("explicit trusted CA failed: %v", err)
	}
}

func TestInstallerConfigurationPreservesActiveWorkAndSettings(t *testing.T) {
	s := setup(t)
	ctx := context.Background()
	original, err := s.Create(ctx, Config{Name: "Existing AI", Kind: "ollama", Endpoint: "http://localhost:11434", Models: []string{"custom:tag"}, Disabled: true, LocalOptions: LocalOptions{ContextLength: 8192, KeepAliveSeconds: 300, Thinking: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO provider_calls(attempt_id,connection_id,kind,model,started,status) VALUES('active-call',?,'ollama','custom:tag','','running')`, original.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO model_installations(id,connection_id,endpoint,model,status,message,created) VALUES('active-pull',?,?,'qwen3.5:4b','running','','')`, original.ID, original.Endpoint); err != nil {
		t.Fatal(err)
	}
	config, err := OpenForConfiguration(s.db, s.secretDir)
	if err != nil {
		t.Fatal(err)
	}
	defer config.Close()
	got, err := config.EnsureLocal(ctx, Config{Name: "Local AI", Kind: "ollama", Endpoint: original.Endpoint, Models: []string{"qwen3.5:4b"}})
	if err != nil || got.ID != original.ID || !got.Disabled || got.Models[0] != "custom:tag" || got.ContextLength != 8192 || got.KeepAliveSeconds != 300 || !got.Thinking {
		t.Fatalf("existing connection changed: %+v %v", got, err)
	}
	var status string
	for _, query := range []string{`SELECT status FROM provider_calls WHERE attempt_id='active-call'`, `SELECT status FROM model_installations WHERE id='active-pull'`} {
		if err = s.db.QueryRow(query).Scan(&status); err != nil || status != "running" {
			t.Fatal("installer interrupted active work", status, err)
		}
	}
	created, err := config.EnsureLocal(WithActor(ctx, "local administrator"), Config{Name: "Herd routing", Kind: "ollama", Endpoint: "http://127.0.0.1:11435", Models: []string{"qwen3.5:4b"}, LocalOptions: LocalOptions{ContextLength: 4096}})
	if err != nil || created.ContextLength != 4096 {
		t.Fatal(created, err)
	}
	if _, err = config.EnsureLocal(ctx, Config{Name: original.Name, Kind: "ollama", Endpoint: "http://127.0.0.1:11434", Models: []string{"qwen3.5:4b"}}); !errors.Is(err, ErrConflict) {
		t.Fatal("overwrote conflicting name", err)
	}
}
