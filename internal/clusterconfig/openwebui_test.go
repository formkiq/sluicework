package clusterconfig

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"sluicework/internal/state"
)

func TestOpenWebUIConfigurationEnsureAndProbe(t *testing.T) {
	m, db := fixture(t)

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":true}`))
	}))
	defer healthy.Close()

	// Reject non-loopback HTTP and paths.
	for _, badURL := range []string{"http://192.168.1.5:8080", "https://example.com/chat", "https://user:pass@example.com"} {
		if err := m.SetOpenWebUI("admin", OpenWebUIInput{Enabled: true, URL: badURL}); !errors.Is(err, state.ErrInvalid) {
			t.Fatalf("expected invalid origin for %q, got %v", badURL, err)
		}
	}

	view, err := m.EnsureOpenWebUI("local administrator", healthy.URL+"/", "http://127.0.0.1:11434/")
	if err != nil {
		t.Fatal(err)
	}
	if !view.Enabled || view.URL != healthy.URL || view.OllamaBaseURL != "http://127.0.0.1:11434" {
		t.Fatalf("unexpected ensured Open WebUI view: %+v", view)
	}

	status := m.OpenWebUIStatus(context.Background())
	if !status.Configured || !status.Enabled || !status.Healthy || status.Error != "" {
		t.Fatalf("expected healthy Open WebUI status, got %+v", status)
	}

	// EnsureOpenWebUI must preserve an existing disabled state and custom URL.
	if err := m.SetOpenWebUI("admin", OpenWebUIInput{Enabled: false, URL: "https://chat.local:8443", OllamaBaseURL: "http://127.0.0.1:11435"}); err != nil {
		t.Fatal(err)
	}
	preserved, err := m.EnsureOpenWebUI("local administrator", healthy.URL, "http://127.0.0.1:11434")
	if err != nil {
		t.Fatal(err)
	}
	if preserved.Enabled || preserved.URL != "https://chat.local:8443" || preserved.OllamaBaseURL != "http://127.0.0.1:11435" {
		t.Fatalf("EnsureOpenWebUI overwrote existing settings: %+v", preserved)
	}

	// Persists across restart.
	m.Close()
	restarted, err := Open(db, Seed{DataDir: m.dir, Listen: m.primary})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	snap := restarted.Snapshot()
	if snap.OpenWebUI == nil || snap.OpenWebUI.Enabled || snap.OpenWebUI.URL != "https://chat.local:8443" {
		t.Fatalf("Open WebUI settings did not persist across restart: %+v", snap.OpenWebUI)
	}
}
