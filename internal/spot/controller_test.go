package spot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryStore struct {
	mu sync.Mutex
	s  State
}

func (m *memoryStore) Load(context.Context) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneState(m.s), nil
}
func (m *memoryStore) Save(_ context.Context, s *State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.Version != m.s.Version {
		return ErrConflict
	}
	s.Version++
	m.s = cloneState(*s)
	return nil
}

type fakeCloud struct {
	instances map[string]Instance
	byToken   map[string]string
	calls     []LaunchSpec
	uncertain bool
	rejected  bool
}

func (f *fakeCloud) Launch(_ context.Context, s LaunchSpec) (string, error) {
	f.calls = append(f.calls, s)
	if f.rejected {
		return "", ErrLaunchRejected
	}
	id := f.byToken[s.ClientToken]
	if id == "" {
		id = fmt.Sprintf("i-%017x", len(f.byToken)+1)
		f.byToken[s.ClientToken] = id
		f.instances[id] = Instance{ID: id, State: "running", Owned: true}
	}
	if f.uncertain {
		f.uncertain = false
		return "", errors.New("connection lost after instance creation")
	}
	return id, nil
}
func (f *fakeCloud) Inspect(_ context.Context, id string) (Instance, error) {
	v, ok := f.instances[id]
	if !ok {
		return Instance{}, errors.New("instance not yet visible")
	}
	return v, nil
}
func (f *fakeCloud) Terminate(_ context.Context, id string) error {
	v := f.instances[id]
	v.State = "shutting-down"
	f.instances[id] = v
	return nil
}

type fakeRunners struct {
	r       map[int64]Runner
	names   map[string]int64
	created int
}

func (f *fakeRunners) Create(_ context.Context, name, label string) (Registration, error) {
	f.created++
	id := int64(f.created)
	f.r[id] = Runner{ID: id, Online: true, Labels: []string{"self-hosted", "Linux", "ARM64", label}}
	f.names[name] = id
	return Registration{ID: id, JIT: "T05FX0pPQl9TRUNSRVQ=", Package: Package{"https://github.com/actions/runner/releases/download/v2.338.2/actions-runner-linux-arm64-2.338.2.tar.gz", strings.Repeat("a", 64)}}, nil
}
func (f *fakeRunners) Find(_ context.Context, name string) (Runner, bool, error) {
	r, ok := f.r[f.names[name]]
	return r, ok, nil
}
func (f *fakeRunners) Get(_ context.Context, id int64) (Runner, bool, error) {
	r, ok := f.r[id]
	return r, ok, nil
}
func (f *fakeRunners) Remove(_ context.Context, id int64) error {
	if f.r[id].Busy {
		return errors.New("runner is busy")
	}
	delete(f.r, id)
	return nil
}
func fixture(t *testing.T) (*Controller, *memoryStore, *fakeCloud, *fakeRunners, *time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 22, 22, 0, 0, 0, time.UTC)
	s := &memoryStore{}
	cloud := &fakeCloud{instances: map[string]Instance{}, byToken: map[string]string{}}
	runners := &fakeRunners{r: map[int64]Runner{}, names: map[string]int64{}}
	cfg := Infrastructure{AccountID: "123456789012", RootDeviceName: "/dev/sda1", Region: "eu-central-1", ImageID: "ami-00000000000000001", SubnetIDs: []string{"subnet-00000000000000001", "subnet-00000000000000002"}, SecurityGroupID: "sg-00000000000000001", RunnerGroupID: 4, ControlRepository: "org/one", VolumeGiB: 32}
	settings := Defaults("backend")
	settings.Enabled = true
	settings.Repositories = map[string]string{"org/one": "standard", "org/two": "standard"}
	c := &Controller{BackendID: "backend", Infrastructure: cfg, Approved: []string{"org/one", "org/two"}, Store: s, Cloud: cloud, Runners: runners, Settings: settings, Now: func() time.Time { return now }}
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
	return c, s, cloud, runners, &now
}
func job(id, repository string) Job {
	return Job{Kind: "site", ID: id, Repository: repository, WorkflowRef: repository + "/.github/workflows/sluicework-build.yml@refs/heads/main", WorkflowSHA: strings.Repeat("a", 40), SourceSHA: strings.Repeat("b", 40), RequestID: "123-1-" + id + "-local", Policy: Policy{"standard", 5, 10, 90}}
}
func TestThreeSlotsAreGlobalAcrossRepositoriesAndFourthWaits(t *testing.T) {
	c, s, cloud, _, _ := fixture(t)
	for n := 0; n < 3; n++ {
		repo := "org/one"
		if n == 1 {
			repo = "org/two"
		}
		if _, ready, e := c.Ensure(context.Background(), job(fmt.Sprintf("job%d", n), repo)); e != nil || !ready {
			t.Fatal(ready, e)
		}
	}
	if _, _, e := c.Ensure(context.Background(), job("fourth", "org/two")); !errors.Is(e, ErrCapacity) {
		t.Fatal(e)
	}
	state, _ := s.Load(context.Background())
	if len(state.Allocations) != 3 || len(cloud.instances) != 3 {
		t.Fatal("fleet exceeded global cap", len(state.Allocations), len(cloud.instances))
	}
}
func TestSlotCannotBeReusedBeforePositiveTermination(t *testing.T) {
	c, _, cloud, _, _ := fixture(t)
	ctx := context.Background()
	for n := 0; n < 3; n++ {
		_, _, e := c.Ensure(ctx, job(fmt.Sprintf("job%d", n), "org/one"))
		if e != nil {
			t.Fatal(e)
		}
	}
	if done, e := c.Retire(ctx, "job0"); e != nil || done {
		t.Fatal("slot released while instance is terminating", done, e)
	}
	if _, _, e := c.Ensure(ctx, job("fourth", "org/two")); !errors.Is(e, ErrCapacity) {
		t.Fatal("terminating slot was reused", e)
	}
	state, _ := c.Store.Load(ctx)
	id := state.Allocations["job0"].InstanceID
	v := cloud.instances[id]
	v.State = "terminated"
	cloud.instances[id] = v
	if done, e := c.Retire(ctx, "job0"); e != nil || !done {
		t.Fatal(done, e)
	}
	if _, ready, e := c.Ensure(ctx, job("fourth", "org/two")); e != nil || !ready {
		t.Fatal(ready, e)
	}
}
func TestLostLaunchResponseReusesExactTokenAZAndSecrets(t *testing.T) {
	c, _, cloud, runners, _ := fixture(t)
	cloud.uncertain = true
	ctx := context.Background()
	j := job("one", "org/one")
	if _, _, e := c.Ensure(ctx, j); !errors.Is(e, ErrUncertain) {
		t.Fatal(e)
	}
	if done, e := c.Retire(ctx, j.ID); done || !errors.Is(e, ErrUncertain) {
		t.Fatal("uncertain reservation was released", done, e)
	}
	if _, ready, e := c.Ensure(ctx, j); e != nil || !ready {
		t.Fatal(ready, e)
	}
	if len(cloud.instances) != 1 || runners.created != 1 || len(cloud.calls) != 2 || !reflect.DeepEqual(cloud.calls[0], cloud.calls[1]) {
		t.Fatal("launch retry changed immutable parameters or created duplicate resources")
	}
}
func TestPublicStatusNeverContainsJITOrLaunchSecrets(t *testing.T) {
	c, _, _, _, _ := fixture(t)
	if _, _, e := c.Ensure(context.Background(), job("one", "org/one")); e != nil {
		t.Fatal(e)
	}
	status, e := c.Status(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(status)
	for _, secret := range []string{"ONE_JOB_SECRET", "jit", "imageId", "subnetId", "clientToken", "package"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("private launch material leaked", secret)
		}
	}
}
func TestDisabledAdmissionPreservesRunningAllocations(t *testing.T) {
	c, s, _, _, _ := fixture(t)
	ctx := context.Background()
	_, _, _ = c.Ensure(ctx, job("one", "org/one"))
	settings := c.Settings
	settings.Enabled = false
	if e := c.SetSettings(settings); e != nil {
		t.Fatal(e)
	}
	if _, _, e := c.Ensure(ctx, job("two", "org/two")); !errors.Is(e, ErrDisabled) {
		t.Fatal(e)
	}
	state, _ := s.Load(ctx)
	if len(state.Allocations) != 1 {
		t.Fatal("disabling admission terminated a running build")
	}
}
func TestMaximumLifetimeTerminatesEvenWithoutGitHubDiscovery(t *testing.T) {
	c, s, cloud, _, now := fixture(t)
	ctx := context.Background()
	_, _, _ = c.Ensure(ctx, job("one", "org/one"))
	*now = now.Add(91 * time.Minute)
	if e := c.Reconcile(ctx); e != nil {
		t.Fatal(e)
	}
	state, _ := s.Load(ctx)
	id := state.Allocations["one"].InstanceID
	if cloud.instances[id].State != "shutting-down" {
		t.Fatal("expired instance remained running")
	}
}
func TestChangedRequestCannotReuseReservation(t *testing.T) {
	c, _, _, _, _ := fixture(t)
	ctx := context.Background()
	j := job("one", "org/one")
	_, _, _ = c.Ensure(ctx, j)
	j.SourceSHA = strings.Repeat("c", 40)
	if _, _, e := c.Ensure(ctx, j); e == nil {
		t.Fatal("reservation rebound to another source")
	}
}
func TestFleetSettingsRejectOverLimitAndUnapprovedRepositories(t *testing.T) {
	c, _, _, _, _ := fixture(t)
	for _, cap := range []int{-1, 4, 100} {
		s := c.Settings
		s.MaxConcurrent = cap
		if s.Validate(c.BackendID, c.Approved) == nil {
			t.Fatal("invalid cap accepted", cap)
		}
	}
	s := c.Settings
	s.Repositories = map[string]string{"evil/repo": "standard"}
	if s.Validate(c.BackendID, c.Approved) == nil {
		t.Fatal("foreign repository accepted")
	}
	raw, _ := json.Marshal(c.Settings)
	bad := strings.TrimSuffix(string(raw), "}") + `,"privateKey":"bad"}`
	if _, e := ParseSettings(bad, c.BackendID, c.Approved); e == nil {
		t.Fatal("unknown settings field accepted")
	}
}

func (f *fakeCloud) Inventory(context.Context) ([]ManagedInstance, error) {
	out := []ManagedInstance{}
	for token, id := range f.byToken {
		out = append(out, ManagedInstance{Instance: f.instances[id], Token: token})
	}
	return out, nil
}
func TestReconcileRecoversLostLaunchAndReapsOrphanBeforeAdmission(t *testing.T) {
	c, _, cloud, _, _ := fixture(t)
	ctx := context.Background()
	cloud.uncertain = true
	_, _, _ = c.Ensure(ctx, job("one", "org/one"))
	if e := c.Reconcile(ctx); e != nil {
		t.Fatal(e)
	}
	if done, e := c.Retire(ctx, "one"); e != nil || done {
		t.Fatal(done, e)
	}
	cloud.byToken["orphan-token"] = "i-000000000000000ff"
	cloud.instances["i-000000000000000ff"] = Instance{ID: "i-000000000000000ff", Owned: true, State: "running"}
	if e := c.Reconcile(ctx); e != nil {
		t.Fatal(e)
	}
	if _, _, e := c.Ensure(ctx, job("two", "org/two")); !errors.Is(e, ErrUncertain) {
		t.Fatal("orphan must block new launches", e)
	}
	if cloud.instances["i-000000000000000ff"].State != "shutting-down" {
		t.Fatal("orphan left running")
	}
}

func TestOrphanBlocksPreviouslyReservedMachineFromIncreasingPhysicalFleet(t *testing.T) {
	c, _, cloud, runners, _ := fixture(t)
	ctx := context.Background()
	j := job("pending", "org/one")
	a, e := c.reserve(ctx, j)
	if e != nil {
		t.Fatal(e)
	}
	grant, e := runners.Create(ctx, a.Spec.Name, a.Spec.Label)
	if e != nil {
		t.Fatal(e)
	}
	a.RunnerID = grant.ID
	a.Spec.JIT = grant.JIT
	a.Spec.Package = grant.Package
	a.State = "registered"
	if e = c.update(ctx, a); e != nil {
		t.Fatal(e)
	}
	cloud.byToken["orphan"] = "i-000000000000000ff"
	cloud.instances["i-000000000000000ff"] = Instance{ID: "i-000000000000000ff", State: "running", Owned: true}
	if e = c.Reconcile(ctx); e != nil {
		t.Fatal(e)
	}
	if _, _, e = c.Ensure(ctx, j); !errors.Is(e, ErrUncertain) || len(cloud.calls) != 0 {
		t.Fatal("new machine launched while orphan termination remained unconfirmed", e)
	}
}

func TestConfirmedTerminationSurvivesEC2RecordExpiryAndRetainsRunnerSafety(t *testing.T) {
	c, s, cloud, runners, now := fixture(t)
	ctx := context.Background()
	j := job("gone", "org/one")
	if _, _, e := c.Ensure(ctx, j); e != nil {
		t.Fatal(e)
	}
	state, _ := s.Load(ctx)
	a := state.Allocations[j.ID]
	v := cloud.instances[a.InstanceID]
	v.State = "terminated"
	v.Reason = "Client.InstanceInitiatedShutdown"
	cloud.instances[a.InstanceID] = v
	if out, found, e := c.Observe(ctx, j.ID); e != nil || !found || out.State != "terminated" {
		t.Fatal(out, found, e)
	}
	delete(cloud.instances, a.InstanceID)
	*now = now.Add(2 * time.Hour)
	if e := c.Reconcile(ctx); e != nil {
		t.Fatal(e)
	}
	if len(cloud.instances) != 0 {
		t.Fatal("terminated receipt caused another EC2 mutation")
	}
	if out, found, e := c.Observe(ctx, j.ID); e != nil || !found || out.EndReason != v.Reason {
		t.Fatal(out, found, e)
	}
	r := runners.r[a.RunnerID]
	r.Busy = true
	runners.r[a.RunnerID] = r
	if done, e := c.Retire(ctx, j.ID); e != nil || done {
		t.Fatal("busy registration must retain receipt", done, e)
	}
	r.Busy = false
	runners.r[a.RunnerID] = r
	if done, e := c.Retire(ctx, j.ID); e != nil || !done {
		t.Fatal(done, e)
	}
	state, _ = s.Load(ctx)
	if len(state.Allocations) != 0 || len(runners.r) != 0 {
		t.Fatal("retired receipt or registration retained")
	}
}

func TestRetirePersistsTerminationBeforeWaitingForRunner(t *testing.T) {
	c, s, cloud, runners, _ := fixture(t)
	ctx := context.Background()
	j := job("persist", "org/one")
	_, _, _ = c.Ensure(ctx, j)
	state, _ := s.Load(ctx)
	a := state.Allocations[j.ID]
	v := cloud.instances[a.InstanceID]
	v.State = "terminated"
	v.Reason = "Client.InstanceInitiatedShutdown"
	cloud.instances[a.InstanceID] = v
	r := runners.r[a.RunnerID]
	r.Busy = true
	runners.r[a.RunnerID] = r
	if done, e := c.Retire(ctx, j.ID); e != nil || done {
		t.Fatal(done, e)
	}
	state, _ = s.Load(ctx)
	if state.Allocations[j.ID].State != "terminated" {
		t.Fatal("positive termination evidence was not persisted")
	}
	delete(cloud.instances, a.InstanceID)
	r.Busy = false
	runners.r[a.RunnerID] = r
	if done, e := c.Retire(ctx, j.ID); e != nil || !done {
		t.Fatal(done, e)
	}
}

func TestMissingInstanceWithoutConfirmedTerminationKeepsReservation(t *testing.T) {
	c, s, cloud, runners, _ := fixture(t)
	ctx := context.Background()
	j := job("unknown", "org/one")
	_, _, _ = c.Ensure(ctx, j)
	state, _ := s.Load(ctx)
	a := state.Allocations[j.ID]
	delete(cloud.instances, a.InstanceID)
	if done, e := c.Retire(ctx, j.ID); e == nil || done {
		t.Fatal("unknown instance was treated as terminated", done, e)
	}
	state, _ = s.Load(ctx)
	if len(state.Allocations) != 1 || len(runners.r) != 1 {
		t.Fatal("unknown reservation or registration was removed")
	}
}
