package worker

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sluicework/internal/state"
)

func TestJoinAndRun(t *testing.T) {
	t.Setenv("SLUICE_TEST_PRIVATE", "secret-value")
	dir := t.TempDir()
	source := filepath.Join(dir, "collected.txt")
	if err := os.WriteFile(source, []byte("previous failures"), 0600); err != nil {
		t.Fatal(err)
	}
	claim := state.Claim{RunID: "run", AttemptID: "active", Inputs: map[string]string{"report": source, "suffix": "{{report}}"}, Job: state.Job{
		Type: "script", Command: "/bin/sh",
		Args:    []string{"-c", `test -z "$SLUICE_TEST_PRIVATE" && test "$1" != "$3" && test "$PWD" -ef "$HOME/work" && cat "$1" && printf ' %s' "$2"`, "approved", "{{report}}", "{{suffix}}", source},
		Inputs:  []state.Input{{Name: "report", Paths: []string{source}, Required: true}, {Name: "suffix", Required: true}},
		WorkDir: "work", TimeoutSeconds: 4, OutputLimit: 8,
	}}
	var claimed atomic.Bool
	var logs strings.Builder
	var logMu sync.Mutex
	done := make(chan state.Completion, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method %s", r.Method)
		}
		if r.URL.Path != "/api/v1/worker/enroll" && r.Header.Get("Authorization") != "Bearer worker-private-key" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/api/v1/worker/enroll":
			var req struct {
				Token string
				Info  state.WorkerInfo
			}
			if json.NewDecoder(r.Body).Decode(&req) != nil || req.Token != "one-use" || req.Info.Name != "mac-2" {
				w.WriteHeader(400)
				return
			}
			json.NewEncoder(w).Encode(state.Enrollment{ID: "second", Credential: "worker-private-key"})
		case "/api/v1/worker/heartbeat":
			json.NewEncoder(w).Encode(state.Machine{Slots: 1})
		case "/api/v1/worker/claim":
			if claimed.Swap(true) {
				w.WriteHeader(204)
				return
			}
			claim.LeaseUntil = time.Now().Add(3 * time.Second)
			json.NewEncoder(w).Encode(claim)
		case "/api/v1/worker/attempts/active/logs":
			var line struct{ Text string }
			if err := json.NewDecoder(r.Body).Decode(&line); err != nil {
				t.Error(err)
			}
			logMu.Lock()
			logs.WriteString(line.Text)
			logMu.Unlock()
			w.WriteHeader(204)
		case "/api/v1/worker/attempts/active/renew":
			json.NewEncoder(w).Encode(state.Renewal{LeaseUntil: time.Now().Add(3 * time.Second)})
		case "/api/v1/worker/attempts/active/complete":
			var end state.Completion
			if err := json.NewDecoder(r.Body).Decode(&end); err != nil {
				t.Error(err)
			}
			done <- end
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	caFile := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caFile, ca, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{URL: server.URL, Name: "mac-2", CredentialFile: filepath.Join(dir, "worker.json"), WorkDir: filepath.Join(dir, "scratch"), InputRoot: dir}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := Join(ctx, cfg, "one-use"); err == nil {
		t.Fatal("untrusted server accepted")
	}
	cfg.CAFile = caFile
	if err := Join(ctx, cfg, "one-use"); err != nil {
		t.Fatal(err)
	}
	if err := Join(ctx, cfg, "one-use"); err == nil {
		t.Fatal("overwrote worker credential")
	}
	st, err := os.Stat(cfg.CredentialFile)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("worker credential permissions", err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- Run(ctx, cfg) }()
	select {
	case end := <-done:
		if end.State != "succeeded" || end.Output != "previous" || !end.OutputTruncated {
			t.Errorf("completion %+v", end)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for outcome")
	case err := <-stopped:
		t.Fatal("worker stopped early", err)
	}
	cancel()
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	logMu.Lock()
	defer logMu.Unlock()
	if logs.String() != "previous failures {{report}}" {
		t.Errorf("logs %q", logs.String())
	}
	entries, err := os.ReadDir(cfg.WorkDir)
	if err != nil || len(entries) != 0 {
		t.Fatal("worker left scratch files", err)
	}
}

func TestLeaseStopsProcessGroup(t *testing.T) {
	for _, mode := range []string{"cancel", "expired", "stale"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			child := make(chan int, 1)
			started := make(chan struct{})
			var startedOnce sync.Once
			var shortened atomic.Bool
			ends := make(chan state.Completion, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/logs"):
					var text struct{ Text string }
					if err := json.NewDecoder(r.Body).Decode(&text); err != nil {
						t.Error(err)
					}
					pid, err := strconv.Atoi(strings.TrimSpace(text.Text))
					if err == nil && runtime.GOOS == "linux" {
						pid = hostPIDForAttempt(dir, pid)
					}
					if err != nil || pid <= 0 {
						t.Error("child PID unavailable", err)
					} else {
						child <- pid
						startedOnce.Do(func() { close(started) })
					}
					w.WriteHeader(204)
				case strings.HasSuffix(r.URL.Path, "/renew"):
					// Trigger lease loss only after the subprocess exists. Sandbox
					// startup on slower native CI machines can exceed 300 ms.
					select {
					case <-started:
					case <-r.Context().Done():
						return
					}
					switch mode {
					case "cancel":
						json.NewEncoder(w).Encode(state.Renewal{CancelRequested: true, LeaseUntil: time.Now().Add(time.Second)})
					case "expired":
						if !shortened.Swap(true) {
							json.NewEncoder(w).Encode(state.Renewal{LeaseUntil: time.Now().Add(200 * time.Millisecond)})
							return
						}
						io.Copy(io.Discard, r.Body)
						select {
						case <-r.Context().Done():
						case <-time.After(time.Second):
						}
					case "stale":
						w.WriteHeader(http.StatusConflict)
					}
				case strings.HasSuffix(r.URL.Path, "/complete"):
					var end state.Completion
					json.NewDecoder(r.Body).Decode(&end)
					ends <- end
					w.WriteHeader(204)
				default:
					w.WriteHeader(204)
				}
			}))
			defer server.Close()
			c, err := newClient(Config{URL: server.URL, WorkDir: dir, CredentialFile: filepath.Join(dir, "credential")})
			if err != nil {
				t.Fatal(err)
			}
			ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			claim := state.Claim{AttemptID: "leased", LeaseUntil: time.Now().Add(5 * time.Second), Job: state.Job{
				Type: "script", Command: "/bin/sh", Args: []string{"-c", `sleep 30 & printf '%s\n' "$!"; wait`},
				TimeoutSeconds: 8,
			}}
			c.attempt(ctx, claim)
			var pid int
			select {
			case pid = <-child:
			default:
				t.Fatal("script did not start its child")
			}
			select {
			case end := <-ends:
				if mode != "cancel" || end.State != "cancelled" {
					t.Errorf("unexpected completion %+v", end)
				}
			default:
				if mode == "cancel" {
					t.Fatal("no cancellation confirmation")
				}
			}
			// Orphans may remain as zombies until init reaps them.
			deadline := time.Now().Add(time.Second)
			for {
				output, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
				status := strings.TrimSpace(string(output))
				if status == "" || strings.HasPrefix(status, "Z") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("child survived lease termination in state %q", status)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

// Child PIDs printed inside bubblewrap belong to its PID namespace. Map the
// live child back to the host using both its unique attempt cwd and NSpid, so
// cancellation checks never inspect an unrelated host process with that PID.
func hostPIDForAttempt(root string, namespacePID int) int {
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cwd, err := os.Readlink("/proc/" + entry.Name() + "/cwd")
		if err != nil || !strings.HasPrefix(cwd, root+"/attempt-") {
			continue
		}
		status, _ := os.ReadFile("/proc/" + entry.Name() + "/status")
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "NSpid:") {
				fields := strings.Fields(line)
				if fields[len(fields)-1] == strconv.Itoa(namespacePID) {
					return pid
				}
			}
		}
	}
	return 0
}

func TestInferenceForwardsOnlyDeclaredFiles(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "reports")
	os.Mkdir(root, 0700)
	allowed := filepath.Join(root, "latest")
	secret := filepath.Join(base, "credential")
	os.WriteFile(allowed, []byte("report"), 0600)
	os.WriteFile(secret, []byte("secret"), 0600)
	symlink := filepath.Join(root, "outside")
	os.Symlink(secret, symlink)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/worker/attempts/active/infer" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var req struct{ Inputs map[string]string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Inputs) != 1 || req.Inputs["file"] != "report" {
			t.Errorf("unexpected infer payload %+v %v", req, err)
		}
		json.NewEncoder(w).Encode(map[string]any{"text": "summary", "truncated": true})
	}))
	defer server.Close()
	c, err := newClient(Config{URL: server.URL, WorkDir: base, InputRoot: root, CredentialFile: secret})
	if err != nil {
		t.Fatal(err)
	}
	claim := state.Claim{AttemptID: "active", Job: state.Job{Type: "ai", Inputs: []state.Input{{Name: "file", Paths: []string{allowed, symlink}}, {Name: "literal"}}}, Inputs: map[string]string{"file": allowed, "literal": "literal"}}
	result := c.execute(context.Background(), claim)
	if result.State != "succeeded" || result.Output != "summary" || !result.OutputTruncated {
		t.Errorf("result %+v", result)
	}
	for _, path := range []string{secret, symlink} {
		claim.Inputs["file"] = path
		result = c.execute(context.Background(), claim)
		if result.State != "failed" {
			t.Errorf("invalid selection %q accepted", path)
		}
	}
}

func TestTransportAndOrigins(t *testing.T) {
	for _, origin := range []string{"http://192.168.2.3", "http://localhost.other", "https://admin:key@localhost", "http://127.0.0.1/path"} {
		if _, err := newClient(Config{URL: origin, CredentialFile: "secret"}); err == nil {
			t.Errorf("accepted %s", origin)
		}
	}
	for _, origin := range []string{"http://127.0.0.1:8090", "http://[::1]:8080", "https://build.local"} {
		if _, err := newClient(Config{URL: origin, CredentialFile: "secret"}); err != nil {
			t.Error(err)
		}
	}
}

func TestRunnerReportRequiresFreshMatchingRegularFile(t *testing.T) {
	const target = "gui/90876/actions.runner.sluicework.test"
	file := filepath.Join(t.TempDir(), "status.json")
	if err := os.WriteFile(file, []byte(`{"service":"gui/90876/actions.runner.sluicework.test","status":"running"}`), 0644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if got := runnerStatus(ctx, target, file); got != "running" {
		t.Fatalf("expected runner report, got %q", got)
	}
	if got := runnerStatus(ctx, target+"x", file); got == "running" {
		t.Fatal("accepted report for different runner")
	}
	past := time.Now().Add(-3 * time.Minute)
	if err := os.Chtimes(file, past, past); err != nil {
		t.Fatal(err)
	}
	if got := runnerStatus(ctx, target, file); got == "running" {
		t.Fatal("accepted stale runner report")
	}
}

func TestBoundArgumentExpansion(t *testing.T) {
	if _, err := renderArgs([]string{strings.Repeat("{{value}}", 1024)}, map[string]string{"value": strings.Repeat("x", 128<<10)}); err == nil {
		t.Fatal("accepted excessively expanded arguments")
	}
}

func TestLogUploadKeepsSequenceAndUTF8(t *testing.T) {
	var mu sync.Mutex
	var ids []int64
	var texts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var chunk struct {
			Text     string
			Sequence int64
		}
		if err := json.NewDecoder(r.Body).Decode(&chunk); err != nil {
			t.Error(err)
		}
		mu.Lock()
		defer mu.Unlock()
		ids = append(ids, chunk.Sequence)
		texts = append(texts, chunk.Text)
		if len(ids) == 1 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	c, err := newClient(Config{URL: server.URL, CredentialFile: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	w := &logStream{writer: &logWriter{c: c, ctx: context.Background(), attempt: "leased"}}
	// Split the first rune across pipe reads and the second at an HTTP chunk boundary.
	text := []byte("☃" + strings.Repeat("x", 16380) + "☃!")
	for _, part := range [][]byte{text[:2], text[2:]} {
		if _, err := w.Write(part); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.finish(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ids) != 3 || ids[0] != 1 || ids[1] != 1 || ids[2] != 2 || texts[0] != texts[1] || strings.Join(texts[1:], "") != string(text) {
		t.Fatalf("wrong replay or character boundary, sequence %v", ids)
	}
}
