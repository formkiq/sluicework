package githubqueue

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type memoryStore struct {
	rows   map[string]Request
	leased bool
}

func (s *memoryStore) Lease(context.Context, string, time.Time, time.Duration) (bool, error) {
	if s.leased {
		return false, nil
	}
	s.leased = true
	return true, nil
}
func (s *memoryStore) Release(context.Context, string) error { s.leased = false; return nil }
func (s *memoryStore) Active(context.Context) ([]Request, error) {
	rows := []Request{}
	for _, r := range s.rows {
		if r.Active() {
			rows = append(rows, clone(r))
		}
	}
	return rows, nil
}
func (s *memoryStore) Save(_ context.Context, r *Request) error {
	old, exists := s.rows[r.ID]
	if exists && r.Version == 0 {
		return ErrExists
	}
	if exists && old.Version != r.Version {
		return ErrConflict
	}
	r.Version++
	s.rows[r.ID] = clone(*r)
	return nil
}
func clone(r Request) Request {
	b, _ := json.Marshal(r)
	var c Request
	_ = json.Unmarshal(b, &c)
	return c
}

type fakeGitHub struct {
	incoming          []Request
	dispatches        []Request
	cancels           []Request
	observations      map[string]Observation
	capacity          map[string]int
	callbacks         []Request
	failDispatch      bool
	observationErrors map[string]error
}

func (f *fakeGitHub) Discover(context.Context) ([]Request, error) { return f.incoming, nil }
func (f *fakeGitHub) Observe(_ context.Context, r Request) (Observation, error) {
	return f.observations[r.Attempt.RequestID], f.observationErrors[r.Attempt.RequestID]
}
func (f *fakeGitHub) Available(_ context.Context, _ Request, t Target) (int, error) {
	return f.capacity[t.Key()], nil
}
func (f *fakeGitHub) Dispatch(_ context.Context, r Request) error {
	f.dispatches = append(f.dispatches, clone(r))
	if f.failDispatch {
		return errors.New("uncertain POST")
	}
	return nil
}
func (f *fakeGitHub) Cancel(_ context.Context, r Request) error {
	f.cancels = append(f.cancels, clone(r))
	return nil
}
func (f *fakeGitHub) Publish(context.Context, Request) error { return nil }
func (f *fakeGitHub) Complete(_ context.Context, r Request) error {
	f.callbacks = append(f.callbacks, clone(r))
	return nil
}
func fixture() (*Engine, *memoryStore, *fakeGitHub, *time.Time) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	store := &memoryStore{rows: map[string]Request{}}
	gh := &fakeGitHub{observations: map[string]Observation{}, capacity: map[string]int{}}
	engine := &Engine{Store: store, GitHub: gh, ID: "test", Now: func() time.Time { return now }}
	return engine, store, gh, &now
}
func request(id string, p Priority, created time.Time) Request {
	minutes := 60
	if p == High {
		minutes = 15
	}
	return Request{ID: id, Repository: "team/" + id, ParentID: 100, ParentAttempt: 1, SourceSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Created: created, State: "queued", Placement: "prefer-local", Policy: Policy{Priority: p, PrimaryMinutes: minutes, Primary: Target{"linux-x64", "linux-runner-01", "local"}, Fallback: Target{"macos-arm64", "mac", "local"}, Cloud: Target{"linux-arm64", "", "cloud"}, FallbackMinutes: 2}}
}
func tick(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func observe(f *fakeGitHub, r Request, o Observation) { f.observations[r.Attempt.RequestID] = o }

func TestHighPriorityPrecedesOlderLowPriority(t *testing.T) {
	e, s, g, n := fixture()
	g.incoming = []Request{request("old-low", Low, n.Add(-time.Minute)), request("high", High, *n)}
	g.capacity["linux-x64:linux-runner-01"] = 1
	tick(t, e)
	if len(g.dispatches) != 1 || g.dispatches[0].ID != "high" {
		t.Fatalf("dispatches: %+v", g.dispatches)
	}
	if s.rows["old-low"].Attempt != nil {
		t.Fatal("low-priority request escaped the shared queue")
	}
}
func TestFifteenAndSixtyMinuteDeadlinesAndUnavailableMac(t *testing.T) {
	for p, wait := range map[Priority]time.Duration{High: 15 * time.Minute, Low: 60 * time.Minute} {
		t.Run(string(p), func(t *testing.T) {
			e, s, g, n := fixture()
			r := request("job", p, *n)
			g.incoming = []Request{r}
			tick(t, e)
			if r.Policy.Wait() != wait {
				t.Fatal("unexpected queue window", r.Policy.Wait())
			}
			*n = n.Add(5 * time.Minute)
			tick(t, e)
			if len(g.dispatches) != 0 {
				t.Fatal("fallback started at the former five-minute deadline")
			}
			*n = n.Add(wait - 5*time.Minute - time.Second)
			tick(t, e)
			if len(g.dispatches) != 0 {
				t.Fatal("fallback started before the deadline")
			}
			*n = n.Add(time.Second)
			tick(t, e)
			if len(g.dispatches) != 1 || g.dispatches[0].Attempt.Target.Host != "cloud" {
				t.Fatalf("dispatches: %+v", g.dispatches)
			}
			if s.rows[r.ID].Policy.Priority != p {
				t.Fatal("queue class changed")
			}
		})
	}
}
func TestAvailableMacFollowsLinuxRunnerDeadline(t *testing.T) {
	e, _, g, n := fixture()
	g.incoming = []Request{request("job", High, n.Add(-15*time.Minute))}
	g.capacity["macos-arm64:mac"] = 1
	tick(t, e)
	if len(g.dispatches) != 1 || g.dispatches[0].Attempt.Tier != 1 {
		t.Fatal(g.dispatches)
	}
}

func TestAcceptedLegacyHighRequestKeepsFiveMinuteSnapshot(t *testing.T) {
	for _, minutes := range []int{0, 5} {
		e, _, g, n := fixture()
		r := request("legacy", High, *n)
		r.Policy.PrimaryMinutes = minutes
		g.incoming = []Request{r}
		tick(t, e)
		*n = n.Add(5 * time.Minute)
		tick(t, e)
		if len(g.dispatches) != 1 || g.dispatches[0].Attempt.Target.Host != "cloud" {
			t.Fatal("accepted legacy deadline changed", minutes, g.dispatches)
		}
	}
}
func TestRunningLowPriorityWorkIsNotPreempted(t *testing.T) {
	e, s, g, n := fixture()
	r := request("low", Low, *n)
	g.incoming = []Request{r}
	g.capacity["linux-x64:linux-runner-01"] = 1
	tick(t, e)
	observe(g, s.rows[r.ID], Observation{Found: true, ID: 10, Status: "in_progress", Assigned: true, JobCount: 1})
	g.capacity["linux-x64:linux-runner-01"] = 0
	g.incoming = append(g.incoming, request("high", High, *n))
	tick(t, e)
	if len(g.cancels) != 0 || len(g.dispatches) != 1 {
		t.Fatalf("running work was preempted: %+v", g)
	}
}
func TestQueuedLowPriorityYieldsOnlyAfterConfirmedCancellation(t *testing.T) {
	e, s, g, n := fixture()
	r := request("low", Low, *n)
	g.incoming = []Request{r}
	g.capacity["linux-x64:linux-runner-01"] = 1
	tick(t, e)
	old := s.rows[r.ID]
	observe(g, old, Observation{Found: true, ID: 10, Status: "queued", JobCount: 1})
	g.incoming = append(g.incoming, request("high", High, *n))
	tick(t, e)
	if len(g.cancels) != 1 || len(g.dispatches) != 1 {
		t.Fatal("queue priority did not wait for cancellation")
	}
	observe(g, old, Observation{Found: true, ID: 10, Status: "completed", Conclusion: "cancelled", JobCount: 1})
	tick(t, e)
	if len(g.dispatches) != 2 || g.dispatches[1].ID != "high" {
		t.Fatal("high-priority request was not next")
	}
	if s.rows["low"].Attempt != nil {
		t.Fatal("lower-priority request also dispatched")
	}
}
func TestCancellationRaceNeverDuplicatesTheBuild(t *testing.T) {
	e, s, g, n := fixture()
	g.incoming = []Request{request("low", Low, *n)}
	g.capacity["linux-x64:linux-runner-01"] = 1
	tick(t, e)
	old := s.rows["low"]
	observe(g, old, Observation{Found: true, ID: 10, Status: "queued", JobCount: 1})
	g.incoming = append(g.incoming, request("high", High, *n))
	tick(t, e)
	observe(g, old, Observation{Found: true, ID: 10, Status: "completed", Conclusion: "cancelled", Assigned: true, JobCount: 1})
	g.capacity["linux-x64:linux-runner-01"] = 0
	tick(t, e)
	if s.rows["low"].State != "failed" || len(g.dispatches) != 1 {
		t.Fatal("assignment race caused duplicate work")
	}
}
func TestUncertainDispatchIsRecoveredWithoutAnotherPOST(t *testing.T) {
	e, s, g, n := fixture()
	g.incoming = []Request{request("job", High, *n)}
	g.capacity["linux-x64:linux-runner-01"] = 1
	g.failDispatch = true
	if e.Tick(context.Background()) == nil {
		t.Fatal("expected uncertain POST")
	}
	r := s.rows["job"]
	g.failDispatch = false
	observe(g, r, Observation{Found: true, ID: 10, Status: "in_progress", Assigned: true, JobCount: 1})
	tick(t, e)
	if len(g.dispatches) != 1 || s.rows["job"].State != "running" {
		t.Fatal("uncertain dispatch was repeated")
	}
}
func TestBuildFailureDoesNotRetryOnAnotherHost(t *testing.T) {
	e, s, g, n := fixture()
	g.incoming = []Request{request("job", High, *n)}
	g.capacity["linux-x64:linux-runner-01"] = 1
	tick(t, e)
	observe(g, s.rows["job"], Observation{Found: true, ID: 10, Status: "completed", Conclusion: "failure", Assigned: true, JobCount: 1})
	tick(t, e)
	tick(t, e)
	if len(g.dispatches) != 1 || len(g.callbacks) != 1 || g.callbacks[0].Conclusion != "failure" {
		t.Fatal("failed build was duplicated or not reported")
	}
}
func TestIdleMacSlotsCanServeMultipleRequests(t *testing.T) {
	e, _, g, n := fixture()
	g.capacity["macos-arm64:mac"] = 3
	for _, id := range []string{"one", "two", "three", "four"} {
		g.incoming = append(g.incoming, request(id, High, n.Add(-15*time.Minute)))
	}
	tick(t, e)
	if len(g.dispatches) != 4 {
		t.Fatal(g.dispatches)
	}
	mac := 0
	for _, r := range g.dispatches {
		if r.Attempt.Tier == 1 {
			mac++
		}
	}
	if mac != 3 || g.dispatches[3].Attempt.Target.Host != "cloud" {
		t.Fatal("Mac capacity was overbooked")
	}
}

func TestTargetReservationsIgnoreLabelCase(t *testing.T) {
	if (Target{Platform: "linux-x64", Label: "LINUX-RUNNER-01"}).Key() != (Target{Platform: "linux-x64", Label: "linux-runner-01"}).Key() {
		t.Fatal("GitHub label aliases created separate reservations")
	}
}

func TestUncertainWorkerRetainsReservationWithoutStoppingOtherTargets(t *testing.T) {
	e, s, g, n := fixture()
	g.incoming = []Request{request("uncertain", Low, *n)}
	g.capacity["linux-x64:linux-runner-01"] = 1
	tick(t, e)
	previous := s.rows["uncertain"]
	g.observationErrors = map[string]error{previous.Attempt.RequestID: errors.New("worker attempt changed")}
	g.incoming = append(g.incoming, request("ready", High, n.Add(-15*time.Minute)))
	g.capacity["macos-arm64:mac"] = 1
	if err := e.Tick(context.Background()); err == nil {
		t.Fatal("observation failure did not reach operational alert")
	}
	if len(g.dispatches) != 2 || g.dispatches[1].ID != "ready" || g.dispatches[1].Attempt.Tier != 1 {
		t.Fatal(g.dispatches)
	}
	if s.rows["uncertain"].State != "blocked" || s.rows["uncertain"].Attempt.RequestID != previous.Attempt.RequestID || len(g.cancels) != 0 {
		t.Fatal("uncertain execution was retried or its reservation released")
	}
}
