package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sluicework/internal/state"
)

type fakeActivity struct {
	runs      []workflowRun
	jobs      []workflowJob
	err       error
	jobCalls  int
	requested chan struct{}
	release   chan struct{}
}

func (f *fakeActivity) ActivityRuns(ctx context.Context) ([]workflowRun, error) {
	if f.requested != nil {
		close(f.requested)
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.runs, f.err
}
func (f *fakeActivity) ActivityRun(context.Context, int64) (workflowRun, error) {
	return f.runs[0], f.err
}
func (f *fakeActivity) ActivityJobs(context.Context, int64) ([]workflowJob, error) {
	f.jobCalls++
	return f.jobs, f.err
}

func activityFixture(t *testing.T) (*Broker, *state.Store, state.Machine, *fakeActivity) {
	t.Helper()
	b, s, m, _ := brokerFixture(t)
	if _, err := b.db.Exec(activitySchema); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	f := &fakeActivity{runs: []workflowRun{{ID: 100, Name: "ARM build", Branch: "main", SHA: strings.Repeat("a", 40), Attempt: 1, Status: "in_progress", Updated: now}},
		jobs: []workflowJob{{ID: 200, RunID: 100, Attempt: 1, Name: "build", RunnerID: 42, RunnerName: "sluicework-" + m.ID, Labels: []string{"self-hosted", "macOS", "ARM64", "sluicework-" + m.ID[:12]}, Status: "in_progress", StartedAt: &now}}}
	f.jobs[0].Steps = append(f.jobs[0].Steps, struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}{"npm test", "in_progress"})
	b.activityClient = f
	return b, s, m, f
}

func TestActivityVerifiesLocalAssignmentAndRetainsHistoryAcrossRestart(t *testing.T) {
	b, _, m, f := activityFixture(t)
	registration := m.Runner
	cloud := f.jobs[0]
	cloud.ID = 201
	cloud.RunnerName = "GitHub Actions hosted runner"
	wrongLabel := f.jobs[0]
	wrongLabel.ID = 202
	wrongLabel.Labels = []string{"self-hosted"}
	wrongRun := f.jobs[0]
	wrongRun.ID = 203
	wrongRun.RunID = 101
	f.jobs = append(f.jobs, cloud, wrongLabel, wrongRun)
	b.RefreshActivity(context.Background(), []state.Machine{m})
	a, err := b.Activity(context.Background())
	if err != nil || len(a.Builds) != 1 || a.SyncedAt == nil || a.Issue != "" {
		t.Fatalf("bad activity: %+v %v", a, err)
	}
	build := a.Builds[0]
	if build.MachineID != m.ID || build.Step != "npm test" || build.RunnerID != 42 || build.URL != "https://github.com/team/pilot/actions/runs/100/job/200" {
		t.Fatalf("wrong local assignment: %+v", build)
	}
	// Completed records remain attributable after runner approval is removed.
	f.runs[0].Status = "completed"
	f.runs[0].Conclusion = "success"
	f.jobs = f.jobs[:1]
	f.jobs[0].Status = "completed"
	f.jobs[0].Conclusion = "success"
	ended := time.Now().UTC()
	f.jobs[0].CompletedAt = &ended
	m.Runner = nil
	b.activityNext = time.Time{}
	b.RefreshActivity(context.Background(), []state.Machine{m})
	restarted := &Broker{db: b.db, repository: b.repository, activityClient: f}
	a, err = restarted.Activity(context.Background())
	if err != nil || len(a.Builds) != 1 || a.Builds[0].Conclusion != "success" || a.Builds[0].CompletedAt == nil {
		t.Fatalf("completion lost on restart: %+v %v", a, err)
	}
	before := f.jobCalls
	m.Runner = registration
	restarted.RefreshActivity(context.Background(), []state.Machine{m})
	if f.jobCalls != before {
		t.Fatal("unchanged completed run refetched all jobs")
	}
}

func TestActivityTracksOlderActiveBuildAndPreservesLastKnownStateOnError(t *testing.T) {
	b, _, m, f := activityFixture(t)
	b.RefreshActivity(context.Background(), []state.Machine{m})
	// An active build falling out of recent discovery still gets refreshed.
	f.runs[0].Status = "completed"
	f.jobs[0].Status = "completed"
	f.jobs[0].Conclusion = "failure"
	old := f.runs[0]
	older := &olderActivity{fakeActivity: f, older: old}
	b.activityClient = older
	b.activityNext = time.Time{}
	b.RefreshActivity(context.Background(), []state.Machine{m})
	a, _ := b.Activity(context.Background())
	if !older.loaded || a.Builds[0].Conclusion != "failure" {
		t.Fatal("older active run was lost", a)
	}
	lastSuccessful := *a.SyncedAt
	f.err = HTTPError(403)
	b.activityNext = time.Time{}
	b.RefreshActivity(context.Background(), []state.Machine{m})
	a, _ = b.Activity(context.Background())
	if len(a.Builds) != 1 || !strings.Contains(a.Issue, "Actions: read") || a.SyncedAt == nil || !a.SyncedAt.Equal(lastSuccessful) {
		t.Fatalf("history lost or error hidden: %+v", a)
	}
}

type olderActivity struct {
	*fakeActivity
	older  workflowRun
	loaded bool
}

func (f *olderActivity) ActivityRuns(context.Context) ([]workflowRun, error) { return nil, f.err }
func (f *olderActivity) ActivityRun(context.Context, int64) (workflowRun, error) {
	f.loaded = true
	return f.older, f.err
}

func TestSlowActivitySyncDoesNotBlockRunnerControl(t *testing.T) {
	b, _, m, f := activityFixture(t)
	f.requested = make(chan struct{})
	f.release = make(chan struct{})
	done := make(chan struct{})
	go func() { b.RefreshActivity(context.Background(), []state.Machine{m}); close(done) }()
	<-f.requested
	defer func() { close(f.release); <-done }()
	control := make(chan error, 1)
	go func() { _, err := b.Act(context.Background(), m, "status"); control <- err }()
	select {
	case err := <-control:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("activity sync blocked runner control")
	}
}

func TestActivityHTTPPaginationAndCanonicalPaths(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		if r.Header.Get("Authorization") != "Bearer synthetic-actions" {
			t.Error("wrong activity credential")
		}
		if strings.HasSuffix(r.URL.Path, "/jobs") {
			fmt.Fprintf(w, `{"total_count":101,"jobs":[{"id":%s,"run_id":100,"status":"in_progress"}]}`, r.URL.Query().Get("page"))
		} else if r.URL.Query().Get("status") == "in_progress" {
			fmt.Fprintf(w, `{"total_count":101,"workflow_runs":[{"id":%s,"status":"in_progress"}]}`, r.URL.Query().Get("page"))
		} else {
			w.Write([]byte(`{"workflow_runs":[{"id":100,"status":"completed"}]}`))
		}
	}))
	defer server.Close()
	c := &Client{config: Config{Repository: "team/pilot"}, http: server.Client(), origin: server.URL, actionsToken: "synthetic-actions", actionsExpires: time.Now().Add(time.Hour)}
	runs, err := c.ActivityRuns(context.Background())
	if err != nil || len(runs) != 3 {
		t.Fatal(runs, err)
	}
	jobs, err := c.ActivityJobs(context.Background(), 100)
	if err != nil || len(jobs) != 2 || len(paths) != 5 {
		t.Fatal(jobs, paths, err)
	}
	for _, path := range paths {
		if !strings.HasPrefix(path, "/repos/team/pilot/actions/") {
			t.Fatal("scope escaped", path)
		}
	}
}

func TestActionsPermissionUsesSeparateTokenWithoutBreakingRunnerAccess(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body struct {
			Repositories []string
			Permissions  map[string]string
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(body.Repositories) != 1 || body.Repositories[0] != "pilot" || body.Permissions["actions"] != "read" || body.Permissions["administration"] != "" || len(body.Permissions) != 2 {
			t.Errorf("activity token is not read-only: %+v", body)
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer server.Close()
	c := &Client{config: Config{AppID: 123, InstallationID: 456, Repository: "team/pilot"}, key: key, http: server.Client(), origin: server.URL, token: "existing-runner-token", expires: time.Now().Add(time.Hour)}
	if _, err = c.installationToken(context.Background(), true); err != HTTPError(422) {
		t.Fatal(err)
	}
	token, err := c.accessToken(context.Background())
	if err != nil || token != "existing-runner-token" || requests != 1 {
		t.Fatal("activity permission broke runner access", err, requests)
	}
}

func TestSlowActionsAuthenticationDoesNotBlockRunnerToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	requested, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requested)
		<-release
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer server.Close()
	c := &Client{config: Config{AppID: 123, InstallationID: 456, Repository: "team/pilot"}, key: key, http: server.Client(), origin: server.URL, token: "existing-runner-token", expires: time.Now().Add(time.Hour)}
	done := make(chan struct{})
	go func() { _, _ = c.installationToken(context.Background(), true); close(done) }()
	<-requested
	defer func() { close(release); <-done }()
	control := make(chan string, 1)
	go func() { token, _ := c.accessToken(context.Background()); control <- token }()
	select {
	case token := <-control:
		if token != "existing-runner-token" {
			t.Fatal("runner token changed")
		}
	case <-time.After(time.Second):
		t.Fatal("Actions token acquisition blocked runner control")
	}
}

func TestActivityRefreshUsesOneMinuteBudgetWithoutBlockingRunnerControl(t *testing.T) {
	b, _, m, f := activityFixture(t)
	before := time.Now()
	b.RefreshActivity(context.Background(), []state.Machine{m})
	calls := f.jobCalls
	b.RefreshActivity(context.Background(), []state.Machine{m})
	if f.jobCalls != calls || b.activityNext.Before(before.Add(59*time.Second)) {
		t.Fatal("activity polling exceeded the one-minute budget")
	}
	if _, e := b.Act(context.Background(), m, "status"); e != nil {
		t.Fatal("activity budget blocked runner readiness", e)
	}
}
