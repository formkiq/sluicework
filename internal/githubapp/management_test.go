package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"sluicework/internal/state"
)

type fakeManagement struct {
	repo                            []Runner
	org                             []OrganizationRunner
	orgErr                          error
	integration                     Integration
	raw                             string
	exists                          bool
	legacy                          map[string]string
	writes, labels, deletes, tokens int
	busyOnGet                       bool
}

func (f *fakeManagement) ListRunners(context.Context) ([]Runner, error) { return f.repo, nil }
func (f *fakeManagement) OrganizationInventory(context.Context) ([]OrganizationRunner, error) {
	return f.org, f.orgErr
}
func (f *fakeManagement) ScopedRunner(_ context.Context, scope string, id int64) (Runner, error) {
	for _, r := range f.repo {
		if scope == "repository" && r.ID == id {
			r.Busy = r.Busy || f.busyOnGet
			return r, nil
		}
	}
	for _, r := range f.org {
		if scope == "organization" && r.ID == id {
			x := r.Runner
			x.Busy = x.Busy || f.busyOnGet
			return x, nil
		}
	}
	return Runner{}, HTTPError(404)
}
func (f *fakeManagement) SetRunnerLabels(_ context.Context, _ string, _ int64, _ []string) error {
	f.labels++
	return nil
}
func (f *fakeManagement) RemoveRunner(context.Context, string, int64) error { f.deletes++; return nil }
func (f *fakeManagement) Registration(context.Context, string) (RegistrationToken, error) {
	f.tokens++
	return RegistrationToken{"synthetic-registration-secret", time.Now().Add(time.Hour)}, nil
}
func (f *fakeManagement) Applications(context.Context, string) ([]RunnerApplication, error) {
	return []RunnerApplication{{OS: "linux", Architecture: "x64", Filename: "actions-runner-linux-x64-2.338.2.tar.gz", DownloadURL: "https://github.com/actions/runner/releases/download/v2.338.2/actions-runner-linux-x64-2.338.2.tar.gz", SHA256: strings.Repeat("a", 64)}}, nil
}
func (f *fakeManagement) Integration(context.Context) (Integration, error) { return f.integration, nil }
func (f *fakeManagement) Variable(context.Context, string) (string, bool, error) {
	return f.raw, f.exists, nil
}
func (f *fakeManagement) LegacyVariables(context.Context) (map[string]string, error) {
	return f.legacy, nil
}
func (f *fakeManagement) SaveRouting(_ context.Context, raw string, _ bool) error {
	f.raw, f.exists = raw, true
	f.writes++
	return nil
}
func managementFixture(t *testing.T) (*Broker, *fakeManagement, state.Machine) {
	b, _, m, _ := brokerFixture(t)
	labels := []RunnerLabel{{"self-hosted", "read-only"}, {"Linux", "read-only"}, {"X64", "read-only"}, {"linux-runner-01", "custom"}}
	f := &fakeManagement{org: []OrganizationRunner{{Runner: Runner{ID: 123, Name: "linux-runner-01", OS: "linux", Status: "online", Labels: labels}, Group: RunnerGroup{ID: 2, Name: "Default"}, Available: true}}, legacy: map[string]string{}}
	f.integration = Integration{Version: 1, Variable: RoutingVariable, Workflow: ".github/workflows/main.yml", Platforms: []string{"macos-arm64", "linux-arm64", "linux-x64"}, Defaults: RoutingPolicy{Version: 1, Placement: "prefer-local", Platform: "macos-arm64", MachineLabel: "sluicework-" + m.ID[:12], QueueMinutes: 2}}
	b.management = f
	b.organization = true
	return b, f, m
}
func TestRunnerInventoryIncludesOrganizationAccessAndDeterministicTargets(t *testing.T) {
	b, f, m := managementFixture(t)
	ctx := context.Background()
	f.repo = []Runner{{ID: 42, Name: "sluicework-" + m.ID, OS: "macOS", Status: "online", Labels: []RunnerLabel{{"macOS", "read-only"}, {"ARM64", "read-only"}, {"sluicework-" + m.ID[:12], "custom"}}}}
	inventory, err := b.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Runners) != 2 || !inventory.Runners[0].Managed || inventory.Runners[1].Scope != "organization" || len(inventory.Targets) != 3 {
		t.Fatal("incorrect inventory", inventory)
	}
	f.org[0].Available = false
	inventory, err = b.Inventory(ctx)
	if err != nil || len(inventory.Targets) != 1 {
		t.Fatal("unshared org runner selectable", inventory, err)
	}
	f.org[0].Available = true
	f.org[0].Group.Restricted = true
	inventory, _ = b.Inventory(ctx)
	if len(inventory.Targets) != 1 {
		t.Fatal("workflow-restricted group selectable")
	}
	f.org[0].Group.Restricted = false
	other := f.org[0]
	other.ID = 124
	other.Name = "another-linux"
	f.org = append(f.org, other)
	inventory, _ = b.Inventory(ctx)
	if len(inventory.Targets) != 2 {
		t.Fatal("ambiguous label selectable", inventory.Targets)
	}
	f.orgErr = HTTPError(403)
	inventory, err = b.Inventory(ctx)
	if err != nil || inventory.OrganizationIssue == "" || len(inventory.Targets) != 1 {
		t.Fatal("org permission failure disabled managed capacity", err)
	}
}
func TestExternalRunnerChangesCannotTakeOverManagedRunnerOrInterruptBusyWork(t *testing.T) {
	b, f, m := managementFixture(t)
	ctx := context.Background()
	f.repo = []Runner{{ID: 42, Name: "sluicework-" + m.ID, Status: "online"}}
	if err := b.UpdateExternalLabels(ctx, "admin", "repository", 42, []string{"other"}); !errors.Is(err, state.ErrInvalid) {
		t.Fatal("managed labels changed", err)
	}
	for _, labels := range [][]string{{"Linux"}, {"a", "A"}, {"sluicework-stolen"}, {"bad\nvalue"}} {
		if err := b.UpdateExternalLabels(ctx, "admin", "organization", 123, labels); !errors.Is(err, state.ErrInvalid) {
			t.Fatal("unsafe labels accepted", labels, err)
		}
	}
	if f.labels != 0 {
		t.Fatal("failed validation mutated GitHub")
	}
	if err := b.UpdateExternalLabels(ctx, "admin", "organization", 123, []string{"linux-runner-01", "fast"}); err != nil || f.labels != 1 {
		t.Fatal(err)
	}
	f.busyOnGet = true
	if err := b.RemoveExternal(ctx, "admin", "organization", 123, "linux-runner-01"); !errors.Is(err, state.ErrConflict) || f.deletes != 0 {
		t.Fatal("assignment race removed busy runner", err)
	}
	f.busyOnGet = false
	if err := b.RemoveExternal(ctx, "admin", "organization", 123, "linux-runner-01"); !errors.Is(err, state.ErrConflict) {
		t.Fatal("online runner removed before its service stopped", err)
	}
	f.org[0].Status = "offline"
	if err := b.RemoveExternal(ctx, "admin", "organization", 123, "wrong"); !errors.Is(err, state.ErrInvalid) {
		t.Fatal(err)
	}
	if err := b.RemoveExternal(ctx, "admin", "organization", 123, "linux-runner-01"); err != nil || f.deletes != 1 {
		t.Fatal(err)
	}
}
func TestPlacementPublishesOneVersionedVariableAndRejectsStaleOrAmbiguousEdits(t *testing.T) {
	b, f, _ := managementFixture(t)
	ctx := context.Background()
	current, err := b.Routing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	policy := RoutingPolicy{Version: 1, Placement: "prefer-local", Platform: "linux-x64", MachineLabel: "linux-runner-01", QueueMinutes: 3}
	saved, err := b.PublishRouting(ctx, "admin", policy, current.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if f.writes != 1 || saved.Policy != policy || saved.Revision == current.Revision || saved.Source != "repository variable" {
		t.Fatal("publication not reflected", saved)
	}
	if _, err = b.PublishRouting(ctx, "admin", policy, current.Revision); !errors.Is(err, state.ErrConflict) || f.writes != 1 {
		t.Fatal("stale edit overwrote config", err)
	}
	policy.MachineLabel = "self-hosted"
	if _, err = b.PublishRouting(ctx, "admin", policy, saved.Revision); !errors.Is(err, state.ErrInvalid) {
		t.Fatal("broad target accepted")
	}
	policy.MachineLabel = "missing"
	if _, err = b.PublishRouting(ctx, "admin", policy, saved.Revision); !errors.Is(err, state.ErrInvalid) {
		t.Fatal("unknown target accepted", err)
	}
	policy.MachineLabel = "linux-runner-01"
	f.org[0].Available = false
	if _, err = b.PublishRouting(ctx, "admin", policy, saved.Revision); !errors.Is(err, state.ErrInvalid) {
		t.Fatal("unshared org target accepted", err)
	}
	f.org[0].Available = true
	f.orgErr = HTTPError(403)
	if _, err = b.PublishRouting(ctx, "admin", policy, saved.Revision); err == nil {
		t.Fatal("unknown organization visibility accepted")
	}
	policy.Placement = "cloud-only"
	if _, err = b.PublishRouting(ctx, "admin", policy, saved.Revision); err != nil {
		t.Fatal("cloud-only depends on runner permissions", err)
	}
}
func TestLegacyRoutingAndRegistrationStayScopedAndSecretsAreNeverAudited(t *testing.T) {
	b, f, _ := managementFixture(t)
	ctx := context.Background()
	f.legacy = map[string]string{"SLUICEWORK_LOCAL_PLATFORM": "linux-x64", "SLUICEWORK_MACHINE_LABEL": "linux-runner-01", "SLUICEWORK_LOCAL_QUEUE_MINUTES": "4"}
	r, err := b.Routing(ctx)
	if err != nil || r.Policy.QueueMinutes != 4 || r.Policy.MachineLabel != "linux-runner-01" {
		t.Fatal(r, err)
	}
	setup, err := b.ExternalRegistration(ctx, "admin", "organization", "linux", "x64", "linux-runner-01", "linux-runner-01")
	if err != nil || setup.URL != "https://github.com/team" || setup.Token == "" {
		t.Fatal(setup.URL, err)
	}
	var audits string
	rows, err := b.db.Query("SELECT target FROM audit")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var x string
		if err = rows.Scan(&x); err != nil {
			t.Fatal(err)
		}
		audits += x
	}
	rows.Close()
	if strings.Contains(audits, setup.Token) {
		t.Fatal("token persisted to audit")
	}
	b.organization = false
	if _, err = b.ExternalRegistration(ctx, "admin", "organization", "linux", "x64", "test", "test"); !errors.Is(err, state.ErrInvalid) {
		t.Fatal("organization opt-in bypassed")
	}
	f.raw = `{"version":1,"placement":"prefer-local","platform":"linux-x64","machineLabel":"linux-runner-01","queueMinutes":2,"extra":1}`
	f.exists = true
	if _, err = b.Routing(ctx); err == nil {
		t.Fatal("unknown routing fields accepted")
	}
	if !slices.Contains(f.integration.Platforms, "linux-x64") {
		t.Fatal("fixture error")
	}
	encoded, _ := json.Marshal(setup)
	if !strings.Contains(string(encoded), `"token"`) {
		t.Fatal("registration wire format")
	}
}

func TestAddingCustomLabelsPreservesPublishedRoutingAlias(t *testing.T) {
	b, f, _ := managementFixture(t)
	ctx := context.Background()
	current, err := b.Routing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p := RoutingPolicy{Version: 1, Placement: "prefer-local", Platform: "linux-x64", MachineLabel: "linux-runner-01", QueueMinutes: 2}
	saved, err := b.PublishRouting(ctx, "admin", p, current.Revision)
	if err != nil {
		t.Fatal(err)
	}
	f.org[0].Labels = append(f.org[0].Labels, RunnerLabel{"fast", "custom"})
	inventory, err := b.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	aliases := []string{}
	for _, target := range inventory.Targets {
		if !target.Managed {
			aliases = append(aliases, target.MachineLabel)
		}
	}
	if !slices.Contains(aliases, "linux-runner-01") || !slices.Contains(aliases, "fast") {
		t.Fatal("existing unique alias disappeared", aliases)
	}
	p.QueueMinutes = 3
	if _, err = b.PublishRouting(ctx, "admin", p, saved.Revision); err != nil {
		t.Fatal("existing alias cannot be republished", err)
	}
}
func TestPublishedPolicyRequiresAllExactVersionOneFields(t *testing.T) {
	b, f, _ := managementFixture(t)
	f.exists = true
	for _, raw := range []string{
		`{"version":1}`,
		`{"version":1,"placement":"cloud-only","platform":"linux-x64","queueMinutes":2}`,
		`{"version":1,"placement":"cloud-only","platform":"linux-x64","machineLabel":null,"queueMinutes":2}`,
		`{"Version":1,"placement":"prefer-local","platform":"linux-x64","machineLabel":"linux-runner-01","queueMinutes":2}`,
	} {
		f.raw = raw
		if _, err := b.Routing(context.Background()); err == nil {
			t.Fatal("partial or inconsistent policy accepted", raw)
		}
	}
}
func TestRoutingRevisionIsBoundToConfiguredRepository(t *testing.T) {
	b, _, _ := managementFixture(t)
	ctx := context.Background()
	before, err := b.Routing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b.repository = "another/pilot"
	after, err := b.Routing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.Revision == after.Revision || after.Repository != "another/pilot" {
		t.Fatal("routing context not bound to repository")
	}
	if _, err = b.PublishRouting(ctx, "admin", before.Policy, before.Revision); !errors.Is(err, state.ErrConflict) {
		t.Fatal("stale page changed a different repository", err)
	}
}

func TestOrderedPlacementValidatesAndPublishesBothExplicitTargets(t *testing.T) {
	b, f, m := managementFixture(t)
	ctx := context.Background()
	f.integration.Version = 2
	p := RoutingPolicy{Version: 2, Placement: "prefer-local", Platform: "linux-x64", MachineLabel: "linux-runner-01", QueueMinutes: 2, FallbackRunner: &RunnerSelector{"macos-arm64", "sluicework-" + m.ID[:12]}}
	current, err := b.Routing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := b.PublishRouting(ctx, "admin", p, current.Revision)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeRouting([]byte(f.raw))
	if err != nil || decoded.FallbackRunner == nil || *decoded.FallbackRunner != *p.FallbackRunner || saved.Policy.Version != 2 {
		t.Fatal("lost runner order", f.raw, err)
	}
	p.FallbackRunner = &RunnerSelector{"macos-arm64", "missing"}
	if _, err = b.PublishRouting(ctx, "admin", p, saved.Revision); !errors.Is(err, state.ErrInvalid) || f.writes != 1 {
		t.Fatal("unavailable fallback accepted", err)
	}
	p.FallbackRunner = &RunnerSelector{"macos-arm64", "sluicework-" + m.ID[:12]}
	f.integration.Platforms = []string{"linux-x64"}
	current, _ = b.Routing(ctx)
	if _, err = b.PublishRouting(ctx, "admin", p, current.Revision); !errors.Is(err, state.ErrInvalid) {
		t.Fatal("unsupported fallback platform accepted", err)
	}
	f.integration.Platforms = []string{"linux-x64", "macos-arm64"}
	f.integration.Version = 1
	current, _ = b.Routing(ctx)
	if _, err = b.PublishRouting(ctx, "admin", p, current.Revision); !errors.Is(err, state.ErrInvalid) {
		t.Fatal("ordered policy published to legacy workflow", err)
	}
}

func TestSavingHighPriorityPublishesFifteenMinutes(t *testing.T) {
	b, f, m := managementFixture(t)
	f.integration.Version = 3
	p := RoutingPolicy{Version: 3, Priority: "high", Placement: "prefer-local", Platform: "linux-x64", MachineLabel: "linux-runner-01", QueueMinutes: 5, FallbackRunner: &RunnerSelector{"macos-arm64", "sluicework-" + m.ID[:12]}, FallbackQueueMinutes: 2}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	f.raw, f.exists = string(raw), true
	current, err := b.Routing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	saved, err := b.PublishRouting(context.Background(), "admin", p, current.Revision)
	if err != nil || saved.Policy.QueueMinutes != 15 {
		t.Fatal("High did not publish fifteen minutes", saved, err)
	}
	parsed, err := ParseRouting([]byte(f.raw))
	if err != nil || parsed.QueueMinutes != 15 || parsed.FallbackQueueMinutes != 2 {
		t.Fatal("unexpected saved windows", f.raw, err)
	}
}
func TestOrderedRoutingRejectsMalformedDuplicateAndPartialFallbacks(t *testing.T) {
	base := `{"version":2,"placement":"prefer-local","platform":"linux-x64","machineLabel":"linux-runner-01","queueMinutes":2,"fallbackRunner":%s}`
	for _, fallback := range []string{`null`, `{}`, `{"platform":"macos-arm64"}`, `{"platform":"macos-arm64","machineLabel":null}`, `{"platform":"macos-arm64","machineLabel":"LINUX-RUNNER-01"}`, `{"platform":"macos-arm64","machineLabel":"macOS"}`, `{"Platform":"macos-arm64","machineLabel":"mac"}`, `{"platform":"macos-arm64","machineLabel":"mac","extra":1}`} {
		raw := fmt.Sprintf(base, fallback)
		if _, err := decodeRouting([]byte(raw)); err == nil {
			t.Fatal("invalid fallback accepted", raw)
		}
	}
	valid := fmt.Sprintf(base, `{"platform":"macos-arm64","machineLabel":"mac"}`)
	if _, err := decodeRouting([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{strings.Replace(valid, `"version":2`, `"version":1`, 1), strings.Replace(valid, `"prefer-local"`, `"cloud-only"`, 1)} {
		if _, err := decodeRouting([]byte(raw)); err == nil {
			t.Fatal("invalid ordered version or mode", raw)
		}
	}
}

func TestVersionThreeLinuxPoolPublicationRespectsRepositoryAccess(t *testing.T) {
	b, f, m := managementFixture(t)
	f.integration.Version = 3
	f.integration.Platforms = append(f.integration.Platforms, "linux")
	p := RoutingPolicy{Version: 3, Priority: "high", QueueMinutes: 15, Placement: "prefer-local", Platform: "linux", MachineLabel: "self-hosted", FallbackQueueMinutes: 2, FallbackRunner: &RunnerSelector{Platform: "macos-arm64", MachineLabel: "sluicework-" + m.ID[:12]}}
	// A second Linux runner needs neither a custom label nor an architecture label.
	other := f.org[0]
	other.ID++
	other.Name = "linux-runner-02"
	other.Labels = []RunnerLabel{{"self-hosted", "read-only"}, {"Linux", "read-only"}}
	f.org = append(f.org, other)
	current, err := b.Routing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.PublishRouting(context.Background(), "admin", p, current.Revision); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*RoutingPolicy){func(p *RoutingPolicy) { p.Version = 2 }, func(p *RoutingPolicy) { p.MachineLabel = "linux-runner-01" }, func(p *RoutingPolicy) { p.Platform = "macos-arm64" }, func(p *RoutingPolicy) {
		p.FallbackRunner = &RunnerSelector{Platform: "linux", MachineLabel: "self-hosted"}
	}} {
		bad := p
		mutate(&bad)
		if bad.Validate() == nil {
			t.Fatal("unsupported generic selector accepted")
		}
	}
	for i := range f.org {
		f.org[i].Available = false
	}
	current, err = b.Routing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.PublishRouting(context.Background(), "admin", p, current.Revision); err == nil {
		t.Fatal("pool without repository access published")
	}
}
