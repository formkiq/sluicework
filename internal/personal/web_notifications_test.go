package personal

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sluicework/internal/provider"
)

func TestOptionalPermissionsAreBoundToLocalApproval(t *testing.T) {
	a := testAgent(t)
	p := propose(a)
	a.inbox[p.ID] = p
	preview, err := a.Preview(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	p.Manifest.WebSearch = &provider.WebSearch{AllowedDomains: []string{"example.com"}}
	p.Manifest.DesktopNotifications = true
	a.inbox[p.ID] = p
	if _, err = a.Approve(p.ID, preview.Digest); err == nil {
		t.Fatal("web and notification permissions changed after preview without reapproval")
	}
	p.Manifest.WebSearch = nil
	p.Manifest.DesktopNotifications = false
	a.inbox[p.ID] = p
	if _, err = a.Approve(p.ID, preview.Digest); err != nil {
		t.Fatal(err)
	}
	p.Revision++
	p.Manifest.WebSearch = &provider.WebSearch{}
	p.Manifest.DesktopNotifications = true
	a.inbox[p.ID] = p
	var generations, alerts atomic.Int32
	a.generate = func(ctx context.Context, r provider.Request) (provider.Result, error) {
		generations.Add(1)
		if r.WebSearch != nil {
			t.Error("remote edit enabled browsing")
		}
		return provider.Result{Text: "Private report"}, nil
	}
	a.notify = func(context.Context, string) error { alerts.Add(1); return nil }
	now := time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
	a.boot = now.Add(-time.Minute)
	_ = a.Pause(false)
	if err = a.Tick(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	a.wg.Wait()
	if generations.Load() != 1 || alerts.Load() != 0 {
		t.Fatal("remote proposal gained a notification permission")
	}
	approvals, _ := a.Report()
	if approvals.Approvals[0].WebSearch != nil || approvals.Approvals[0].DesktopNotifications {
		t.Fatal("reported permissions do not match local consent")
	}
}
func TestApprovedResearchNotificationAndPrivateCitations(t *testing.T) {
	a := testAgent(t)
	p := propose(a)
	p.Manifest.WebSearch = &provider.WebSearch{AllowedDomains: []string{"example.com"}}
	p.Manifest.DesktopNotifications = true
	a.inbox[p.ID] = p
	preview, _ := a.Preview(p.ID)
	if _, err := a.Approve(p.ID, preview.Digest); err != nil {
		t.Fatal(err)
	}
	a.generate = func(ctx context.Context, r provider.Request) (provider.Result, error) {
		if r.WebSearch == nil || len(r.WebSearch.AllowedDomains) != 1 || r.WebSearch.AllowedDomains[0] != "example.com" {
			t.Error("approved search scope lost")
		}
		return provider.Result{Text: "Private result [1]", WebSearched: true, Citations: []provider.Citation{{URL: "https://example.com", Title: "Private source", Start: 15, End: 18}}}, nil
	}
	var alerts atomic.Int32
	a.notify = func(ctx context.Context, status string) error {
		alerts.Add(1)
		if status != "succeeded" {
			t.Error("wrong outcome")
		}
		return errors.New("fake unavailable desktop")
	}
	now := time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
	a.boot = now.Add(-time.Minute)
	_ = a.Pause(false)
	if err := a.Tick(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	a.wg.Wait()
	local, err := a.Results(10, false)
	if err != nil || len(local) != 1 || local[0].Notification != "failed" || local[0].State != "succeeded" || len(local[0].Citations) != 1 || !local[0].WebSearched {
		t.Fatalf("research or delivery status lost: %+v %v", local, err)
	}
	report, _ := a.Report()
	if report.Runs[0].Result != "" || len(report.Runs[0].Citations) != 0 {
		t.Fatal("private result or sources uploaded")
	}
	a.notifyRun(local[0].ID, "succeeded")
	_ = a.Tick(context.Background(), now)
	a.wg.Wait()
	if alerts.Load() != 1 {
		t.Fatal("desktop submission or inference was retried")
	}
}
func TestDesktopMessagesExcludeModelAndTaskContent(t *testing.T) {
	for _, platform := range []string{"darwin", "linux"} {
		for _, status := range []string{"succeeded", "failed", "cancelled", `injected\"; shell`} {
			name, args, input, err := notificationCommand(platform, status)
			if err != nil || name == "" || strings.Contains(strings.Join(args, " ")+input, "injected") {
				t.Fatal("notification contains uncontrolled content")
			}
			if platform == "darwin" && (name != "/usr/bin/osascript" || len(args) != 1 || args[0] != "-") {
				t.Fatal("script did not use stdin")
			}
		}
	}
}
func TestOriginalManifestDigestRemainsStableWithoutNewPermissions(t *testing.T) {
	p := Proposal{ID: "fixture", Revision: 1, RuntimeID: "runtime", Owner: "alice", Manifest: testManifest()}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), "webSearch") || strings.Contains(string(raw), "desktopNotifications") {
		t.Fatal("default permissions changed legacy approval serialization")
	}
	before := p.Digest("https://example.com")
	p.Manifest.DesktopNotifications = true
	if before == p.Digest("https://example.com") {
		t.Fatal("new notification permission not included in consent")
	}
}
func TestNotificationIsPersistedAndClaimedOnce(t *testing.T) {
	a := testAgent(t)
	_, err := a.db.Exec(`INSERT INTO personal_local_runs(id,schedule_id,slot,day,started,status,share) VALUES('notice','task','slot','day','date','running',0)`)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.finishRun("notice", "failed", "provider error", "", nil, false, true); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	a.notify = func(context.Context, string) error { calls.Add(1); return nil }
	a.notifyRun("notice", "failed")
	a.notifyRun("notice", "failed")
	var outcome string
	_ = a.db.QueryRow(`SELECT notification FROM personal_run_details WHERE run_id='notice'`).Scan(&outcome)
	if calls.Load() != 1 || outcome != "submitted" {
		t.Fatalf("duplicate or unrecorded alert: %d %s", calls.Load(), outcome)
	}
}

func TestRestartDoesNotReplayAmbiguousDesktopSubmission(t *testing.T) {
	a := testAgent(t)
	_, err := a.db.Exec(`INSERT INTO personal_local_runs(id,schedule_id,slot,day,started,status,share) VALUES('restart-notice','task','slot','day','date','running',0)`)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.finishRun("restart-notice", "succeeded", "", "Saved private report", nil, false, true); err != nil {
		t.Fatal(err)
	}
	_, err = a.db.Exec(`UPDATE personal_run_details SET notification='attempting' WHERE run_id='restart-notice'`)
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	reopened, err := OpenAgent(a.vault)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var alerts atomic.Int32
	reopened.notify = func(context.Context, string) error { alerts.Add(1); return nil }
	reopened.notifyRun("restart-notice", "succeeded")
	runs, err := reopened.Results(10, false)
	if err != nil || len(runs) != 1 || runs[0].State != "succeeded" || runs[0].Result != "Saved private report" || runs[0].Notification != "unknown" || alerts.Load() != 0 {
		t.Fatalf("restart replayed/lost delivery or result: %+v %v", runs, err)
	}
}

func TestCitationsRespectSavedOutputLimit(t *testing.T) {
	a := testAgent(t)
	p := propose(a)
	p.Manifest.WebSearch = &provider.WebSearch{}
	p.Manifest.OutputLimit = 128
	p.Manifest.ShareResults = true
	a.inbox[p.ID] = p
	preview, _ := a.Preview(p.ID)
	if _, err := a.Approve(p.ID, preview.Digest); err != nil {
		t.Fatal(err)
	}
	a.generate = func(context.Context, provider.Request) (provider.Result, error) {
		return provider.Result{Text: strings.Repeat("a", 126) + "☀[2]", WebSearched: true, Citations: []provider.Citation{{URL: "https://example.com", Start: 0, End: 1}, {URL: "https://example.com/tail", Start: 127, End: 130}}}, nil
	}
	now := time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
	a.boot = now.Add(-time.Minute)
	_ = a.Pause(false)
	if err := a.Tick(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	a.wg.Wait()
	report, err := a.Report()
	if err != nil || len(report.Runs) != 1 || len(report.Runs[0].Result) != 126 || len(report.Runs[0].Citations) != 1 || !strings.Contains(report.Runs[0].Message, "truncated") {
		t.Fatalf("truncated citation or partial Unicode escaped limit: %+v %v", report, err)
	}
}
