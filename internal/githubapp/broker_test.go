package githubapp

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"sluicework/internal/state"
)

type fakeRunners struct {
	runner           Runner
	created, deleted int
	labels           []string
	missing          bool
}

func (f *fakeRunners) Create(_ context.Context, _ string, labels []string) (Grant, error) {
	f.created++
	f.runner = Runner{ID: 42, Status: "online"}
	f.labels = slices.Clone(labels)
	f.missing = false
	return Grant{Runner: f.runner, JIT: "one-run-secret"}, nil
}
func (f *fakeRunners) Get(context.Context, int64) (Runner, error) {
	if f.missing {
		return Runner{}, HTTPError(404)
	}
	return f.runner, nil
}
func (f *fakeRunners) Labels(_ context.Context, _ int64, labels []string) error {
	f.labels = slices.Clone(labels)
	if f.missing {
		return HTTPError(404)
	}
	return nil
}
func (f *fakeRunners) Delete(context.Context, int64) error { f.deleted++; f.missing = true; return nil }

func brokerFixture(t *testing.T) (*Broker, *state.Store, state.Machine, *fakeRunners) {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := &state.Store{DB: db}
	if err = s.Init(); err != nil {
		t.Fatal(err)
	}
	token, _ := s.EnrollToken("admin")
	e, err := s.Enroll(token, state.WorkerInfo{Name: "Mac", OS: "darwin", Arch: "arm64", Roles: []string{"ci"}})
	if err != nil {
		t.Fatal(err)
	}
	reg := state.Runner{Workstation: true, Name: "pilot", Scope: "team/pilot", Labels: []string{"macOS", "ARM64", "sluicework-pilot"}}
	if err = s.UpdateMachine("admin", e.ID, state.MachinePatch{Runner: &reg}); err != nil {
		t.Fatal(err)
	}
	m, _ := s.Machine(e.ID)
	if _, err = db.Exec(leaseSchema); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunners{missing: true}
	b := &Broker{db: db, client: f, repository: "team/pilot"}
	return b, s, m, f
}

func TestBrokerScopesGrantsAndPreservesAssignedJobOnPause(t *testing.T) {
	b, _, m, f := brokerFixture(t)
	ctx := context.Background()
	bad := m
	bad.Runner = &state.Runner{Workstation: true, Scope: "another/repository"}
	if _, err := b.Act(ctx, bad, "acquire"); !errors.Is(err, state.ErrInvalid) {
		t.Fatal("scope escaped", err)
	}
	grant, err := b.Act(ctx, m, "acquire")
	if err != nil || grant.JIT == "" {
		t.Fatal(grant, err)
	}
	if !slices.Contains(f.labels, "sluicework-"+m.ID[:12]) {
		t.Fatal("missing machine label", f.labels)
	}
	grant, err = b.Act(ctx, m, "acquire")
	if err != nil || grant.JIT != "" || f.created != 1 {
		t.Fatal("duplicate grant", grant, err)
	}
	f.runner.Busy = true
	if _, err = b.Act(ctx, m, "pause"); err != nil || len(f.labels) != 0 || f.deleted != 0 {
		t.Fatal("pause interrupted build", err)
	}
	if _, err = b.Act(ctx, m, "release"); !errors.Is(err, state.ErrConflict) {
		t.Fatal("deleted busy runner", err)
	}
	if _, err = b.Act(ctx, m, "enable"); err != nil || len(f.labels) == 0 {
		t.Fatal("cannot resume", err)
	}
	m.Draining = true
	if _, err = b.Act(ctx, m, "enable"); !errors.Is(err, state.ErrConflict) {
		t.Fatal("enabled draining runner", err)
	}
	f.runner.Busy = false
	m.Draining = false
	if _, err = b.Act(ctx, m, "release"); err != nil || f.deleted != 1 {
		t.Fatal("cleanup", err)
	}
}

func TestReconcileWithdrawsOfflineMachinesAndWaitsForBusyRunner(t *testing.T) {
	b, s, m, f := brokerFixture(t)
	ctx := context.Background()
	if _, err := b.Act(ctx, m, "acquire"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE machines SET heartbeat=0 WHERE id=?", m.ID); err != nil {
		t.Fatal(err)
	}
	f.runner.Busy = true
	if err := b.Reconcile(ctx, s); err != nil || len(f.labels) != 0 || f.deleted != 0 {
		t.Fatal("offline withdrawal", err)
	}
	if _, err := s.DB.Exec("UPDATE github_runner_leases SET withdrawn=?", time.Now().Add(-2*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := b.Reconcile(ctx, s); err != nil || f.deleted != 0 {
		t.Fatal("interrupted busy runner", err)
	}
	f.runner.Busy = false
	if err := b.Reconcile(ctx, s); err != nil || f.deleted != 1 {
		t.Fatal("stale lease not removed", err)
	}
}

func TestConnectionReplacementRequiresIdleAndRetiresOldBroker(t *testing.T) {
	b, _, machine, _ := brokerFixture(t)
	if _, err := b.Act(context.Background(), machine, "acquire"); err != nil {
		t.Fatal(err)
	}
	changed := false
	apply := func() error { changed = true; return nil }
	if err := b.Replace(true, apply); !errors.Is(err, state.ErrConflict) || changed {
		t.Fatal("replaced App while a runner lease existed", err)
	}
	if _, err := b.Act(context.Background(), machine, "release"); err != nil {
		t.Fatal(err)
	}
	if err := b.Replace(true, apply); err != nil || !changed {
		t.Fatal(err)
	}
	if _, err := b.Act(context.Background(), machine, "acquire"); !errors.Is(err, state.ErrConflict) {
		t.Fatal("retired broker registered another runner", err)
	}
}
