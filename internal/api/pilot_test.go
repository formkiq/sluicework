package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sluicework/internal/api"
	"sluicework/internal/auth"
	"sluicework/internal/provider"
	"sluicework/internal/schedule"
	"sluicework/internal/state"
	"sluicework/internal/worker"
)

// TestFleetSummary uses the actual worker enrollment, polling, lease and input paths.
// Only the inference runtime is replaced, so the test runs without a model download.
func TestFleetSummary(t *testing.T) {
	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "cluster.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := &state.Store{DB: db, LeaseTTL: 15 * time.Second, OfflineAfter: 30 * time.Second, LogLimit: 64 << 10}
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	users, err := auth.New(db)
	if err != nil {
		t.Fatal(err)
	}
	const password = "local-test-password"
	if err := users.SetUser("admin", "admin", password, true); err != nil {
		t.Fatal(err)
	}
	if err := users.SetUser("operator", "operator", password, false); err != nil {
		t.Fatal(err)
	}
	broker, err := provider.Open(db, filepath.Join(dir, "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	schedules := schedule.New(db, store)
	if err := schedules.Init(); err != nil {
		t.Fatal(err)
	}
	server := api.Server{State: store, Auth: users, Provider: broker, Schedule: schedules}
	cluster := httptest.NewTLSServer(server.Handler(http.NotFoundHandler()))
	defer cluster.Close()

	request := func(client *http.Client, method, path string, body any, want int, out any) {
		t.Helper()
		var in io.Reader
		if body != nil {
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			in = bytes.NewReader(encoded)
		}
		req, err := http.NewRequest(method, cluster.URL+"/api/v1"+path, in)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != want {
			msg, _ := io.ReadAll(res.Body)
			t.Fatalf("%s %s returned %d: %s", method, path, res.StatusCode, msg)
		}
		if out != nil {
			if err := json.NewDecoder(res.Body).Decode(out); err != nil {
				t.Fatal(err)
			}
		}
	}
	login := func(name string) *http.Client {
		t.Helper()
		jar, _ := cookiejar.New(nil)
		client := &http.Client{Transport: cluster.Client().Transport, Jar: jar, Timeout: 3 * time.Second}
		request(client, "POST", "/login", map[string]string{"username": name, "password": password}, 200, nil)
		return client
	}
	admin, operator := login("admin"), login("operator")
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cluster.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	var workers []worker.Config
	for _, name := range []string{"orchestrator-worker", "summary-worker"} {
		var ticket struct{ Token string }
		request(admin, "POST", "/enrollment-tokens", map[string]any{}, 200, &ticket)
		root := filepath.Join(dir, name)
		inputs := filepath.Join(root, "inputs")
		if err := os.MkdirAll(inputs, 0700); err != nil {
			t.Fatal(err)
		}
		cfg := worker.Config{Name: name, Version: "pilot", URL: cluster.URL, CAFile: ca, CredentialFile: filepath.Join(root, "credential"), WorkDir: filepath.Join(root, "work"), InputRoot: inputs, Roles: []string{"agent"}}
		if err := worker.Join(t.Context(), cfg, ticket.Token); err != nil {
			t.Fatal(err)
		}
		workers = append(workers, cfg)
	}
	var machines []state.Machine
	request(admin, "GET", "/machines", nil, 200, &machines)
	var designated string
	for _, m := range machines {
		if m.Name == workers[1].Name {
			designated = m.ID
		}
	}
	if len(machines) != 2 || designated == "" {
		t.Fatalf("unexpected enrollment %+v", machines)
	}
	labels := []string{"summaries"}
	request(admin, "PATCH", "/machines/"+designated, state.MachinePatch{Labels: &labels}, 200, nil)
	base := filepath.Join("..", "..", "examples", "local-summary")
	fixture, err := os.ReadFile(filepath.Join(base, "failures.txt"))
	if err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(workers[1].InputRoot, "failures.txt")
	if err := os.WriteFile(report, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	var offline atomic.Bool
	var invocations atomic.Int32
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/api/tags" {
			io.WriteString(w, `{"models":[{"name":"qwen2.5:3b"}]}`)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/api/generate" {
			http.NotFound(w, r)
			return
		}
		invocations.Add(1)
		if offline.Load() {
			http.Error(w, "runtime failure details", 503)
			return
		}
		var body struct{ Prompt, Model string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "qwen2.5:3b" || !strings.HasSuffix(body.Prompt, string(fixture)) {
			http.Error(w, "unexpected model or selected input", 400)
			return
		}
		io.WriteString(w, `{"done":true,"response":"Two cache tests returned stale values. Export lost the accented filename.","prompt_eval_count":90,"eval_count":16}`)
	}))
	defer runtime.Close()
	connInput := provider.Config{Name: "Pilot inference", Kind: "ollama", Endpoint: runtime.URL, Models: []string{"qwen2.5:3b"}}
	request(operator, "POST", "/connections", connInput, 403, nil)
	var conn provider.Connection
	request(admin, "POST", "/connections", connInput, 200, &conn)
	request(admin, "POST", "/connections/"+conn.ID+"/test", map[string]any{}, 200, &conn)
	if conn.Health != "healthy" {
		t.Fatal(conn)
	}
	raw, err := os.ReadFile(filepath.Join(base, "template.json"))
	if err != nil {
		t.Fatal(err)
	}
	var template state.Job
	if err := json.Unmarshal(raw, &template); err != nil {
		t.Fatal(err)
	}
	template.Connection = conn.ID
	template.Inputs[0].Paths = []string{report}
	var job state.Job
	request(admin, "POST", "/jobs", template, 200, &job)
	queue := func(path string, want int) state.Run {
		t.Helper()
		var run state.Run
		var out any
		if want == 200 {
			out = &run
		}
		request(operator, "POST", "/jobs/"+job.ID+"/runs", map[string]any{"inputs": map[string]string{"report": path}}, want, out)
		return run
	}
	queue(filepath.Join(dir, "unapproved.txt"), 400)
	initial := queue(report, 200)
	var plan schedule.Schedule
	request(admin, "POST", "/schedules", schedule.Spec{Name: "Daily failures", JobID: job.ID, Cron: "0 9 * * 1-5", Timezone: "America/Winnipeg", Inputs: map[string]string{"report": report}}, 200, &plan)
	if plan.NextRun == nil {
		t.Fatal("schedule lacks next run")
	}
	ctx, stop := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	workerErr := make(chan error, len(workers))
	defer func() { stop(); wg.Wait() }()
	for _, cfg := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := worker.Run(ctx, cfg); e != nil {
				workerErr <- e
			}
		}()
	}
	waitRun := func(id, want string) state.Run {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			var run state.Run
			request(operator, "GET", "/runs/"+id, nil, 200, &run)
			if run.State == want {
				return run
			}
			if run.State != "queued" && run.State != "running" {
				t.Fatalf("run ended unexpectedly: %+v", run)
			}
			select {
			case e := <-workerErr:
				t.Fatal(e)
			case <-time.After(30 * time.Millisecond):
			}
		}
		t.Fatalf("run %s did not reach %s", id, want)
		return state.Run{}
	}
	first := waitRun(initial.ID, "succeeded")
	if len(first.Attempts) != 1 || first.Attempts[0].MachineID != designated || !strings.Contains(first.Attempts[0].Output, "stale values") {
		t.Fatal(first)
	}
	var metrics []provider.Call
	request(operator, "GET", "/runs/"+first.ID+"/calls", nil, 200, &metrics)
	if len(metrics) != 1 || *metrics[0].OutputTokens != 16 || metrics[0].ConnectionID != conn.ID {
		t.Fatal(metrics)
	}
	offline.Store(true)
	second := queue(report, 200)
	waitRun(second.ID, "failed")
	request(operator, "GET", "/runs/"+second.ID+"/calls", nil, 200, &metrics)
	if len(metrics) != 1 || metrics[0].Error != "provider returned HTTP 503" {
		t.Fatal(metrics)
	}
	offline.Store(false)
	request(operator, "POST", "/runs/"+second.ID+"/retry", map[string]any{}, 200, nil)
	retried := waitRun(second.ID, "succeeded")
	if len(retried.Attempts) != 2 || retried.Attempts[0].State != "failed" || invocations.Load() != 3 {
		t.Fatalf("unexpected retry history: %v, %d provider calls", retried.Attempts, invocations.Load())
	}
	drained := true
	request(admin, "PATCH", "/machines/"+designated, state.MachinePatch{Draining: &drained}, 200, nil)
	pending := queue(report, 200)
	for _, m := range machines {
		claim, err := store.Claim(m.ID)
		if err != nil || claim != nil {
			t.Fatalf("drained/labeled job %s claimed by %s: %v, %v", pending.ID, m.ID, claim, err)
		}
	}
}
