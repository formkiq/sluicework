package githubqueue

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sluicework/internal/githubapp"
	"strings"
	"testing"
	"time"
)

type missingReader struct{}

func (missingReader) Get(context.Context, string) (Request, error) { return Request{}, ErrNotFound }

type mockRepository struct {
	call     func(string, string, any) (any, error)
	artifact []byte
	runners  []githubapp.Runner
}

func (m *mockRepository) QueueRequest(_ context.Context, method, path string, body, out any) error {
	v, err := m.call(method, path, body)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	data, _ := json.Marshal(v)
	return json.Unmarshal(data, out)
}
func (m *mockRepository) QueueArtifact(context.Context, int64, string) ([]byte, error) {
	return m.artifact, nil
}
func (m *mockRepository) QueueRunners(context.Context) ([]githubapp.Runner, error) {
	return m.runners, nil
}
func peerFixture(t *testing.T) (*Peer, *mockRepository, *RequestFile) {
	t.Helper()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	sha := strings.Repeat("a", 40)
	cfg := Config{BackendID: "test", AppID: 55, InstallationID: 66, Since: now.Add(-time.Hour), Primary: Target{"linux-x64", "linux-runner-01", "local"}, Fallback: Target{"macos-arm64", "mac-1", "local"}, FallbackMinutes: 2, Repositories: []Repository{{Name: "o/r", ID: 7, Branch: "main", HeartbeatSHA: sha, Priority: Low, CloudPlatform: "linux-arm64", Coordinators: []Coordinator{{File: "ci.yml", Kind: "site", Check: "build"}}}}}
	f := &RequestFile{Version: 3, BackendID: "test", ID: "00000000-0000-4000-8000-000000000001", ParentID: 1, ParentAttempt: 1, CheckID: 8, CheckSHA: sha, SourceSHA: sha, WorkflowSHA: sha, Ref: "main", Kind: "site", Placement: "prefer-local"}
	routing := githubapp.RoutingPolicy{Version: 3, Priority: "high", QueueMinutes: 15, Placement: "prefer-local", Platform: "linux-x64", MachineLabel: "linux-runner-01", FallbackRunner: &githubapp.RunnerSelector{Platform: "macos-arm64", MachineLabel: "mac-1"}, FallbackQueueMinutes: 2}
	manifest := githubapp.Integration{Version: 3, Platforms: []string{"linux-x64", "linux-arm64", "macos-arm64"}, Defaults: routing, Dispatcher: &githubapp.DispatcherBinding{AppID: 55, BackendID: "test", HeartbeatSHA: sha}}
	m := &mockRepository{}
	m.call = func(method, path string, body any) (any, error) {
		switch {
		case strings.Contains(path, "check_name=Sluicework+dispatcher"):
			return map[string]any{"total_count": 1, "check_runs": []any{map[string]any{"id": 9, "name": "Sluicework dispatcher", "head_sha": sha, "external_id": "sluicework-dispatcher:test", "app": map[string]int{"id": 55}, "output": map[string]string{"text": fmt.Sprintf(`{"at":%q}`, now.Format(time.RFC3339))}}}}, nil
		case strings.Contains(path, "/contents/.github/sluicework.json"):
			b, _ := json.Marshal(manifest)
			return map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString(b)}, nil
		case strings.HasSuffix(path, "/actions/variables/SLUICEWORK_ROUTING"):
			b, _ := json.Marshal(routing)
			return map[string]string{"value": string(b)}, nil
		case strings.Contains(path, "/actions/workflows/ci.yml/runs?"):
			return map[string]any{"total_count": 1, "workflow_runs": []any{map[string]any{"id": 1, "run_attempt": 1, "workflow_id": 4, "path": ".github/workflows/ci.yml", "event": "push", "head_sha": sha, "head_branch": "main", "status": "completed", "conclusion": "success", "repository": map[string]any{"id": 7, "full_name": "o/r", "private": true}, "head_repository": map[string]string{"full_name": "o/r"}}}}, nil
		case strings.HasSuffix(path, "/actions/workflows/ci.yml"):
			return map[string]int{"id": 4}, nil
		case strings.Contains(path, "/actions/runs/1/artifacts?"):
			return map[string]any{"total_count": 1, "artifacts": []any{map[string]any{"id": 44, "name": "sluicework-request-1", "digest": "sha256:" + strings.Repeat("d", 64), "size_in_bytes": 400, "created_at": now, "workflow_run": map[string]any{"id": 1, "head_sha": sha}}}}, nil
		case strings.Contains(path, "/git/ref/heads/main"):
			return map[string]any{"object": map[string]string{"sha": sha}}, nil
		case strings.HasSuffix(path, "/check-runs/8"):
			return map[string]any{"id": 8, "name": "build", "head_sha": sha, "status": "in_progress", "app": map[string]string{"slug": "github-actions"}, "external_id": "sluicework-v3:1:1:" + f.ID}, nil
		default:
			return nil, fmt.Errorf("unexpected request %s %s", method, path)
		}
	}
	data, _ := json.Marshal(f)
	m.artifact = data
	return &Peer{Config: cfg, Clients: map[string]RepositoryAPI{"o/r": m}, Store: missingReader{}, Now: func() time.Time { return now }}, m, f
}
func TestPeerAcceptsImmutableParentAndProtectedPriority(t *testing.T) {
	p, m, f := peerFixture(t)
	result, err := p.Discover(context.Background())
	if err != nil || len(result) != 1 {
		t.Fatal(result, err)
	}
	r := result[0]
	if r.Policy.Priority != High || r.Policy.Wait() != 15*time.Minute || r.Policy.PrimaryMinutes != 15 || r.ID != f.ID || r.ArtifactID != 44 {
		t.Fatal(r)
	}
	// Policy cannot be injected by adding fields to an artifact.
	m.artifact = append(m.artifact[:len(m.artifact)-1], []byte(`,"priority":"high"}`)...)
	if rows, err := p.Discover(context.Background()); err != nil || len(rows) != 0 {
		t.Fatal("unknown request field accepted", rows, err)
	}
}
func TestPeerRejectsForgedParentCheckAndTrailingMetadata(t *testing.T) {
	for _, mode := range []string{"check", "trailing", "backend"} {
		t.Run(mode, func(t *testing.T) {
			p, m, _ := peerFixture(t)
			old := m.call
			if mode == "check" {
				m.call = func(method, path string, body any) (any, error) {
					if strings.HasSuffix(path, "/check-runs/8") {
						return map[string]any{"id": 8, "app": map[string]string{"slug": "evil"}}, nil
					}
					return old(method, path, body)
				}
			}
			if mode == "trailing" {
				m.artifact = append(m.artifact, []byte(` {}`)...)
			}
			if mode == "backend" {
				m.artifact = []byte(strings.ReplaceAll(string(m.artifact), `"backendId":"test"`, `"backendId":"other"`))
			}
			result, err := p.Discover(context.Background())
			if mode == "backend" {
				if err != nil || len(result) != 0 {
					t.Fatal(result, err)
				}
			} else if err != nil || len(result) != 0 {
				t.Fatal("forged metadata accepted", result, err)
			}
		})
	}
}
func TestStaleUnstartedSourceDoesNotBlockOtherRepositories(t *testing.T) {
	p, m, _ := peerFixture(t)
	old := m.call
	m.call = func(method, path string, body any) (any, error) {
		if strings.Contains(path, "/git/ref/heads/") {
			return map[string]any{"object": map[string]string{"sha": strings.Repeat("b", 40)}}, nil
		}
		return old(method, path, body)
	}
	result, err := p.Discover(context.Background())
	if err != nil || len(result) != 0 {
		t.Fatal(result, err)
	}
}
func TestDispatchRefusesChangedSourceBeforeAnyPOST(t *testing.T) {
	p, m, _ := peerFixture(t)
	posts := 0
	m.call = func(method, path string, body any) (any, error) {
		if method == "POST" {
			posts++
		}
		if strings.HasSuffix(path, "/actions/runs/1") {
			return map[string]any{"run_attempt": 1, "status": "completed", "conclusion": "success"}, nil
		}
		return map[string]any{"object": map[string]string{"sha": strings.Repeat("b", 40)}}, nil
	}
	r := Request{Repository: "o/r", ParentID: 1, ParentAttempt: 1, Ref: "main", WorkflowSHA: strings.Repeat("a", 40)}
	if err := p.Dispatch(context.Background(), r); !errors.Is(err, ErrSuperseded) || posts != 0 {
		t.Fatal(posts, err)
	}
}

func TestDispatchSuppliesRequiredLabelForHostedFallback(t *testing.T) {
	for _, target := range []Target{{Platform: "linux-arm64", Host: "cloud"}, {Platform: "linux-x64", Label: "linux-runner-01", Host: "local"}} {
		p, m, _ := peerFixture(t)
		sha := strings.Repeat("a", 40)
		posts := 0
		m.call = func(method, path string, body any) (any, error) {
			if strings.HasSuffix(path, "/actions/runs/1") {
				return map[string]any{"run_attempt": 1, "status": "completed", "conclusion": "success"}, nil
			}
			if strings.Contains(path, "/git/ref/heads/") {
				return map[string]any{"object": map[string]string{"sha": sha}}, nil
			}
			if method != "POST" || !strings.HasSuffix(path, "/sluicework-build.yml/dispatches") {
				t.Fatal(method, path)
			}
			posts++
			inputs := body.(map[string]any)["inputs"].(map[string]string)
			want := target.Label
			if target.Host == "cloud" {
				want = "unused"
			}
			if inputs["machine_label"] != want || inputs["host"] != target.Host || inputs["local_platform"] != target.Platform {
				t.Fatal("workflow input contract differs", inputs)
			}
			return nil, nil
		}
		r := Request{Repository: "o/r", ParentID: 1, ParentAttempt: 1, Ref: "main", WorkflowSHA: sha, Attempt: &Attempt{RequestID: "one", Target: target}}
		if err := p.Dispatch(context.Background(), r); err != nil || posts != 1 {
			t.Fatal(posts, err)
		}
	}
}
func TestInventoryCountsOnlyIdleMatchingRunners(t *testing.T) {
	p, m, _ := peerFixture(t)
	p.inventory = map[string][]githubapp.Runner{}
	m.runners = []githubapp.Runner{{ID: 1, Status: "online", Labels: []githubapp.RunnerLabel{{Name: "Linux"}, {Name: "X64"}, {Name: "linux-runner-01"}}}, {ID: 2, Status: "online", Busy: true, Labels: []githubapp.RunnerLabel{{Name: "Linux"}, {Name: "X64"}, {Name: "linux-runner-01"}}}}
	n, err := p.Available(context.Background(), Request{Repository: "o/r"}, p.Config.Primary)
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func TestCoordinatorMayRemoveCloudFallbackButCannotEnableLocalExecutionAgainstCloudOnly(t *testing.T) {
	for _, published := range []string{"prefer-local", "cloud-only"} {
		t.Run(published, func(t *testing.T) {
			p, m, f := peerFixture(t)
			f.Placement = "local-only"
			m.artifact, _ = json.Marshal(f)
			old := m.call
			m.call = func(method, path string, body any) (any, error) {
				result, e := old(method, path, body)
				if e != nil {
					return result, e
				}
				if strings.HasSuffix(path, "/actions/variables/SLUICEWORK_ROUTING") {
					v := result.(map[string]string)
					var routing githubapp.RoutingPolicy
					if e = json.Unmarshal([]byte(v["value"]), &routing); e != nil {
						return nil, e
					}
					routing.Placement = published
					b, _ := json.Marshal(routing)
					return map[string]string{"value": string(b)}, nil
				}
				return result, nil
			}
			rows, e := p.Discover(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			if published == "prefer-local" {
				if len(rows) != 1 || rows[0].Placement != "local-only" {
					t.Fatal("local-only restriction was not preserved", rows)
				}
			} else if len(rows) != 0 {
				t.Fatal("cloud-only policy was weakened", rows)
			}
		})
	}
}

func TestLinuxPoolCountsBothArchitecturesWithoutCustomLabels(t *testing.T) {
	p, m, _ := peerFixture(t)
	p.inventory = map[string][]githubapp.Runner{}
	runner := func(id int64, labels ...string) githubapp.Runner {
		r := githubapp.Runner{ID: id, Status: "online", GroupName: "Default"}
		for _, name := range labels {
			r.Labels = append(r.Labels, githubapp.RunnerLabel{Name: name})
		}
		return r
	}
	m.runners = []githubapp.Runner{runner(1, "self-hosted", "Linux", "X64", "linux-runner-01"), runner(2, "self-hosted", "Linux", "ARM64"), runner(3, "self-hosted", "Linux"), runner(4, "self-hosted", "macOS", "X64"), runner(5, "Linux", "X64"), runner(6, "self-hosted", "Linux"), runner(7, "self-hosted", "Linux")}
	spotRunner := runner(8, "self-hosted", "Linux", "ARM64", "sluicework-spot-fixture")
	spotRunner.GroupName = "Sluicework Spot"
	m.runners = append(m.runners, spotRunner)
	m.runners[5].Busy = true
	m.runners[6].Status = "offline"
	n, err := p.Available(context.Background(), Request{Repository: "o/r"}, Target{Platform: "linux", Label: "self-hosted", Host: "local"})
	if err != nil || n != 3 {
		t.Fatalf("generic Linux capacity = %d: %v", n, err)
	}
}

func TestWorkflowIdentityCacheIsBoundedAndDoesNotCacheFailures(t *testing.T) {
	p, m, _ := peerFixture(t)
	old := m.call
	calls := 0
	id := int64(4)
	fail := false
	m.call = func(method, path string, body any) (any, error) {
		if strings.HasSuffix(path, "/actions/workflows/ci.yml") {
			calls++
			if fail {
				return nil, errors.New("temporarily unavailable")
			}
			return map[string]int64{"id": id}, nil
		}
		return old(method, path, body)
	}
	ctx := context.Background()
	now := p.now()
	p.Now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if got, e := p.workflowID(ctx, "o/r", "ci.yml"); e != nil || got != 4 {
			t.Fatal(got, e)
		}
	}
	if calls != 1 {
		t.Fatal("workflow identity fetched on every poll", calls)
	}
	if _, e := p.workflowID(ctx, "other/repository", "ci.yml"); e == nil {
		t.Fatal("workflow cache crossed repository scope")
	}
	now = now.Add(5 * time.Minute)
	fail = true
	if _, e := p.workflowID(ctx, "o/r", "ci.yml"); e == nil {
		t.Fatal("expired identity hid a provider error")
	}
	fail = false
	id = 5
	if got, e := p.workflowID(ctx, "o/r", "ci.yml"); e != nil || got != 5 {
		t.Fatal("new workflow identity was not fetched", got, e)
	}
	if calls != 3 {
		t.Fatal(calls)
	}
}

func TestHeartbeatLookupIsCachedOnlyUntilAuthenticatedRefreshDeadline(t *testing.T) {
	p, m, _ := peerFixture(t)
	old := m.call
	now := p.now()
	p.Now = func() time.Time { return now }
	reads, writes := 0, 0
	fail := false
	m.call = func(method, path string, body any) (any, error) {
		if strings.Contains(path, "check_name=Sluicework+dispatcher") {
			reads++
			if fail {
				return nil, errors.New("provider unavailable")
			}
		}
		if method == "PATCH" && strings.HasSuffix(path, "/check-runs/9") {
			writes++
			return nil, nil
		}
		return old(method, path, body)
	}
	ctx := context.Background()
	if e := p.heartbeat(ctx); e != nil {
		t.Fatal(e)
	}
	now = now.Add(time.Minute)
	if e := p.heartbeat(ctx); e != nil {
		t.Fatal(e)
	}
	if reads != 1 || writes != 0 {
		t.Fatal("fresh owned heartbeat was repeatedly fetched", reads, writes)
	}
	now = now.Add(4 * time.Minute)
	fail = true
	if e := p.heartbeat(ctx); e == nil {
		t.Fatal("expired heartbeat hid provider failure")
	}
	fail = false
	if e := p.heartbeat(ctx); e != nil {
		t.Fatal(e)
	}
	if reads != 3 || writes != 1 {
		t.Fatal("failed refresh was cached or due refresh skipped", reads, writes)
	}
	now = now.Add(time.Minute)
	if e := p.heartbeat(ctx); e != nil {
		t.Fatal(e)
	}
	if reads != 3 {
		t.Fatal("just-published heartbeat was not cached")
	}
}
