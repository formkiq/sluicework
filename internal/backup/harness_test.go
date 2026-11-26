package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"sluicework/internal/auth"
	"sluicework/internal/provider"
	"sluicework/internal/state"
)

func TestRestoreRevokesCopiedCodingCredentials(t *testing.T) {
	root := t.TempDir()
	db, e := state.Open(filepath.Join(root, "original.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = (&state.Store{DB: db}).Init(); e != nil {
		t.Fatal(e)
	}
	if _, e = auth.New(db); e != nil {
		t.Fatal(e)
	}
	service, e := provider.Open(db, filepath.Join(root, "secrets"))
	if e != nil {
		t.Fatal(e)
	}
	defer service.Close()
	ctx := context.Background()
	conn, e := service.Create(ctx, provider.Config{Name: "Private model", Kind: "ollama", Endpoint: "http://127.0.0.1:11434", Models: []string{"qwen"}})
	if e != nil {
		t.Fatal(e)
	}
	_, token, e := service.CreateHarnessClient(ctx, "admin", provider.HarnessClientInput{Name: "Laptop", ConnectionID: conn.ID, Models: []string{"qwen"}, ContextLength: 4096, MaxOutputTokens: 512, MaxConcurrent: 1, TimeoutSeconds: 30, LifetimeDays: 7})
	if e != nil {
		t.Fatal(e)
	}
	config := filepath.Join(root, "server.json")
	if e = os.WriteFile(config, []byte(`{"dataDir":"unused"}`), 0600); e != nil {
		t.Fatal(e)
	}
	snapshot := filepath.Join(root, "snapshot")
	if e = Create(db, config, snapshot); e != nil {
		t.Fatal(e)
	}
	restored := filepath.Join(root, "restored")
	if e = Restore(snapshot, restored); e != nil {
		t.Fatal(e)
	}
	other, e := state.Open(filepath.Join(restored, "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer other.Close()
	p, e := provider.Open(other, filepath.Join(root, "other-secrets"))
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	if _, e = p.AuthenticateHarness(ctx, token); !errors.Is(e, provider.ErrHarnessAuth) {
		t.Fatal("restored credential remained valid")
	}
}
