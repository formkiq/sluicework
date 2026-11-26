package state

import (
	"errors"
	"testing"
)

func TestCentralWorkstationSettingsPersistAndLegacyWorkersRequireUpgrade(t *testing.T) {
	s, _ := testStore(t)
	local := WorkstationSettings{MaxLoadPerCPU: .5, MinAvailableMemoryMB: 4096, BuildThreads: 4, EnableMinutes: 120, RequireACPower: true}
	info := WorkerInfo{Name: "Mac", OS: "darwin", Arch: "arm64", Roles: []string{"ci"}}
	token, _ := s.EnrollToken("admin")
	e, err := s.Enroll(token, info)
	if err != nil {
		t.Fatal(err)
	}
	requested := local
	requested.BuildThreads = 8
	runner := Runner{Workstation: true, Name: "Mac", Scope: "team/repo", Labels: []string{"self-hosted", "macOS", "ARM64"}, Settings: &requested}
	if err = s.UpdateMachine("admin", e.ID, MachinePatch{Runner: &runner}); !errors.Is(err, ErrInvalid) {
		t.Fatal("legacy worker accepted central settings", err)
	}
	info.Workstation = &WorkstationStatus{SettingsVersion: 1, Local: local, Effective: local}
	if err = s.Heartbeat(e.ID, info); err != nil {
		t.Fatal(err)
	}
	if err = s.UpdateMachine("admin", e.ID, MachinePatch{Runner: &runner}); err != nil {
		t.Fatal(err)
	}
	m, err := s.Machine(e.ID)
	if err != nil || m.Runner.Settings.BuildThreads != 8 || m.Workstation.Effective.BuildThreads != 4 {
		t.Fatal("requested/effective settings lost", m, err)
	}
	requested.BuildThreads = 9
	if err = s.UpdateMachine("admin", e.ID, MachinePatch{Runner: &runner}); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid settings accepted", err)
	}
	m, _ = s.Machine(e.ID)
	if m.Runner.Settings.BuildThreads != 8 {
		t.Fatal("invalid patch changed stored settings")
	}
	// A temporarily older controller must not erase an existing profile just
	// because an administrator changes unrelated runner details.
	info.Workstation = nil
	if err = s.Heartbeat(e.ID, info); err != nil {
		t.Fatal(err)
	}
	requested.BuildThreads = 8
	runner.Name = "Renamed Mac"
	if err = s.UpdateMachine("admin", e.ID, MachinePatch{Runner: &runner}); err != nil {
		t.Fatal("unchanged managed settings rejected", err)
	}
	runner.Settings = nil
	if err = s.UpdateMachine("admin", e.ID, MachinePatch{Runner: &runner}); err != nil {
		t.Fatal(err)
	}
	m, _ = s.Machine(e.ID)
	if m.Runner.Settings != nil {
		t.Fatal("installation defaults were not restored")
	}
}

func TestWorkstationSettingsRejectUnsafeValuesAndUnsupportedIdleGate(t *testing.T) {
	valid := WorkstationSettings{MaxLoadPerCPU: .5, MinAvailableMemoryMB: 4096, BuildThreads: 4, EnableMinutes: 120}
	for _, edit := range []func(*WorkstationSettings){
		func(w *WorkstationSettings) { w.BuildThreads = 0 },
		func(w *WorkstationSettings) { w.BuildThreads = 9 },
		func(w *WorkstationSettings) { w.MaxLoadPerCPU = 0 },
		func(w *WorkstationSettings) { w.MaxLoadPerCPU = 1.1 },
		func(w *WorkstationSettings) { w.MinAvailableMemoryMB = 511 },
		func(w *WorkstationSettings) { w.EnableMinutes = 1441 },
		func(w *WorkstationSettings) { w.MinIdleSeconds = -1 },
	} {
		w := valid
		edit(&w)
		if err := w.Validate("darwin"); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid setting accepted", w, err)
		}
	}
	valid.MinIdleSeconds = 120
	if valid.Validate("darwin") != nil || !errors.Is(valid.Validate("linux"), ErrInvalid) {
		t.Fatal("platform idle policy was not enforced")
	}
}
