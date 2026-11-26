package worker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sluicework/internal/state"
)

func TestCapacityRequestsDoNotReplayAfterLocalPauseOrRestart(t *testing.T) {
	dir := t.TempDir()
	session := time.Now().Add(-time.Minute)
	request := state.WorkstationControlRequest{ID: "one", Operation: "enable", IssuedAt: time.Now().Add(-time.Second), EnabledUntil: time.Now().Add(time.Hour)}
	current, err := applyControl(dir, request, session)
	if err != nil || current.EnabledUntil.IsZero() {
		t.Fatal("admin offer not applied", current, err)
	}
	_, err = updateControl(dir, func(c *workstationControl) { c.EnabledUntil = time.Time{}; c.ChangedAt = time.Now() })
	if err != nil {
		t.Fatal(err)
	}
	current, err = applyControl(dir, request, session)
	if err != nil || !current.EnabledUntil.IsZero() {
		t.Fatal("admin offer replayed after local pause", current, err)
	}
	request.ID = "delayed"
	current, err = applyControl(dir, request, session)
	if err != nil || !current.EnabledUntil.IsZero() {
		t.Fatal("delayed admin request overrode a newer local pause", current, err)
	}
	request.ID = "two"
	request.IssuedAt = time.Now().Add(-time.Millisecond)
	current, err = applyControl(dir, request, time.Now())
	if err != nil || !current.EnabledUntil.IsZero() || current.AppliedControl != "two" {
		t.Fatal("old offer enabled a new controller session", current, err)
	}
	request.ID = "three"
	request.IssuedAt = time.Now()
	current, err = applyControl(dir, request, session)
	if err != nil || current.EnabledUntil.IsZero() {
		t.Fatal("fresh explicit offer not applied", current, err)
	}
	request.ID = "four"
	request.Operation = "pause"
	request.IssuedAt = time.Now()
	request.EnabledUntil = time.Time{}
	current, err = applyControl(dir, request, session)
	if err != nil || !current.EnabledUntil.IsZero() {
		t.Fatal("admin pause not applied", current, err)
	}
	request.ID = "five"
	request.Operation = "enable"
	request.EnabledUntil = time.Now().Add(25 * time.Hour)
	if _, err = applyControl(dir, request, session); err == nil {
		t.Fatal("unbounded remote offer accepted")
	}
}
func TestLocalPausePreservesAdminRequestAcknowledgement(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("workstation requires an unprivileged login")
	}
	root := t.TempDir()
	cfg := Config{WorkDir: filepath.Join(root, "work"), CredentialFile: filepath.Join(root, "credential"), Workstation: &WorkstationConfig{RunnerDir: filepath.Join(root, "runner")}}
	if err := WorkstationCommand(context.Background(), cfg, "workstation-enable", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.WorkDir, "workstation")
	if _, err := updateControl(dir, func(c *workstationControl) { c.AppliedControl = "seen" }); err != nil {
		t.Fatal(err)
	}
	if err := WorkstationCommand(context.Background(), cfg, "workstation-pause", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	current, err := control(filepath.Join(dir, "control.json"))
	if err != nil || current.AppliedControl != "seen" || !current.EnabledUntil.IsZero() || current.ChangedAt.IsZero() {
		t.Fatal(current, err)
	}
}
