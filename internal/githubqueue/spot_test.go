package githubqueue

import (
	"context"
	"errors"
	"sluicework/internal/spot"
	"strings"
	"testing"
	"time"
)

type fakeFleet struct {
	alloc    map[string]spot.PublicAllocation
	jobs     []spot.Job
	ready    bool
	disabled bool
	retired  []string
}

func (f *fakeFleet) Refresh(context.Context) error   { return nil }
func (f *fakeFleet) Reconcile(context.Context) error { return nil }
func (f *fakeFleet) Policy(string) *spot.Policy {
	return &spot.Policy{Profile: "standard", QueueMinutes: 5, StartupMinutes: 10, RuntimeMinutes: 90}
}
func (f *fakeFleet) Ensure(_ context.Context, j spot.Job) (spot.PublicAllocation, bool, error) {
	if f.disabled {
		return spot.PublicAllocation{}, false, spot.ErrDisabled
	}
	if a, ok := f.alloc[j.ID]; ok {
		return a, f.ready, nil
	}
	if len(f.alloc) >= 3 {
		return spot.PublicAllocation{}, false, spot.ErrCapacity
	}
	a := spot.PublicAllocation{ID: j.ID, State: "booting", Profile: "standard", Label: "sluicework-spot-" + j.ID, InstanceID: "i-00000000000000001"}
	f.alloc[j.ID] = a
	f.jobs = append(f.jobs, j)
	return a, f.ready, nil
}
func (f *fakeFleet) Allocation(_ context.Context, id string) (spot.PublicAllocation, bool, error) {
	a, ok := f.alloc[id]
	return a, ok, nil
}
func (f *fakeFleet) Retire(_ context.Context, id string) (bool, error) {
	f.retired = append(f.retired, id)
	delete(f.alloc, id)
	return true, nil
}
func fleetFixture() (*Engine, *memoryStore, *fakeGitHub, *fakeFleet, *time.Time) {
	e, s, g, n := fixture()
	f := &fakeFleet{alloc: map[string]spot.PublicAllocation{}}
	e.Fleet = f
	return e, s, g, f, n
}
func spotRequest(id string, priority Priority, n time.Time) Request {
	r := request(id, priority, n.Add(-time.Hour))
	r.WorkflowSHA = strings.Repeat("b", 40)
	r.Ref = "main"
	r.Kind = "site"
	return r
}
func TestSpotThreeAcrossReposFourthWaitsInPriorityOrder(t *testing.T) {
	e, s, g, f, n := fleetFixture()
	g.incoming = []Request{spotRequest("low", Low, *n), spotRequest("a", High, *n), spotRequest("b", High, *n), spotRequest("c", High, *n)}
	tick(t, e)
	if len(f.jobs) != 3 || len(g.dispatches) != 0 || s.rows["low"].State != "waiting-spot" {
		t.Fatal("unexpected fleet reservation", f.jobs, g.dispatches, s.rows)
	}
	f.ready = true
	tick(t, e)
	if len(g.dispatches) != 3 {
		t.Fatal(g.dispatches)
	}
	for _, r := range g.dispatches {
		if r.Attempt.Tier != 2 || r.Attempt.Target.Host != "local" || r.Attempt.Target.Platform != "linux-arm64" || r.Attempt.RequestID != f.jobs[0].RequestID && r.ID == f.jobs[0].ID {
			t.Fatal("v3 dispatch identity differs from one-job gate", r)
		}
	}
	*n = n.Add(5 * time.Minute)
	tick(t, e)
	if s.rows["low"].Attempt == nil || s.rows["low"].Attempt.Target.Host != "cloud" {
		t.Fatal("full fleet did not fall back", s.rows["low"])
	}
}
func TestSpotReservationDoesNotSwitchBackToMacWhileBooting(t *testing.T) {
	e, s, g, f, n := fleetFixture()
	g.incoming = []Request{spotRequest("one", High, *n)}
	tick(t, e)
	r := s.rows["one"]
	g.capacity[r.Policy.Fallback.Key()] = 1
	tick(t, e)
	if len(g.dispatches) != 0 || len(f.jobs) != 1 || s.rows["one"].NextTier != 2 {
		t.Fatal("provisioning switched targets")
	}
}
func TestSpotFailureDoesNotRetryButVerifiedInterruptionDoes(t *testing.T) {
	for _, interruption := range []bool{false, true} {
		t.Run(map[bool]string{false: "code failure", true: "AWS interruption"}[interruption], func(t *testing.T) {
			e, s, g, f, n := fleetFixture()
			f.ready = true
			g.incoming = []Request{spotRequest("one", High, *n)}
			tick(t, e)
			r := s.rows["one"]
			observe(g, r, Observation{Found: true, ID: 42, Status: "completed", Conclusion: "failure", Assigned: true, JobCount: 1})
			if interruption {
				a := f.alloc[r.ID]
				a.State = "terminated"
				a.EndReason = "Server.SpotInstanceTermination"
				f.alloc[r.ID] = a
			}
			tick(t, e)
			if interruption {
				if len(g.dispatches) != 2 || g.dispatches[1].Attempt.Target.Host != "cloud" || len(f.retired) != 1 {
					t.Fatal("interruption not retired before hosted fallback", g.dispatches, f.retired)
				}
			} else if len(g.dispatches) != 1 || !s.rows[r.ID].Terminal() {
				t.Fatal("ordinary code failure retried")
			}
		})
	}
}
func TestSpotCancellationWaitsForTerminalWorkerBeforeFallback(t *testing.T) {
	e, s, g, f, n := fleetFixture()
	f.ready = true
	g.incoming = []Request{spotRequest("one", High, *n)}
	tick(t, e)
	r := s.rows["one"]
	a := f.alloc[r.ID]
	a.State = "terminated"
	a.EndReason = "Server.SpotInstanceShutdown"
	f.alloc[r.ID] = a
	observe(g, r, Observation{Found: true, ID: 42, Status: "in_progress", Assigned: true, JobCount: 1})
	tick(t, e)
	if len(g.dispatches) != 1 || len(g.cancels) != 1 {
		t.Fatal("fallback before cancellation", g.dispatches, g.cancels)
	}
	r = s.rows[r.ID]
	observe(g, r, Observation{Found: true, ID: 42, Status: "completed", Conclusion: "cancelled", Assigned: true, JobCount: 1})
	tick(t, e)
	if len(g.dispatches) != 2 {
		t.Fatal(g.dispatches)
	}
}
func TestLocalOnlyNeverCreatesSpot(t *testing.T) {
	e, s, g, f, n := fleetFixture()
	r := spotRequest("one", High, *n)
	r.Placement = "local-only"
	g.incoming = []Request{r}
	tick(t, e)
	if len(f.jobs) != 0 || !s.rows[r.ID].Terminal() {
		t.Fatal("local-only launched cloud", f.jobs)
	}
}
func TestMissingFleetCannotSilentlyBypassExistingSpotReservation(t *testing.T) {
	e, _, g, _, n := fleetFixture()
	g.incoming = []Request{spotRequest("one", High, *n)}
	tick(t, e)
	e.Fleet = nil
	if err := e.Tick(context.Background()); err == nil || errors.Is(err, spot.ErrDisabled) {
		t.Fatal(err)
	}
}
