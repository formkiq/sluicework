package api_test

import (
	"testing"
	"time"

	"sluicework/internal/state"
)

func TestOnlyMainAdministratorCanManageRemoteWorkstationSettings(t *testing.T) {
	c := setup(t)
	var token struct{ Token string }
	c.do("POST", "/enrollment-tokens", nil, c.admin, 200, &token)
	local := state.WorkstationSettings{MaxLoadPerCPU: .5, MinAvailableMemoryMB: 4096, BuildThreads: 4, EnableMinutes: 120}
	info := state.WorkerInfo{Name: "Remote Mac", OS: "darwin", Arch: "arm64", Roles: []string{"ci"}, Workstation: &state.WorkstationStatus{SettingsVersion: 1, Local: local, Effective: local}}
	var enrollment state.Enrollment
	c.do("POST", "/worker/enroll", map[string]any{"token": token.Token, "info": info}, identity{}, 200, &enrollment)
	worker := identity{bearer: enrollment.Credential}
	requested := local
	requested.BuildThreads = 8
	runner := state.Runner{Workstation: true, Name: "Remote Mac", Scope: "team/repo", Labels: []string{"self-hosted", "macOS", "ARM64"}, Settings: &requested}
	patch := state.MachinePatch{Runner: &runner}
	path := "/machines/" + enrollment.ID
	c.do("PATCH", path, patch, c.operator, 403, nil)
	c.do("PATCH", path, patch, worker, 401, nil)
	var machine state.Machine
	c.do("PATCH", path, patch, c.admin, 200, &machine)
	if machine.Runner.Settings.BuildThreads != 8 || machine.Workstation.Effective.BuildThreads != 4 {
		t.Fatal("settings applied without worker acknowledgement")
	}
	c.do("POST", "/worker/heartbeat", info, worker, 200, &machine)
	if machine.Runner.Settings.BuildThreads != 8 {
		t.Fatal("worker did not receive requested settings")
	}
	// An older client updating labels/name must not clear central settings.
	legacy := map[string]any{"workstation": true, "name": "Renamed Mac", "scope": "team/repo", "labels": runner.Labels, "service": ""}
	c.do("PATCH", path, map[string]any{"runner": legacy}, c.admin, 200, &machine)
	if machine.Runner.Settings == nil || machine.Runner.Settings.BuildThreads != 8 {
		t.Fatal("older client erased central settings")
	}
	requested.BuildThreads = 9
	c.do("PATCH", path, patch, c.admin, 400, nil)
	info.Workstation.Effective = *machine.Runner.Settings
	c.do("POST", "/worker/heartbeat", info, worker, 200, &machine)
	if machine.Workstation.Effective.BuildThreads != 8 {
		t.Fatal("worker effective settings not reported")
	}
	legacy["settings"] = nil
	machine = state.Machine{}
	c.do("PATCH", path, map[string]any{"runner": legacy}, c.admin, 200, &machine)
	if machine.Runner.Settings != nil {
		t.Fatal("explicit reset did not restore installation defaults")
	}
	c.do("PATCH", path, map[string]any{"unexpected": true}, c.admin, 400, nil)
}

func TestMainAdminCanRequestCapacityOnlyForCompatibleWorkstation(t *testing.T) {
	c := setup(t)
	var token struct{ Token string }
	c.do("POST", "/enrollment-tokens", nil, c.admin, 200, &token)
	local := state.WorkstationSettings{MaxLoadPerCPU: .5, MinAvailableMemoryMB: 4096, BuildThreads: 4, EnableMinutes: 120}
	info := state.WorkerInfo{Name: "Mac", OS: "darwin", Arch: "arm64", Roles: []string{"ci"}, Workstation: &state.WorkstationStatus{SettingsVersion: 1, ControlVersion: 1, Local: local, Effective: local}}
	var worker state.Enrollment
	c.do("POST", "/worker/enroll", map[string]any{"token": token.Token, "info": info}, identity{}, 200, &worker)
	runner := state.Runner{Workstation: true, Name: "Mac", Scope: "team/repo", Labels: []string{"self-hosted", "macOS", "ARM64"}}
	c.do("PATCH", "/machines/"+worker.ID, state.MachinePatch{Runner: &runner}, c.admin, 200, nil)
	path := "/machines/" + worker.ID + "/workstation/availability"
	body := map[string]any{"operation": "enable", "minutes": 60}
	c.do("POST", path, body, c.operator, 403, nil)
	c.do("POST", path, body, identity{bearer: worker.Credential}, 401, nil)
	var machine state.Machine
	c.do("POST", path, body, c.admin, 200, &machine)
	if machine.Runner.Control == nil || machine.Runner.Control.ID == "" || time.Until(machine.Runner.Control.EnabledUntil) < 59*time.Minute {
		t.Fatal("capacity request not recorded")
	}
	c.do("POST", path, map[string]any{"operation": "enable", "minutes": 1441}, c.admin, 400, nil)
	c.do("POST", path, map[string]any{"operation": "pause"}, c.admin, 200, &machine)
	if !machine.Runner.Control.EnabledUntil.IsZero() {
		t.Fatal("pause retained offer expiry")
	}
	c.do("PATCH", "/machines/"+worker.ID, map[string]bool{"draining": true}, c.admin, 200, nil)
	c.do("POST", path, body, c.admin, 409, nil)
}
