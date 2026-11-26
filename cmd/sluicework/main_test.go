package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sluicework/internal/auth"
	"sluicework/internal/provider"
	"sluicework/internal/schedule"
	"sluicework/internal/state"
)

func TestRuntimeAndLocalAICommands(t *testing.T) {
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Write([]byte(`{"models":[]}`))
			return
		}
		w.Write([]byte(`{"version":"fixture"}`))
	}))
	defer runtime.Close()
	var out bytes.Buffer
	if err := execute(context.Background(), []string{"runtime-check", "--endpoint", runtime.URL}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	var info provider.RuntimeInfo
	if err := json.Unmarshal(out.Bytes(), &info); err != nil || info.Version != "fixture" {
		t.Fatal(info, err)
	}
	dir := t.TempDir()
	config := filepath.Join(dir, "server.json")
	raw, _ := json.Marshal(serverConfig{DataDir: filepath.Join(dir, "data")})
	if err := os.WriteFile(config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := execute(context.Background(), []string{"init", "--config", config}, strings.NewReader("local-setup-test-password"), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var first provider.Connection
	for _, name := range []string{"Local AI", "Preserve existing"} {
		out.Reset()
		if err := execute(context.Background(), []string{"local-ai", "--config", config, "--endpoint", runtime.URL, "--name", name}, strings.NewReader(""), &out); err != nil {
			t.Fatal(err)
		}
		var got provider.Connection
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if first.ID == "" {
			first = got
		}
		if got.ID != first.ID || got.Name != "Local AI" || got.ContextLength != 4096 {
			t.Fatalf("installer did not preserve route: %+v", got)
		}
	}
}

func TestLiveBackupAndReplacementRestore(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	config := func(name string) (string, string) {
		data := filepath.Join(dir, name)
		b, err := json.Marshal(serverConfig{DataDir: data, Listen: "127.0.0.1:7443"})
		if err != nil {
			t.Fatal(err)
		}
		path := data + ".json"
		if err = os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
		return path, data
	}
	cmd := func(input string, args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := execute(ctx, args, strings.NewReader(input), &out); err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out.String())
	}
	source, data := config("original")
	password := "a test password for the rehearsal"
	cmd(password, "init", "--config", source)
	db, err := state.Open(filepath.Join(data, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := state.Store{DB: db}
	users, err := auth.New(db)
	if err != nil {
		t.Fatal(err)
	}
	_, session, err := users.Login("admin", password)
	if err != nil {
		t.Fatal(err)
	}
	token := cmd("", "enroll-token", "--config", source)
	m, err := st.Enroll(token, state.WorkerInfo{Name: "original", Version: version, Roles: []string{"script"}})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := st.Jobs()
	if err != nil || len(jobs) != 1 {
		t.Fatalf("bootstrap jobs: %v %v", jobs, err)
	}
	job := jobs[0]
	run, err := st.Queue("admin", job.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := st.Claim(m.ID)
	if err != nil || c == nil {
		t.Fatalf("claim: %v", err)
	}
	if err = st.AppendLog(m.ID, c.AttemptID, "backup fixture\n"); err != nil {
		t.Fatal(err)
	}
	if err = st.Complete(m.ID, c.AttemptID, state.Completion{State: "succeeded", Output: "persisted"}); err != nil {
		t.Fatal(err)
	}
	sch := schedule.New(db, &st)
	if err = sch.Init(); err != nil {
		t.Fatal(err)
	}
	plan, err := sch.Save("admin", "", schedule.Spec{Name: "Daily check", JobID: job.ID, Cron: "5 7 * * *", Timezone: "America/Winnipeg"})
	if err != nil {
		t.Fatal(err)
	}
	running, err := st.Queue("admin", job.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.Claim(m.ID); err != nil {
		t.Fatal(err)
	}
	queued, err := st.Queue("admin", job.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := provider.Open(db, filepath.Join(data, "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := p.Create(ctx, provider.Config{Name: "AI", Kind: "openai", Models: []string{"test-model"}, APIKey: "synthetic-key-for-restore-test"})
	if err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(dir, "snapshot")
	cmd("", "backup", "--config", source, "--out", snap)
	dest, replacement := config("replacement")
	cmd("", "restore", "--config", dest, "--from", snap)
	restored, err := state.Open(filepath.Join(replacement, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	copy := state.Store{DB: restored}
	logs, err := copy.Logs(run.ID, 0)
	if err != nil || len(logs) != 1 || logs[0].Text != "backup fixture\n" {
		t.Fatalf("restored logs %v %v", logs, err)
	}
	record, err := copy.Run(run.ID)
	if err != nil || record.Attempts[0].Output != "persisted" {
		t.Fatalf("restored history: %v %v", record, err)
	}
	abandoned, err := copy.Run(running.ID)
	if err != nil || abandoned.State != "interrupted" || !abandoned.Attempts[0].OutcomeUnknown {
		t.Fatalf("running snapshot: %v %v", abandoned, err)
	}
	pending, err := copy.Run(queued.ID)
	if err != nil || pending.State != "cancelled" {
		t.Fatalf("queued snapshot: %v %v", pending, err)
	}
	recovered, err := schedule.New(restored, &copy).Get(plan.ID)
	if err != nil || recovered.NextRun == nil || !recovered.NextRun.Equal(*plan.NextRun) {
		t.Fatalf("restored schedule: %v %v", recovered, err)
	}
	if _, err = copy.AuthenticateWorker(m.Credential); err == nil {
		t.Fatal("restored worker credential remained active")
	}
	accounts, err := auth.New(restored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = accounts.Lookup(session); err == nil {
		t.Fatal("restored browser session remained active")
	}
	if _, _, err = accounts.Login("admin", password); err != nil {
		t.Fatal(err)
	}
	conns, err := provider.Open(restored, filepath.Join(replacement, "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	health, err := conns.Get(ctx, conn.ID)
	if err != nil || health.Health != "error" {
		t.Fatalf("missing restored credential: %v %v", health, err)
	}
	if err = execute(ctx, []string{"restore", "--config", dest, "--from", snap}, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Fatal("overwrote existing data directory")
	}
	if err = os.WriteFile(filepath.Join(snap, "server.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	bad, _ := config("damaged")
	if err = execute(ctx, []string{"restore", "--config", bad, "--from", snap}, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Fatal("restored damaged backup")
	}
}
func TestLANRequiresTLS(t *testing.T) {
	for _, addr := range []string{":7443", "0.0.0.0:7443", "[::]:7443", "cluster.local:7443", "192.168.2.20:7443"} {
		if _, _, err := listenConfig(serverConfig{Listen: addr}); err == nil {
			t.Fatalf("insecure LAN listener %q", addr)
		}
	}
}

func TestBootstrapCanRetryAfterSeedWriteFails(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "server.json")
	data := filepath.Join(root, "state")
	b, err := json.Marshal(serverConfig{DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(config, b, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(filepath.Join(data, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := state.Store{DB: db}
	if err = store.Init(); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TRIGGER fail_seed BEFORE INSERT ON jobs BEGIN SELECT RAISE(ABORT,'test failure'); END"); err != nil {
		t.Fatal(err)
	}
	initCluster := func() error {
		return execute(context.Background(), []string{"init", "--config", config}, strings.NewReader("example-fixture-password"), &bytes.Buffer{})
	}
	if err = initCluster(); err == nil {
		t.Fatal("expected seed failure")
	}
	if _, err = db.Exec("DROP TRIGGER fail_seed"); err != nil {
		t.Fatal(err)
	}
	if err = initCluster(); err != nil {
		t.Fatalf("retry after rolled-back bootstrap: %v", err)
	}
	jobs, err := store.Jobs()
	if err != nil || len(jobs) != 1 {
		t.Fatalf("bootstrap jobs: %v, %v", jobs, err)
	}
}
