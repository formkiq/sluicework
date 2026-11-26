package state

import (
	"errors"
	"testing"
)

func TestMixedPlatformPlacementPreservesApprovedRevision(t *testing.T) {
	s, _ := testStore(t)
	platforms := []WorkerInfo{
		{Name: "linux-x64", OS: "linux", Arch: "amd64", Roles: []string{"script"}},
		{Name: "linux-arm", OS: "linux", Arch: "arm64", Roles: []string{"script"}},
		{Name: "mac-arm", OS: "darwin", Arch: "arm64", Roles: []string{"script"}},
		{Name: "mac-x64", OS: "darwin", Arch: "amd64", Roles: []string{"script"}},
		{Name: "legacy", Roles: []string{"script"}},
	}
	workers := []Enrollment{}
	for _, info := range platforms {
		token, _ := s.EnrollToken("admin")
		e, err := s.Enroll(token, info)
		if err != nil {
			t.Fatal(err)
		}
		workers = append(workers, e)
	}
	j := script(t, s)
	j.OS, j.Arch = "linux", "amd64"
	j, err := s.SaveJob("admin", j.ID, j)
	if err != nil {
		t.Fatal(err)
	}
	r := queue(t, s, j)
	// Editing the template cannot redirect a previously queued build.
	j.OS, j.Arch = "darwin", "arm64"
	if _, err = s.SaveJob("admin", j.ID, j); err != nil {
		t.Fatal(err)
	}
	for _, worker := range workers[1:] {
		if claim(t, s, worker.ID) != nil {
			t.Fatal("incompatible worker claimed Linux x86-64 job")
		}
	}
	c := claim(t, s, workers[0].ID)
	if c == nil || c.RunID != r.ID || c.Job.OS != "linux" || c.Job.Arch != "amd64" {
		t.Fatalf("wrong placement: %+v", c)
	}
	if err = s.Complete(workers[0].ID, c.AttemptID, Completion{State: "failed"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Retry("operator", r.ID); err != nil {
		t.Fatal(err)
	}
	if claim(t, s, workers[2].ID) != nil {
		t.Fatal("retry fell back to Mac")
	}
	if claim(t, s, workers[0].ID) == nil {
		t.Fatal("retry lost platform")
	}
	mac := queue(t, s, j)
	if claim(t, s, workers[1].ID) != nil {
		t.Fatal("Mac job fell back to Linux ARM64")
	}
	if got := claim(t, s, workers[2].ID); got == nil || got.RunID != mac.ID {
		t.Fatal("Mac ARM64 placement failed")
	}
}

func TestPlatformValidationAndRunnerLabels(t *testing.T) {
	s, _ := testStore(t)
	j := script(t, s)
	j.OS = "windows"
	if _, err := s.SaveJob("admin", j.ID, j); !errors.Is(err, ErrInvalid) {
		t.Fatal("unsupported job OS accepted", err)
	}
	j.OS, j.Arch = "linux", "x86_64"
	if _, err := s.SaveJob("admin", j.ID, j); !errors.Is(err, ErrInvalid) {
		t.Fatal("noncanonical arch accepted", err)
	}
	info := WorkerInfo{Name: "linux", OS: "linux", Arch: "amd64", Roles: []string{"script"}}
	token, _ := s.EnrollToken("admin")
	e, err := s.Enroll(token, info)
	if err != nil {
		t.Fatal(err)
	}
	runner := Runner{Name: "build", Scope: "team/repo", Service: "actions.runner.team-repo.build.service", Labels: []string{"self-hosted", "Linux", "X64"}}
	if err = s.UpdateMachine("admin", e.ID, MachinePatch{Runner: &runner}); err != nil {
		t.Fatal(err)
	}
	for _, labels := range [][]string{{"macOS", "X64"}, {"Linux", "ARM64"}} {
		runner.Labels = labels
		if err = s.UpdateMachine("admin", e.ID, MachinePatch{Runner: &runner}); !errors.Is(err, ErrInvalid) {
			t.Fatal("misleading runner platform accepted", err)
		}
	}
}
