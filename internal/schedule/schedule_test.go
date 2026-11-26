package schedule

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"sluicework/internal/state"
)

func instant(t *testing.T, raw string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestNextWallTime(t *testing.T) {
	cases := []struct{ name, cron, zone, from, want string }{
		{"first instance of repeated hour", "30 1 * * *", "America/Chicago", "2026-11-01T05:59:00Z", "2026-11-01T06:30:00Z"},
		{"skip second instance", "30 1 * * *", "America/Chicago", "2026-11-01T06:30:00Z", "2026-11-02T07:30:00Z"},
		{"every minute after backward jump", "* * * * *", "America/Chicago", "2026-11-01T06:59:00Z", "2026-11-01T08:00:00Z"},
		{"nonexistent spring minute", "30 2 * * *", "America/Chicago", "2026-03-08T06:00:00Z", "2026-03-09T07:30:00Z"},
		{"first minute after spring gap", "0 3 * * *", "America/Chicago", "2026-03-08T07:59:59Z", "2026-03-08T08:00:00Z"},
		{"half hour transition", "45 1 * * *", "Australia/Lord_Howe", "2026-04-04T14:45:00Z", "2026-04-05T15:15:00Z"},
		{"quarter hour offset and named weekdays", "15 9 * * fri", "Asia/Kathmandu", "2026-11-19T00:00:00Z", "2026-11-20T03:30:00Z"},
		{"day of month or day of week", "0 0 1 * MON", "UTC", "2026-11-01T00:00:00Z", "2026-11-02T00:00:00Z"},
		{"leap day after nonleap century", "0 0 29 FEB *", "UTC", "2096-03-01T00:00:00Z", "2104-02-29T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := parseRule(tc.cron, tc.zone)
			if err != nil {
				t.Fatal(err)
			}
			got := r.next(instant(t, tc.from))
			if !got.Equal(instant(t, tc.want)) {
				t.Errorf("next %s, want %s", got, tc.want)
			}
		})
	}
}

func TestInvalidCron(t *testing.T) {
	for _, tc := range [][2]string{{"@every 1s", "UTC"}, {"TZ=UTC\t*\t*\t*\t*", "UTC"}, {"* * * * *", "Local"}, {"0 0 * * *", "Bogus/Zone"}} {
		if _, err := parseRule(tc[0], tc[1]); err == nil {
			t.Errorf("accepted %q %q", tc[0], tc[1])
		}
	}
	r, err := parseRule("0 0 31 FEB *", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if !r.next(time.Now()).IsZero() {
		t.Error("impossible date returned a next occurrence")
	}
}

func newService(t *testing.T, path string) (*Service, *state.Store) {
	t.Helper()
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st := &state.Store{DB: db}
	if err = st.Init(); err != nil {
		t.Fatal(err)
	}
	s := New(db, st)
	if err = s.Init(); err != nil {
		t.Fatal(err)
	}
	return s, st
}

func TestDurableDispatchRecoveryAndOverlap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster.db")
	s, jobs := newService(t, path)
	origin := instant(t, "2026-10-01T08:22:15Z")
	s.now = func() time.Time { return origin }
	job, err := jobs.SaveJob("pilot", "", state.Job{Name: "Summary", Type: "script", Command: "/bin/echo"})
	if err != nil {
		t.Fatal(err)
	}
	sc, err := s.Save("pilot", "", Spec{Name: "Every minute", Cron: "* * * * *", Timezone: "America/Winnipeg", JobID: job.ID})
	if err != nil {
		t.Fatal(err)
	}
	first := instant(t, "2026-10-01T08:23:00Z")
	if sc.NextRun == nil || !sc.NextRun.Equal(first) {
		t.Fatalf("first due %v", sc.NextRun)
	}

	// Simultaneous ticks from separately opened database handles dispatch once.
	second, _ := newService(t, path)
	done := make(chan error, 2)
	for _, scheduler := range []*Service{s, second} {
		go func(service *Service) { done <- service.tick(context.Background(), first, origin) }(scheduler)
	}
	for i := 0; i < 2; i++ {
		if err = <-done; err != nil {
			t.Fatal(err)
		}
	}
	if err = s.tick(context.Background(), first.Add(time.Minute), origin); err != nil {
		t.Fatal(err)
	}
	// Recover after passing 08:25 and 08:26. The existing run and all prior
	// occurrences survive; replaying the same clock tick adds no duplicates.
	boot := first.Add(3*time.Minute + 20*time.Second)
	second.now = func() time.Time { return boot }
	for i := 0; i < 2; i++ {
		if err = second.tick(context.Background(), boot, boot); err != nil {
			t.Fatal(err)
		}
	}
	list, err := second.Occurrences(sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 {
		t.Fatalf("occurrences %#v", list)
	}
	for i, want := range []string{"missed", "missed", "skipped_overlap", "queued"} {
		if list[i].Status != want {
			t.Errorf("occurrence %d = %+v", i, list[i])
		}
	}
	if list[3].RunID == "" {
		t.Error("queued occurrence has no run")
	}
	runs, err := jobs.Runs("", "", "")
	if err != nil || len(runs) != 1 || runs[0].ID != list[3].RunID {
		t.Fatalf("runs %+v, %v", runs, err)
	}
	current, err := second.Get(sc.ID)
	if err != nil || current.Latest.Status != "missed" || !current.NextRun.Equal(first.Add(4*time.Minute)) {
		t.Fatalf("current = %+v, %v", current, err)
	}

	if err = second.Delete("pilot", sc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = second.Get(sc.ID); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("deleted schedule get: %v", err)
	}
	if past, err := second.Occurrences(sc.ID); err != nil || len(past) != 4 {
		t.Fatalf("historical occurrences %+v %v", past, err)
	}
}

func TestOccurrenceAndRunRollbackTogether(t *testing.T) {
	s, jobs := newService(t, filepath.Join(t.TempDir(), "cluster.db"))
	created := time.Now().Truncate(time.Minute)
	s.now = func() time.Time { return created }
	job, err := jobs.SaveJob("admin", "", state.Job{Name: "Test", Type: "script", Command: "/bin/echo"})
	if err != nil {
		t.Fatal(err)
	}
	sc, err := s.Save("pilot", "", Spec{Name: "Hourly", Cron: "0 * * * *", Timezone: "UTC", JobID: job.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("CREATE TRIGGER injected_failure BEFORE UPDATE OF next_utc ON schedules BEGIN SELECT RAISE(ABORT,'injected'); END"); err != nil {
		t.Fatal(err)
	}
	if err = s.tick(context.Background(), *sc.NextRun, created); err == nil {
		t.Fatal("expected rollback")
	}
	history, err := s.Occurrences(sc.ID)
	if err != nil || len(history) != 0 {
		t.Fatalf("occurrence leaked: %+v, %v", history, err)
	}
	runs, err := jobs.Runs("", "", "")
	if err != nil || len(runs) != 0 {
		t.Fatalf("run leaked: %+v, %v", runs, err)
	}
	if _, err = s.db.Exec("DROP TRIGGER injected_failure"); err != nil {
		t.Fatal(err)
	}
	if err = s.tick(context.Background(), *sc.NextRun, created); err != nil {
		t.Fatal(err)
	}
	history, err = s.Occurrences(sc.ID)
	if err != nil || len(history) != 1 || history[0].RunID == "" {
		t.Fatalf("retry failed: %+v %v", history, err)
	}
}

func TestLiveSlotDuringLongRecovery(t *testing.T) {
	s, jobs := newService(t, filepath.Join(t.TempDir(), "cluster.db"))
	origin := instant(t, "2026-10-01T00:00:00Z")
	s.now = func() time.Time { return origin }
	job, err := jobs.SaveJob("admin", "", state.Job{Name: "Test", Type: "script", Command: "/bin/echo"})
	if err != nil {
		t.Fatal(err)
	}
	sc, err := s.Save("admin", "", Spec{Name: "Watch", JobID: job.ID, Cron: "* * * * *", Timezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	// More than a batch of minute slots passed while the orchestrator was down.
	boot := origin.Add(12*time.Hour - time.Second)
	now := origin.Add(12*time.Hour + time.Second)
	s.now = func() time.Time { return now }
	tick := func() {
		t.Helper()
		if e := s.tick(context.Background(), now, boot); e != nil {
			t.Fatal(e)
		}
	}
	tick()
	updated, err := s.Get(sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Latest == nil || updated.Latest.RunID == "" || updated.Latest.LocalSlot != "2026-10-01T12:00" {
		t.Fatalf("first live slot lost: %+v", updated.Latest)
	}
	if updated.NextRun == nil || !updated.NextRun.Equal(origin.Add(721*time.Minute)) {
		t.Fatalf("incorrect upcoming slot: %v", updated.NextRun)
	}
	tick()
	tick()
	var missed, queued int
	if err = s.db.QueryRow("SELECT count(*) FILTER (WHERE status='missed'), count(*) FILTER (WHERE status='queued') FROM schedule_occurrences WHERE schedule_id=?", sc.ID).Scan(&missed, &queued); err != nil {
		t.Fatal(err)
	}
	if missed != 719 || queued != 1 {
		t.Errorf("missed=%d queued=%d", missed, queued)
	}
}
