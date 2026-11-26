package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sluicework/internal/githubapp"
	"sluicework/internal/state"
)

func TestWorkstationBuildThreadRange(t *testing.T) {
	if got := (WorkstationConfig{}).defaults().BuildThreads; got != 2 {
		t.Fatalf("default build threads = %d, want 2", got)
	}
	for threads := 1; threads <= 8; threads++ {
		t.Run(strconv.Itoa(threads), func(t *testing.T) {
			w := (WorkstationConfig{BuildThreads: threads}).defaults()
			if err := w.validate(); err != nil {
				t.Fatalf("%d build threads rejected: %v", threads, err)
			}
		})
	}
	for _, threads := range []int{-1, 9, 16} {
		w := (WorkstationConfig{BuildThreads: threads}).defaults()
		if err := w.validate(); err == nil || !strings.Contains(err.Error(), "1–8 build threads") {
			t.Fatalf("invalid thread setting %d: %v", threads, err)
		}
	}
}

func TestWorkstationResourceGates(t *testing.T) {
	idle, ac := 120, true
	w := WorkstationConfig{MinIdleSeconds: &idle, RequireACPower: &ac}.defaults()
	healthy := hostResources{CPUs: 8, Load: 1, AvailableMB: 8192, IdleSeconds: 300, AC: true, ThermalOK: true}
	if reason := w.eligible(healthy); reason != "" {
		t.Fatal(reason)
	}
	for _, tc := range []struct {
		name string
		edit func(*hostResources)
		want string
	}{
		{"foreground", func(h *hostResources) { h.IdleSeconds = 1 }, "foreground activity"},
		{"battery", func(h *hostResources) { h.AC = false }, "AC power"},
		{"memory", func(h *hostResources) { h.AvailableMB = 1024 }, "memory"},
		{"CPU", func(h *hostResources) { h.Load = 7 }, "CPU capacity"},
		{"thermal", func(h *hostResources) { h.ThermalOK = false }, "thermal pressure"},
		{"unknown", func(h *hostResources) { h.AvailableMB = -1 }, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := healthy
			tc.edit(&h)
			if reason := w.eligible(h); !strings.Contains(reason, tc.want) {
				t.Fatal(reason)
			}
		})
	}
}

func TestWorkstationControllerStartsPausedAndDrainsRealProcess(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("unprivileged controller")
	}
	root := t.TempDir()
	runner := filepath.Join(root, "runner")
	dir := filepath.Join(root, "work", "workstation")
	for _, p := range []string{filepath.Join(runner, "bin"), dir} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(runner, ".sluicework-runner"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	// The fake official listener is still a real process. A pause must not kill
	// it while its assigned job is waiting to complete.
	if err := os.WriteFile(filepath.Join(runner, "bin", "Runner.Listener"), []byte("#!/bin/sh\nwhile [ ! -f finish ]; do sleep 0.02; done\n"), 0700); err != nil {
		t.Fatal(err)
	}
	var busy, exists atomic.Bool
	var grants, withdrawals, releases, refreshes atomic.Int32
	m := state.Machine{WorkerInfo: state.WorkerInfo{Name: "pilot", OS: "darwin", Arch: "arm64", Roles: []string{"ci"}}, Runner: &state.Runner{Workstation: true}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-worker" {
			t.Error("missing worker authentication")
			w.WriteHeader(401)
			return
		}
		if strings.HasSuffix(r.URL.Path, "heartbeat") {
			_ = json.NewEncoder(w).Encode(m)
			return
		}
		var body struct{ Operation string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		g := githubapp.Grant{}
		if exists.Load() {
			g.Runner = githubapp.Runner{ID: 42, Status: "online", Busy: busy.Load()}
		}
		switch body.Operation {
		case "acquire":
			grants.Add(1)
			exists.Store(true)
			busy.Store(true)
			g = githubapp.Grant{Runner: githubapp.Runner{ID: 42}, JIT: "synthetic-jit"}
		case "pause":
			withdrawals.Add(1)
		case "enable":
			refreshes.Add(1)
		case "release":
			if busy.Load() {
				t.Error("released assigned build")
				w.WriteHeader(409)
				return
			}
			releases.Add(1)
			exists.Store(false)
		}
		_ = json.NewEncoder(w).Encode(g)
	}))
	defer server.Close()
	idle, ac := 0, false
	cfg := Config{URL: server.URL, Roles: []string{"ci"}, WorkDir: filepath.Join(root, "work"), CredentialFile: filepath.Join(root, "credential.json"), Workstation: &WorkstationConfig{RunnerDir: runner, MinIdleSeconds: &idle, RequireACPower: &ac}}
	if err := atomicJSON(cfg.CredentialFile, credential{Enrollment: state.Enrollment{ID: "fixture", Credential: "synthetic-worker"}, URL: server.URL}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runWorkstationRuntime(ctx, cfg, cfg.Workstation.defaults(), dir, workstationRuntime{poll: 10 * time.Millisecond, drain: 30 * time.Millisecond, refresh: 20 * time.Millisecond, sample: func(context.Context, WorkstationConfig) (hostResources, error) {
			return hostResources{CPUs: 8, Load: 0, AvailableMB: 8192, IdleSeconds: 3600, AC: true, ThermalOK: true}, nil
		}})
	}()
	wait := func(check func() bool) {
		t.Helper()
		until := time.Now().Add(3 * time.Second)
		for !check() {
			if time.Now().After(until) {
				t.Fatal("controller did not reach expected state")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	wait(func() bool { _, err := os.Stat(filepath.Join(dir, "status.json")); return err == nil })
	if grants.Load() != 0 {
		t.Fatal("runner registered without opt-in")
	}
	if err := atomicJSON(filepath.Join(dir, "control.json"), workstationControl{EnabledUntil: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	wait(func() bool { return grants.Load() == 1 })
	// A transient server-side withdrawal must be repaired even if this local
	// process never observed the outage and still considers itself enabled.
	wait(func() bool { return refreshes.Load() > 0 })
	if err := os.WriteFile(filepath.Join(dir, "job-started"), []byte("started"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := atomicJSON(filepath.Join(dir, "control.json"), workstationControl{}); err != nil {
		t.Fatal(err)
	}
	wait(func() bool { return withdrawals.Load() > 0 })
	// Resuming remains possible while the assigned job finishes. Ctrl-C then
	// has to close the start gate itself, without a preceding pause command.
	previousRefreshes := refreshes.Load()
	if err := atomicJSON(filepath.Join(dir, "control.json"), workstationControl{EnabledUntil: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	wait(func() bool { return refreshes.Load() > previousRefreshes })
	previousWithdrawals := withdrawals.Load()
	cancel()
	wait(func() bool { return withdrawals.Load() > previousWithdrawals })
	ctrl, err := control(filepath.Join(dir, "control.json"))
	if err != nil || !ctrl.EnabledUntil.IsZero() {
		t.Fatal("shutdown left the start gate enabled", ctrl, err)
	}
	select {
	case err := <-done:
		t.Fatal("interrupted assigned job", err)
	case <-time.After(80 * time.Millisecond):
	}
	if grants.Load() != 1 || releases.Load() != 0 {
		t.Fatal("duplicated or released assigned runner")
	}
	busy.Store(false)
	if err := os.WriteFile(filepath.Join(runner, "finish"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("controller did not finish drain")
	}
	if releases.Load() != 1 {
		t.Fatal("runner lease not cleaned up")
	}
}

func TestWorkstationOptInExpiryPauseAndStaleStatus(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("workstation refuses root")
	}
	dir := t.TempDir()
	cfg := Config{WorkDir: filepath.Join(dir, "work"), CredentialFile: filepath.Join(dir, "credential"), Workstation: &WorkstationConfig{RunnerDir: filepath.Join(dir, "runner")}}
	var out bytes.Buffer
	for _, command := range []string{"workstation-enable", "workstation-pause"} {
		if err := WorkstationCommand(context.Background(), cfg, command, &out); err != nil {
			t.Fatal(err)
		}
		ctrl, err := control(filepath.Join(cfg.WorkDir, "workstation", "control.json"))
		if err != nil {
			t.Fatal(err)
		}
		if command == "workstation-enable" && (time.Until(ctrl.EnabledUntil) < 119*time.Minute || time.Until(ctrl.EnabledUntil) > 121*time.Minute) {
			t.Fatal(ctrl)
		}
		if command == "workstation-pause" && !ctrl.EnabledUntil.IsZero() {
			t.Fatal("pause did not expire opt-in")
		}
	}
	if err := WorkstationCommand(context.Background(), cfg, "workstation-check", &out); err == nil {
		t.Fatal("paused job admitted")
	}
	path := filepath.Join(cfg.WorkDir, "workstation", "status.json")
	if err := atomicJSON(path, workstationReport{State: "accepting", Updated: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := WorkstationCommand(context.Background(), cfg, "workstation-status", &out); err != nil {
		t.Fatal(err)
	}
	var report workstationReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil || report.State != "offline" {
		t.Fatal(report, err)
	}
}

func TestRunnerEnvironmentDoesNotInheritControllerCredentials(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("unprivileged runner")
	}
	dir := t.TempDir()
	runner := filepath.Join(dir, "runner")
	if err := os.MkdirAll(filepath.Join(runner, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	// The real process exercises the launch path and inherited environment.
	path := filepath.Join(runner, "bin", "Runner.Listener")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n/usr/bin/env\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_SECRET_ACCESS_KEY", "synthetic-should-not-leak")
	t.Setenv("GH_TOKEN", "synthetic-should-not-leak")
	cmd, err := startListener(WorkstationConfig{RunnerDir: runner, BuildThreads: 8}, dir, "/test/hook", "synthetic-jit")
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "runner.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "should-not-leak") || !strings.Contains(string(b), "CARGO_BUILD_JOBS=8") || !strings.Contains(string(b), "CMAKE_BUILD_PARALLEL_LEVEL=8") || !strings.Contains(string(b), "MAKEFLAGS=-j8") || !strings.Contains(string(b), "ACTIONS_RUNNER_HOOK_JOB_STARTED=/test/hook") {
		t.Fatal("incorrect runner environment")
	}
}

func TestWorkstationStartHookRequiresLiveGitHubApproval(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("unprivileged runner")
	}
	root := t.TempDir()
	var checks atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "heartbeat") {
			_ = json.NewEncoder(w).Encode(state.Machine{Runner: &state.Runner{Workstation: true}})
			return
		}
		checks.Add(1)
		w.WriteHeader(503)
	}))
	defer server.Close()
	cfg := Config{URL: server.URL, CredentialFile: filepath.Join(root, "credential.json"), WorkDir: filepath.Join(root, "work"), Workstation: &WorkstationConfig{RunnerDir: filepath.Join(root, "runner")}}
	if err := atomicJSON(cfg.CredentialFile, credential{Enrollment: state.Enrollment{ID: "fixture", Credential: "synthetic-worker"}, URL: server.URL}); err != nil {
		t.Fatal(err)
	}
	if err := WorkstationCommand(context.Background(), cfg, "workstation-enable", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	err := WorkstationCommand(context.Background(), cfg, "workstation-check", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "GitHub connection") || checks.Load() != 1 {
		t.Fatal("admitted build without App connection", err)
	}
}
