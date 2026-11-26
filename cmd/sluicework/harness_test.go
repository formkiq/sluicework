package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sluicework/internal/codexclient"
	"sluicework/internal/provider"
	"sluicework/internal/state"
)

func TestHeadlessCodingClientCreatesPrivateScopeWithoutOverwriting(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	config := filepath.Join(root, "server.json")
	raw, _ := json.Marshal(serverConfig{DataDir: data, Listen: "127.0.0.1:7443"})
	if e := os.WriteFile(config, raw, 0600); e != nil {
		t.Fatal(e)
	}
	db, e := state.Open(filepath.Join(data, "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	st := state.Store{DB: db}
	if e = st.Init(); e != nil {
		t.Fatal(e)
	}
	p, e := provider.Open(db, filepath.Join(data, "secrets"))
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	conn, e := p.Create(ctx, provider.Config{Name: "Remote model", Kind: "ollama", Endpoint: "https://model.example", Models: []string{"qwen"}})
	if e != nil {
		t.Fatal(e)
	}
	dest := filepath.Join(root, "client.json")
	args := []string{"--config", config, "--connection", conn.ID, "--models", "qwen", "--name", "Remote coding workstation", "--server-url", "https://orchestrator.example:8443", "--out", dest, "--enable"}
	var out bytes.Buffer
	if e = harnessClientCommand(ctx, args, &out); e != nil {
		t.Fatal(e)
	}
	bundle, e := codexclient.Load(dest)
	if e != nil {
		t.Fatal(e)
	}
	if bundle.ServerURL != "https://orchestrator.example:8443" || strings.Contains(out.String(), bundle.Token) {
		t.Fatal("explicit origin or secret isolation failed")
	}
	if _, e = p.AuthenticateHarness(ctx, bundle.Token); e != nil {
		t.Fatal(e)
	}
	policy, e := p.HarnessPolicy(ctx)
	if e != nil || !policy.Enabled {
		t.Fatal("explicit enable not applied", e)
	}
	if e = harnessClientCommand(ctx, []string{"--config", config, "--client-id", bundle.ClientID, "--timeout", "1800"}, &out); e != nil {
		t.Fatal(e)
	}
	updated, e := p.AuthenticateHarness(ctx, bundle.Token)
	if e != nil || updated.TimeoutSeconds != 1800 {
		t.Fatal("headless update lost access or deadline", e)
	}
	before, e := os.ReadFile(dest)
	if e != nil {
		t.Fatal(e)
	}
	if e = harnessClientCommand(ctx, args, &out); e == nil {
		t.Fatal("overwrote private client file")
	}
	after, _ := os.ReadFile(dest)
	if !bytes.Equal(before, after) {
		t.Fatal("existing private file changed")
	}
	clients, e := p.HarnessClients(ctx)
	if e != nil || len(clients) != 1 {
		t.Fatal("duplicate grant created", e)
	}
}
