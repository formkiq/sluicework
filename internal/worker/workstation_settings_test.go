package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sluicework/internal/githubapp"
	"sluicework/internal/state"
)

func TestAdminSettingsWaitForAssignedBuildThenRefreshIdleRunner(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("unprivileged controller")
	}
	root := t.TempDir()
	runner, dir := filepath.Join(root, "runner"), filepath.Join(root, "work", "workstation")
	for _, p := range []string{filepath.Join(runner, "bin"), dir} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(runner, ".sluicework-runner"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$CARGO_BUILD_JOBS\" >> launches\nwhile [ ! -f \"finish-$CARGO_BUILD_JOBS\" ]; do sleep 0.02; done\n"
	if err := os.WriteFile(filepath.Join(runner, "bin", "Runner.Listener"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	var policy atomic.Pointer[state.WorkstationSettings]
	var busy, exists atomic.Bool
	var grants, pauses, effective atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-worker" {
			w.WriteHeader(401)
			return
		}
		if strings.HasSuffix(r.URL.Path, "heartbeat") {
			var info state.WorkerInfo
			if err := json.NewDecoder(r.Body).Decode(&info); err != nil {
				t.Error(err)
			}
			if info.Workstation != nil {
				effective.Store(int32(info.Workstation.Effective.BuildThreads))
			}
			_ = json.NewEncoder(w).Encode(state.Machine{Runner: &state.Runner{Workstation: true, Settings: policy.Load()}})
			return
		}
		var body struct{ Operation string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		g := githubapp.Grant{}
		if exists.Load() {
			g.Runner = githubapp.Runner{ID: 42, Busy: busy.Load()}
		}
		switch body.Operation {
		case "acquire":
			n := grants.Add(1)
			exists.Store(true)
			busy.Store(n == 1)
			g = githubapp.Grant{Runner: githubapp.Runner{ID: 42}, JIT: "synthetic"}
		case "pause":
			pauses.Add(1)
		case "release":
			if busy.Load() {
				t.Error("released assigned runner")
				w.WriteHeader(409)
				return
			}
			exists.Store(false)
		}
		_ = json.NewEncoder(w).Encode(g)
	}))
	defer server.Close()
	idle, ac := 0, false
	cfg := Config{URL: server.URL, Roles: []string{"ci"}, WorkDir: filepath.Join(root, "work"), CredentialFile: filepath.Join(root, "credential"), Workstation: &WorkstationConfig{RunnerDir: runner, BuildThreads: 4, MinIdleSeconds: &idle, RequireACPower: &ac}}
	if err := atomicJSON(cfg.CredentialFile, credential{Enrollment: state.Enrollment{ID: "fixture", Credential: "synthetic-worker"}, URL: server.URL}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runWorkstationRuntime(ctx, cfg, cfg.Workstation.defaults(), dir, workstationRuntime{poll: 10 * time.Millisecond, drain: 30 * time.Millisecond, refresh: time.Hour, sample: func(context.Context, WorkstationConfig) (hostResources, error) {
			return hostResources{CPUs: 18, Load: 0, AvailableMB: 16384, ThermalOK: true, AC: true}, nil
		}})
	}()
	wait := func(check func() bool) {
		t.Helper()
		end := time.Now().Add(5 * time.Second)
		for !check() {
			if time.Now().After(end) {
				t.Fatal("controller did not apply expected settings")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	readReport := func() workstationReport {
		var report workstationReport
		b, _ := os.ReadFile(filepath.Join(dir, "status.json"))
		_ = json.Unmarshal(b, &report)
		return report
	}
	wait(func() bool { return readReport().State == "paused" })
	expiry := time.Now().Add(time.Hour)
	if err := atomicJSON(filepath.Join(dir, "control.json"), workstationControl{EnabledUntil: expiry}); err != nil {
		t.Fatal(err)
	}
	wait(func() bool { b, _ := os.ReadFile(filepath.Join(runner, "launches")); return string(b) == "4\n" })
	if err := os.WriteFile(filepath.Join(dir, "job-started"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	requested := cfg.Workstation.defaults().settings()
	requested.BuildThreads = 8
	requested.MinAvailableMemoryMB = 8192
	requested.EnableMinutes = 30
	policy.Store(&requested)
	wait(func() bool {
		r := readReport()
		return pauses.Load() > 0 && r.DesiredSettings.BuildThreads == 8 && r.State == "draining"
	})
	if effective.Load() != 4 || grants.Load() != 1 {
		t.Fatal("active build was changed or duplicated")
	}
	time.Sleep(80 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatal("assigned build interrupted", err)
	default:
	}
	ctrl, err := control(filepath.Join(dir, "control.json"))
	if err != nil || !ctrl.EnabledUntil.Equal(expiry) {
		t.Fatal("settings edit extended or reset local opt-in", ctrl, err)
	}
	busy.Store(false)
	if err := os.WriteFile(filepath.Join(runner, "finish-4"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	wait(func() bool {
		b, _ := os.ReadFile(filepath.Join(runner, "launches"))
		return string(b) == "4\n8\n" && effective.Load() == 8
	})
	if got := cachedSettings(filepath.Join(dir, "settings.json"), cfg.Workstation.defaults()); got.EnableMinutes != 30 || got.BuildThreads != 8 {
		t.Fatal("local enable did not inherit managed settings", got)
	}
	// Clearing admin management also recycles an idle listener safely and
	// returns to the machine's original profile without a controller restart.
	if err := os.Remove(filepath.Join(runner, "finish-4")); err != nil {
		t.Fatal(err)
	}
	policy.Store(nil)
	wait(func() bool {
		b, _ := os.ReadFile(filepath.Join(runner, "launches"))
		return string(b) == "4\n8\n4\n" && effective.Load() == 4
	})
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("controller failed to drain")
	}
}
