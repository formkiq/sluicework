package state

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cluster.sqlite")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := &Store{DB: db, LogLimit: 6}
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	return s, path
}
func machine(t *testing.T, s *Store, name string) Enrollment {
	t.Helper()
	token, err := s.EnrollToken("admin")
	if err != nil {
		t.Fatal(err)
	}
	info := WorkerInfo{Name: name, Version: "1", Roles: []string{"script", "agent"}}
	e, err := s.Enroll(token, info)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enroll(token, info); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("reused token", err)
	}
	return e
}
func script(t *testing.T, s *Store) Job {
	t.Helper()
	j, err := s.SaveJob("admin", "", Job{Name: "sample", Type: "script", Command: "/bin/echo", Args: []string{"{{value}}"}, Inputs: []Input{{Name: "value", Required: true}}, OutputLimit: 5})
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func queue(t *testing.T, s *Store, j Job) Run {
	t.Helper()
	r, err := s.Queue("operator", j.ID, map[string]string{"value": "test"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func claim(t *testing.T, s *Store, id string) *Claim {
	t.Helper()
	c, err := s.Claim(id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func TestConcurrentClaimAndStaleLease(t *testing.T) {
	s, path := testStore(t)
	a, b := machine(t, s, "one"), machine(t, s, "two")
	j := script(t, s)
	r := queue(t, s, j)
	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	second := &Store{DB: db2}
	type answer struct {
		worker string
		c      *Claim
		err    error
	}
	ch := make(chan answer, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, worker := range []string{a.ID, b.ID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c, err := []*Store{s, second}[i].Claim(worker)
			ch <- answer{worker, c, err}
		}()
	}
	close(start)
	wg.Wait()
	close(ch)
	var owner, other string
	var winner *Claim
	for v := range ch {
		if v.err != nil {
			t.Fatal(v.err)
		}
		if v.c != nil {
			if winner != nil {
				t.Fatal("duplicate claims")
			}
			owner = v.worker
			winner = v.c
		} else {
			other = v.worker
		}
	}
	if winner == nil || winner.RunID != r.ID || other == "" {
		t.Fatal("missing unique winner")
	}
	if _, err := s.Renew(other, winner.AttemptID); !errors.Is(err, ErrLease) {
		t.Fatal("cross-worker renewal", err)
	}
	if _, err := s.DB.Exec("UPDATE attempts SET lease_until=0 WHERE id=?", winner.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Renew(owner, winner.AttemptID); !errors.Is(err, ErrLease) {
		t.Fatal("renewed expired attempt", err)
	}
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	record, err := s.Retry("operator", r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Attempts) != 2 || record.Attempts[0].State != "interrupted" || !record.Attempts[0].OutcomeUnknown {
		t.Fatalf("attempt history: %+v", record.Attempts)
	}
	replacement := claim(t, s, other)
	if replacement == nil || replacement.AttemptID == winner.AttemptID {
		t.Fatal("expected new attempt")
	}
	if err := s.Complete(owner, winner.AttemptID, Completion{State: "succeeded"}); !errors.Is(err, ErrLease) {
		t.Fatal("stale completion", err)
	}
}
func TestPlacementAndRevoke(t *testing.T) {
	s, _ := testStore(t)
	a, b := machine(t, s, "one"), machine(t, s, "two")
	j := script(t, s)
	j.Labels = []string{"ci"}
	j, err := s.SaveJob("admin", j.ID, j)
	if err != nil {
		t.Fatal(err)
	}
	first := queue(t, s, j)
	queue(t, s, j)
	labels := []string{"ci"}
	slots := 2
	limits := map[string]int{"script": 1}
	drain := true
	if err := s.UpdateMachine("admin", a.ID, MachinePatch{Labels: &labels, Slots: &slots, RoleLimits: &limits, Draining: &drain}); err != nil {
		t.Fatal(err)
	}
	if c := claim(t, s, a.ID); c != nil {
		t.Fatal("drained worker claimed work")
	}
	if c := claim(t, s, b.ID); c != nil {
		t.Fatal("unlabelled worker claimed work")
	}
	drain = false
	if err := s.UpdateMachine("admin", a.ID, MachinePatch{Draining: &drain}); err != nil {
		t.Fatal(err)
	}
	c := claim(t, s, a.ID)
	if c == nil || c.RunID != first.ID {
		t.Fatal("unexpected placement")
	}
	if claim(t, s, a.ID) != nil {
		t.Fatal("role limit ignored")
	}
	if err := s.Revoke("admin", a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateWorker(a.Credential); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("revoked credential accepted", err)
	}
	if err := s.AppendLog(a.ID, c.AttemptID, "new"); !errors.Is(err, ErrLease) {
		t.Fatal("revoked worker wrote log", err)
	}
	if _, err := s.Claim(a.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("revoked worker claimed", err)
	}
	if _, err := s.DB.Exec("UPDATE machines SET heartbeat=? WHERE id=?", time.Now().Add(-time.Hour).UnixMilli(), b.ID); err != nil {
		t.Fatal(err)
	}
	if m, err := s.Machine(b.ID); err != nil || m.Online {
		t.Fatal("offline worker", m, err)
	}
}
func TestSnapshotLogsAndCancellation(t *testing.T) {
	s, _ := testStore(t)
	a := machine(t, s, "one")
	j := script(t, s)
	r := queue(t, s, j)
	j.Name = "edited"
	if _, err := s.SaveJob("admin", j.ID, j); err != nil {
		t.Fatal(err)
	}
	c := claim(t, s, a.ID)
	if c == nil || c.Job.Name != "sample" || c.Job.Revision != 1 || c.Inputs["value"] != "test" {
		t.Fatalf("snapshot: %+v", c)
	}
	if err := s.AppendLog(a.ID, c.AttemptID, "abc"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendLog(a.ID, c.AttemptID, "☃abc"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendLog(a.ID, c.AttemptID, "later"); err != nil {
		t.Fatal(err)
	}
	logs, err := s.Logs(r.ID, 0)
	if err != nil || len(logs) != 2 {
		t.Fatal("log count", logs, err)
	}
	after, err := s.Logs(r.ID, logs[0].ID)
	if err != nil || len(after) != 1 || after[0].Text != "☃" || !after[0].Truncated {
		t.Fatal("resume", after, err)
	}
	if err := s.Cancel("operator", r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActiveClaim(a.ID, c.AttemptID); !errors.Is(err, ErrLease) {
		t.Fatal("broker after cancellation", err)
	}
	renewal, err := s.Renew(a.ID, c.AttemptID)
	if err != nil || !renewal.CancelRequested || !renewal.LeaseUntil.Equal(c.LeaseUntil) {
		t.Fatal("worker did not receive cancellation", err)
	}
	if err := s.Complete(a.ID, c.AttemptID, Completion{State: "cancelled", Output: "abcd☃"}); err != nil {
		t.Fatal(err)
	}
	rr, err := s.Run(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempt := rr.Attempts[0]
	if rr.State != "cancelled" || attempt.Output != "abcd" || !attempt.OutputTruncated || !attempt.LogsTruncated || attempt.OutcomeUnknown {
		t.Fatalf("cancelled attempt %+v", attempt)
	}
}
func TestInputPathAndExpiry(t *testing.T) {
	s, _ := testStore(t)
	j := script(t, s)
	j.Inputs[0].Paths = []string{"/approved/input.txt"}
	j, err := s.SaveJob("admin", j.ID, j)
	if err != nil {
		t.Fatal(err)
	}
	for _, inputs := range []map[string]string{{}, {"value": "/etc/passwd"}, {"value": "/approved/input.txt", "extra": "text"}} {
		if _, err := s.Queue("operator", j.ID, inputs); !errors.Is(err, ErrInvalid) {
			t.Fatal("accepted invalid inputs", inputs, err)
		}
	}
	if _, err := s.Queue("operator", j.ID, map[string]string{"value": "/approved/input.txt"}); err != nil {
		t.Fatal(err)
	}
	token, err := s.EnrollToken("admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE enrollment_tokens SET expires=0"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enroll(token, WorkerInfo{Name: "expired", Roles: []string{"agent"}}); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("expired enrollment", err)
	}
}

func TestAutomaticRetryPolicy(t *testing.T) {
	s, _ := testStore(t)
	worker := machine(t, s, "test")
	j := script(t, s)
	j.MaxAutoRetries = 1
	if _, err := s.SaveJob("admin", j.ID, j); !errors.Is(err, ErrInvalid) {
		t.Fatal("unsafe automatic retry accepted", err)
	}
	j.SafeToRepeat = true
	j, err := s.SaveJob("admin", j.ID, j)
	if err != nil {
		t.Fatal(err)
	}
	r := queue(t, s, j)
	c := claim(t, s, worker.ID)
	if err := s.Complete(worker.ID, c.AttemptID, Completion{State: "failed", Error: "exit status 1"}); err != nil {
		t.Fatal(err)
	}
	rr, err := s.Run(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rr.Attempts) != 2 || rr.State != "queued" || !rr.Attempts[1].Automatic || rr.Attempts[0].State != "failed" {
		t.Fatalf("automatic retry history %+v", rr)
	}
	c = claim(t, s, worker.ID)
	if _, err := s.DB.Exec("UPDATE attempts SET lease_until=0 WHERE id=?", c.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	rr, err = s.Run(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rr.Attempts) != 2 || rr.State != "interrupted" {
		t.Fatal("exceeded retry budget", rr)
	}
	// A cancellation request wins over opt-in retry, including when its worker disappears.
	r = queue(t, s, j)
	c = claim(t, s, worker.ID)
	if err := s.Cancel("operator", r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE attempts SET lease_until=0 WHERE id=?", c.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	rr, err = s.Run(r.ID)
	if err != nil || len(rr.Attempts) != 1 || rr.State != "interrupted" {
		t.Fatal("cancelled work was retried", rr, err)
	}
}
func TestRestoreFencesHistoricalWork(t *testing.T) {
	s, _ := testStore(t)
	worker := machine(t, s, "test")
	j := script(t, s)
	j.SafeToRepeat = true
	j.MaxAutoRetries = 3
	j, err := s.SaveJob("admin", j.ID, j)
	if err != nil {
		t.Fatal(err)
	}
	running := queue(t, s, j)
	queued := queue(t, s, j)
	if c := claim(t, s, worker.ID); c == nil || c.RunID != running.ID {
		t.Fatal("missing claim")
	}
	if err := transact(s.DB, InterruptRestoredAttempts); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	for _, pair := range []struct {
		id, status string
		unknown    bool
	}{{running.ID, "interrupted", true}, {queued.ID, "cancelled", false}} {
		rr, err := s.Run(pair.id)
		if err != nil {
			t.Fatal(err)
		}
		if rr.State != pair.status || len(rr.Attempts) != 1 || rr.Attempts[0].OutcomeUnknown != pair.unknown || rr.Attempts[0].Error == "" {
			t.Fatalf("restore history %+v", rr)
		}
	}
	if c := claim(t, s, worker.ID); c != nil {
		t.Fatal("restored work automatically started")
	}
}

func TestSuccessfulJSONIsComplete(t *testing.T) {
	s, _ := testStore(t)
	worker := machine(t, s, "test")
	j, err := s.SaveJob("admin", "", Job{Name: "structured", Type: "ai", Connection: "local", Model: "test", Prompt: "hello", OutputFormat: "json", OutputLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Queue("operator", j.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := claim(t, s, worker.ID)
	for _, bad := range []Completion{{State: "succeeded"}, {State: "succeeded", Output: "["}, {State: "succeeded", Output: "[12345678]"}, {State: "succeeded", Output: "{}", OutputTruncated: true}} {
		if e := s.Complete(worker.ID, c.AttemptID, bad); !errors.Is(e, ErrInvalid) {
			t.Fatal("accepted incomplete structured result", bad, e)
		}
	}
	if err := s.Complete(worker.ID, c.AttemptID, Completion{State: "succeeded", Output: "[1]"}); err != nil {
		t.Fatal(err)
	}
	done, err := s.Run(r.ID)
	if err != nil || done.Attempts[0].Output != "[1]" {
		t.Fatal("lost structured result", done, err)
	}
}

func TestIdempotentLogSequence(t *testing.T) {
	s, _ := testStore(t)
	w := machine(t, s, "one")
	j := script(t, s)
	r := queue(t, s, j)
	a := claim(t, s, w.ID)
	if err := s.AppendLogChunk(w.ID, a.AttemptID, "abc", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendLogChunk(w.ID, a.AttemptID, "abc", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendLogChunk(w.ID, a.AttemptID, "last", 3); !errors.Is(err, ErrConflict) {
		t.Fatal("gap was accepted", err)
	}
	if err := s.AppendLogChunk(w.ID, a.AttemptID, "defgh", 2); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendLogChunk(w.ID, a.AttemptID, "dropped", 3); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Logs(r.ID, 0)
	if err != nil || len(rows) != 2 || rows[0].Text != "abc" || rows[1].Text != "def" || !rows[1].Truncated {
		t.Fatal(rows, err)
	}
}
func TestRunListCursor(t *testing.T) {
	s, _ := testStore(t)
	j := script(t, s)
	for i := 0; i < 103; i++ {
		queue(t, s, j)
	}
	// Every run shares the creation timestamp, so a time-only cursor would lose rows.
	if _, err := s.DB.Exec("UPDATE runs SET created=1"); err != nil {
		t.Fatal(err)
	}
	first, err := s.Runs(j.ID, "", "queued")
	if err != nil || len(first) != 100 {
		t.Fatal(len(first), err)
	}
	queue(t, s, j)
	second, err := s.RunsBefore(j.ID, "", "queued", first[99].ID)
	if err != nil || len(second) != 3 {
		t.Fatal(len(second), err)
	}
	seen := map[string]bool{}
	for _, r := range append(first, second...) {
		if seen[r.ID] {
			t.Fatal("duplicate run", r.ID)
		}
		seen[r.ID] = true
	}
	if _, err := s.RunsBefore("", "", "", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
