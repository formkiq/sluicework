package githubapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"sluicework/internal/state"
)

type managementSource interface {
	ListRunners(context.Context) ([]Runner, error)
	OrganizationInventory(context.Context) ([]OrganizationRunner, error)
	ScopedRunner(context.Context, string, int64) (Runner, error)
	SetRunnerLabels(context.Context, string, int64, []string) error
	RemoveRunner(context.Context, string, int64) error
	Registration(context.Context, string) (RegistrationToken, error)
	Applications(context.Context, string) ([]RunnerApplication, error)
	Integration(context.Context) (Integration, error)
	Variable(context.Context, string) (string, bool, error)
	LegacyVariables(context.Context) (map[string]string, error)
	SaveRouting(context.Context, string, bool) error
}
type RunnerSelector struct {
	Platform     string `json:"platform"`
	MachineLabel string `json:"machineLabel"`
}
type DispatcherBinding struct {
	AppID        int64  `json:"appId"`
	BackendID    string `json:"backendId"`
	HeartbeatSHA string `json:"heartbeatSha"`
}
type RoutingPolicy struct {
	Priority             string          `json:"priority,omitempty"`
	FallbackQueueMinutes int             `json:"fallbackQueueMinutes,omitempty"`
	Version              int             `json:"version"`
	Placement            string          `json:"placement"`
	Platform             string          `json:"platform"`
	MachineLabel         string          `json:"machineLabel"`
	QueueMinutes         int             `json:"queueMinutes"`
	FallbackRunner       *RunnerSelector `json:"fallbackRunner,omitempty"`
}
type Integration struct {
	Dispatcher *DispatcherBinding `json:"dispatcher,omitempty"`
	Version    int                `json:"version"`
	Variable   string             `json:"variable"`
	Workflow   string             `json:"workflow"`
	Platforms  []string           `json:"platforms"`
	Defaults   RoutingPolicy      `json:"defaults"`
}
type RoutingState struct {
	Repository  string        `json:"repository"`
	Integration Integration   `json:"integration"`
	Policy      RoutingPolicy `json:"policy"`
	Source      string        `json:"source"`
	Revision    string        `json:"revision"`
}
type RunnerView struct {
	Runner
	Scope     string       `json:"scope"`
	Managed   bool         `json:"managed"`
	MachineID string       `json:"machineId,omitempty"`
	Available bool         `json:"available"`
	Group     *RunnerGroup `json:"group,omitempty"`
}
type RoutingTarget struct {
	Name         string `json:"name"`
	Platform     string `json:"platform"`
	MachineLabel string `json:"machineLabel"`
	Managed      bool   `json:"managed"`
	Online       bool   `json:"online"`
	Busy         bool   `json:"busy"`
}
type RunnerInventory struct {
	Configured          bool            `json:"configured"`
	Repository          string          `json:"repository"`
	OrganizationEnabled bool            `json:"organizationEnabled"`
	OrganizationIssue   string          `json:"organizationIssue,omitempty"`
	Runners             []RunnerView    `json:"runners"`
	Targets             []RoutingTarget `json:"targets"`
}

var labelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)

func validPlatform(p string) bool {
	return slices.Contains([]string{"macos-arm64", "macos-x64", "linux-arm64", "linux-x64", "linux"}, p)
}
func (p RoutingPolicy) Selectors() []RunnerSelector {
	out := []RunnerSelector{{p.Platform, p.MachineLabel}}
	if p.FallbackRunner != nil {
		out = append(out, *p.FallbackRunner)
	}
	return out
}
func (p RoutingPolicy) Validate() error {
	pool := p.Version == 3 && p.Platform == "linux" && p.MachineLabel == "self-hosted"
	if p.Platform == "linux" && !pool {
		return fmt.Errorf("%w: generic Linux selection requires a version 3 self-hosted pool", state.ErrInvalid)
	}
	if (p.Version != 1 && p.Version != 2 && p.Version != 3) || !slices.Contains([]string{"prefer-local", "local-only", "cloud-only"}, p.Placement) || !validPlatform(p.Platform) || p.QueueMinutes < 1 || (p.Version < 3 && p.QueueMinutes > 15) || len(p.MachineLabel) > 100 || (p.Placement != "cloud-only" && !pool && (!labelPattern.MatchString(p.MachineLabel) || platformLabel(p.MachineLabel))) {
		return fmt.Errorf("%w: select a placement policy, explicit runner label, supported platform and 1–15 minute timeout", state.ErrInvalid)
	}
	if p.Version == 3 {
		if (p.Priority != "high" && p.Priority != "low") || (p.Priority == "high" && p.QueueMinutes != 15 && p.QueueMinutes != 5) || (p.Priority == "low" && p.QueueMinutes != 60) || p.FallbackRunner == nil || p.FallbackQueueMinutes < 1 || p.FallbackQueueMinutes > 5 {
			return fmt.Errorf("%w: select high (15 minutes) or low (60 minutes) priority and a 1–5 minute fallback window", state.ErrInvalid)
		}
	} else if p.Priority != "" || p.FallbackQueueMinutes != 0 {
		return fmt.Errorf("%w: priority requires a version 3 dispatcher", state.ErrInvalid)
	}
	if (p.Version == 1 && p.FallbackRunner != nil) || (p.Version == 2 && (p.FallbackRunner == nil || p.Placement == "cloud-only")) {
		return fmt.Errorf("%w: ordered runner placement requires version 2 and a fallback runner", state.ErrInvalid)
	}
	if f := p.FallbackRunner; f != nil && (!validPlatform(f.Platform) || f.Platform == "linux" || !labelPattern.MatchString(f.MachineLabel) || platformLabel(f.MachineLabel) || strings.EqualFold(f.MachineLabel, p.MachineLabel)) {
		return fmt.Errorf("%w: select a distinct fallback runner with an explicit label and supported platform", state.ErrInvalid)
	}
	return nil
}
func strictJSON(raw []byte, out any) error {
	if len(raw) > 16384 {
		return errors.New("configuration is too large")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return errors.New("unexpected trailing configuration")
	}
	return nil
}
func decodeRouting(raw []byte) (RoutingPolicy, error) {
	var p RoutingPolicy
	if err := strictJSON(raw, &p); err != nil {
		return p, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return p, err
	}
	expected := 5
	if p.Version == 2 {
		expected = 6
	}
	if p.Version == 3 {
		expected = 8
	}
	if len(fields) != expected {
		return p, state.ErrInvalid
	}
	for _, key := range []string{"version", "placement", "platform", "machineLabel", "queueMinutes"} {
		v, ok := fields[key]
		if !ok || string(v) == "null" {
			return p, state.ErrInvalid
		}
	}
	if p.Version >= 2 {
		var fallback map[string]json.RawMessage
		if err := json.Unmarshal(fields["fallbackRunner"], &fallback); err != nil || len(fallback) != 2 {
			return p, state.ErrInvalid
		}
		for _, key := range []string{"platform", "machineLabel"} {
			if v, ok := fallback[key]; !ok || string(v) == "null" {
				return p, state.ErrInvalid
			}
		}
	}
	return p, p.Validate()
}
func routingRevision(repository string, integration Integration, raw string) string {
	data, _ := json.Marshal(integration)
	sum := sha256.Sum256([]byte(strings.ToLower(repository) + "\x00" + string(data) + "\x00" + raw))
	return hex.EncodeToString(sum[:])
}
func runnerPlatform(r Runner) string {
	os, arch := "", ""
	for _, l := range r.Labels {
		switch strings.ToLower(l.Name) {
		case "macos":
			os = "macos"
		case "linux":
			os = "linux"
		case "x64":
			arch = "x64"
		case "arm64":
			arch = "arm64"
		}
	}
	if os != "" && arch != "" {
		return os + "-" + arch
	}
	return ""
}
func managedPlatform(m state.Machine) string {
	os := map[string]string{"darwin": "macos", "linux": "linux"}[m.OS]
	arch := map[string]string{"arm64": "arm64", "amd64": "x64"}[m.Arch]
	if os != "" && arch != "" {
		return os + "-" + arch
	}
	return ""
}
func (b *Broker) Inventory(ctx context.Context) (RunnerInventory, error) {
	out := RunnerInventory{Configured: true, Repository: b.repository, OrganizationEnabled: b.organization, Runners: []RunnerView{}, Targets: []RoutingTarget{}}
	if b.management == nil {
		return out, errors.New("runner management is unavailable")
	}
	runners, err := b.management.ListRunners(ctx)
	if err != nil {
		return out, err
	}
	indexed := map[int64]int{}
	for _, r := range runners {
		indexed[r.ID] = len(out.Runners)
		out.Runners = append(out.Runners, RunnerView{Runner: r, Scope: "repository", Available: true})
	}
	if b.organization {
		org, err := b.management.OrganizationInventory(ctx)
		if err != nil {
			out.OrganizationIssue = "Organization runners unavailable. Grant organization Self-hosted runners: read/write and approve the App installation."
		} else {
			for _, r := range org {
				v := RunnerView{Runner: r.Runner, Scope: "organization", Available: r.Available, Group: &r.Group}
				if index, ok := indexed[r.ID]; ok {
					out.Runners[index] = v
				} else {
					indexed[r.ID] = len(out.Runners)
					out.Runners = append(out.Runners, v)
				}
			}
		}
	}
	machines, err := (&state.Store{DB: b.db}).Machines()
	if err != nil {
		return out, err
	}
	byName := map[string]string{}
	for _, m := range machines {
		if m.Runner != nil && m.Runner.Workstation && strings.EqualFold(m.Runner.Scope, b.machineScope()) {
			byName["sluicework-"+m.ID] = m.ID
		}
	}
	for i := range out.Runners {
		v := &out.Runners[i]
		v.MachineID = byName[v.Name]
		v.Managed = v.MachineID != "" || strings.HasPrefix(v.Name, "sluicework-")
		var machine string
		err = b.db.QueryRow("SELECT machine FROM github_runner_leases WHERE repository=? AND runner=?", b.repository, v.ID).Scan(&machine)
		if err == nil {
			v.Managed = true
			v.MachineID = machine
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return out, err
		}
	}
	// The explicit Linux pool has no custom-label or architecture constraint.
	// Keep repository/group permissions authoritative, including restricted groups.
	pool := RoutingTarget{Name: "Any self-hosted Linux runner (Default group)", Platform: "linux", MachineLabel: "self-hosted"}
	hasPool, idlePool := false, false
	for _, v := range out.Runners {
		if v.Managed || !v.Available || v.Group == nil || !strings.EqualFold(v.Group.Name, "Default") || v.Group.Restricted {
			continue
		}
		labels := map[string]bool{}
		for _, label := range v.Labels {
			labels[strings.ToLower(label.Name)] = true
		}
		if !labels["self-hosted"] || !labels["linux"] {
			continue
		}
		hasPool = true
		if v.Status == "online" {
			pool.Online = true
			idlePool = idlePool || !v.Busy
		}
	}
	if hasPool {
		pool.Busy = pool.Online && !idlePool
		out.Targets = append(out.Targets, pool)
	}
	// Resolve explicit labels across every runner on the same platform. A broad
	// or duplicated label never becomes a deterministic placement target.
	for _, v := range out.Runners {
		if v.Managed || !v.Available || (v.Group != nil && v.Group.Restricted) {
			continue
		}
		platform := runnerPlatform(v.Runner)
		if !validPlatform(platform) {
			continue
		}
		labels := []string{}
		for _, l := range v.Labels {
			if l.Type == "custom" && labelPattern.MatchString(l.Name) && !platformLabel(l.Name) {
				labels = append(labels, l.Name)
			}
		}
		sort.Strings(labels)
		for _, label := range labels {
			count := 0
			for _, other := range out.Runners {
				if runnerPlatform(other.Runner) != platform {
					continue
				}
				for _, l := range other.Labels {
					if strings.EqualFold(l.Name, label) {
						count++
						break
					}
				}
			}
			if count == 1 {
				out.Targets = append(out.Targets, RoutingTarget{v.Name, platform, label, false, v.Status == "online", v.Busy})
			}
		}
	}
	// An opt-in Sluicework machine remains selectable while no JIT listener is
	// registered. Its stable machine label is owned by the approved controller.
	for _, m := range machines {
		if m.Revoked || m.Runner == nil || !m.Runner.Workstation || !strings.EqualFold(m.Runner.Scope, b.machineScope()) || len(m.ID) < 12 {
			continue
		}
		p := managedPlatform(m)
		label := "sluicework-" + m.ID[:12]
		collision := false
		for _, r := range out.Runners {
			if runnerPlatform(r.Runner) != p || r.MachineID == m.ID {
				continue
			}
			for _, l := range r.Labels {
				if strings.EqualFold(l.Name, label) {
					collision = true
				}
			}
		}
		if validPlatform(p) && !collision {
			out.Targets = append(out.Targets, RoutingTarget{m.Name, p, label, true, m.Online, m.RunnerStatus == "building"})
		}
	}
	return out, nil
}
func (b *Broker) activeManagement() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.retired {
		return state.ErrConflict
	}
	if b.management == nil {
		return errors.New("runner management is unavailable")
	}
	return nil
}
func (b *Broker) external(ctx context.Context, scope string, id int64) (RunnerView, error) {
	if id <= 0 || !slices.Contains([]string{"repository", "organization"}, scope) {
		return RunnerView{}, state.ErrInvalid
	}
	inventory, err := b.Inventory(ctx)
	if err != nil {
		return RunnerView{}, err
	}
	if scope == "organization" && inventory.OrganizationIssue != "" {
		return RunnerView{}, errors.New(inventory.OrganizationIssue)
	}
	for _, r := range inventory.Runners {
		if r.ID == id && r.Scope == scope {
			if r.Managed || (r.Group != nil && r.Group.Inherited) {
				return r, fmt.Errorf("%w: use the owning controller or organization to manage this runner", state.ErrInvalid)
			}
			return r, nil
		}
	}
	return RunnerView{}, state.ErrNotFound
}
func (b *Broker) UpdateExternalLabels(ctx context.Context, actor, scope string, id int64, labels []string) error {
	b.adminMu.Lock()
	defer b.adminMu.Unlock()
	if err := b.activeManagement(); err != nil {
		return err
	}
	if len(labels) > 32 {
		return state.ErrInvalid
	}
	seen := map[string]bool{}
	for _, label := range labels {
		key := strings.ToLower(label)
		if !labelPattern.MatchString(label) || platformLabel(label) || strings.HasPrefix(key, "sluicework-") || seen[key] {
			return fmt.Errorf("%w: use unique custom labels of 1–100 letters, digits, dots, underscores or hyphens; Sluicework labels are reserved", state.ErrInvalid)
		}
		seen[key] = true
	}
	if _, err := b.external(ctx, scope, id); err != nil {
		return err
	}
	if err := (&state.Store{DB: b.db}).Audit(actor, "request GitHub runner label update", fmt.Sprintf("%s %s/%d", b.repository, scope, id)); err != nil {
		return err
	}
	if labels == nil {
		labels = []string{}
	}
	return b.management.SetRunnerLabels(ctx, scope, id, labels)
}
func (b *Broker) RemoveExternal(ctx context.Context, actor, scope string, id int64, name string) error {
	b.adminMu.Lock()
	defer b.adminMu.Unlock()
	if err := b.activeManagement(); err != nil {
		return err
	}
	v, err := b.external(ctx, scope, id)
	if err != nil {
		return err
	}
	if name == "" || name != v.Name {
		return fmt.Errorf("%w: type the exact runner name", state.ErrInvalid)
	}
	r, err := b.management.ScopedRunner(ctx, scope, id)
	if err != nil {
		return err
	}
	if r.Busy {
		return fmt.Errorf("%w: wait for the assigned job to finish before removing this runner", state.ErrConflict)
	}
	if r.Status != "offline" {
		return fmt.Errorf("%w: stop the external runner service on its host and wait for Offline before removing its registration", state.ErrConflict)
	}
	if r.Name != name {
		return state.ErrConflict
	}
	if err = (&state.Store{DB: b.db}).Audit(actor, "request GitHub runner removal", fmt.Sprintf("%s %s/%d", b.repository, scope, id)); err != nil {
		return err
	}
	return b.management.RemoveRunner(ctx, scope, id)
}

type RegistrationSetup struct {
	Scope       string            `json:"scope"`
	URL         string            `json:"url"`
	Name        string            `json:"name"`
	Label       string            `json:"label"`
	Application RunnerApplication `json:"application"`
	RegistrationToken
}

func (b *Broker) ExternalRegistration(ctx context.Context, actor, scope, os, arch, name, label string) (RegistrationSetup, error) {
	b.adminMu.Lock()
	defer b.adminMu.Unlock()
	var out RegistrationSetup
	if err := b.activeManagement(); err != nil {
		return out, err
	}
	if (scope != "repository" && scope != "organization") || (scope == "organization" && !b.organization) || (os != "linux" && os != "osx") || (arch != "x64" && arch != "arm64") || !labelPattern.MatchString(name) || !labelPattern.MatchString(label) || platformLabel(label) || strings.HasPrefix(strings.ToLower(name), "sluicework-") || strings.HasPrefix(strings.ToLower(label), "sluicework-") {
		return out, fmt.Errorf("%w: choose repository/organization, Mac/Linux, ARM64/x64 and a custom runner name/label", state.ErrInvalid)
	}
	apps, err := b.management.Applications(ctx, scope)
	if err != nil {
		return out, err
	}
	found := false
	for _, app := range apps {
		if app.OS == os && app.Architecture == arch {
			if !strings.HasPrefix(app.DownloadURL, "https://github.com/actions/runner/releases/download/") || !regexp.MustCompile(`^[a-zA-Z0-9_.-]+\.tar\.gz$`).MatchString(app.Filename) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(app.SHA256) {
				return out, errors.New("verified runner download is unavailable; use GitHub runner setup")
			}
			out.Application = app
			found = true
			break
		}
	}
	if !found {
		return out, fmt.Errorf("%w: runner download unavailable for this platform", state.ErrInvalid)
	}
	if err = (&state.Store{DB: b.db}).Audit(actor, "issue external runner registration", b.repository+" "+scope); err != nil {
		return out, err
	}
	token, err := b.management.Registration(ctx, scope)
	if err != nil {
		return out, err
	}
	target := b.repository
	if scope == "organization" {
		target = strings.Split(target, "/")[0]
	}
	out.Scope, out.URL, out.Name, out.Label, out.RegistrationToken = scope, "https://github.com/"+target, name, label, token
	return out, nil
}
func (b *Broker) Routing(ctx context.Context) (RoutingState, error) {
	out := RoutingState{Repository: b.repository}
	if b.management == nil {
		return out, errors.New("workflow management unavailable")
	}
	integration, err := b.management.Integration(ctx)
	if err != nil {
		return out, err
	}
	out.Integration = integration
	raw, exists, err := b.management.Variable(ctx, RoutingVariable)
	if err != nil {
		return out, err
	}
	revision := raw
	out.Policy = integration.Defaults
	out.Source = "workflow defaults"
	if exists {
		if out.Policy, err = decodeRouting([]byte(raw)); err != nil {
			return out, fmt.Errorf("%w: invalid published routing configuration", state.ErrInvalid)
		}
		out.Source = "repository variable"
	} else {
		legacy, e := b.management.LegacyVariables(ctx)
		if e != nil {
			return out, e
		}
		if legacy["SLUICEWORK_MACHINE_LABEL"] != "" || legacy["SLUICEWORK_LOCAL_PLATFORM"] != "" {
			out.Policy.Version, out.Policy.FallbackRunner = 1, nil
		}
		if v := legacy["SLUICEWORK_MACHINE_LABEL"]; v != "" {
			out.Policy.MachineLabel = v
			out.Source = "legacy repository variables"
		}
		if v := legacy["SLUICEWORK_LOCAL_PLATFORM"]; v != "" {
			out.Policy.Platform = v
			out.Source = "legacy repository variables"
		}
		if v := legacy["SLUICEWORK_LOCAL_QUEUE_MINUTES"]; v != "" {
			out.Policy.QueueMinutes, _ = strconv.Atoi(v)
			out.Source = "legacy repository variables"
		}
		data, _ := json.Marshal(legacy)
		revision = string(data)
	}
	if err = out.Policy.Validate(); err != nil {
		return out, err
	}
	out.Revision = routingRevision(b.repository, integration, revision)
	return out, nil
}
func (b *Broker) PublishRouting(ctx context.Context, actor string, policy RoutingPolicy, revision string) (RoutingState, error) {
	b.adminMu.Lock()
	defer b.adminMu.Unlock()
	var out RoutingState
	if err := b.activeManagement(); err != nil {
		return out, err
	}
	if err := policy.Validate(); err != nil {
		return out, err
	}
	// Read old five-minute policies during rolling upgrades; new saves use fifteen.
	if policy.Version == 3 && policy.Priority == "high" {
		policy.QueueMinutes = 15
	}
	current, err := b.Routing(ctx)
	if err != nil {
		return out, err
	}
	if revision == "" || revision != current.Revision {
		return out, fmt.Errorf("%w: routing changed; reload it before saving", state.ErrConflict)
	}
	if policy.Version > current.Integration.Version {
		return out, fmt.Errorf("%w: upgrade the repository workflow before publishing ordered runners", state.ErrInvalid)
	}
	for _, selector := range policy.Selectors() {
		if !slices.Contains(current.Integration.Platforms, selector.Platform) {
			return out, fmt.Errorf("%w: this workflow does not support that build host", state.ErrInvalid)
		}
	}
	if policy.Placement != "cloud-only" {
		inventory, e := b.Inventory(ctx)
		if e != nil {
			return out, e
		}
		for _, selector := range policy.Selectors() {
			matches := 0
			for _, target := range inventory.Targets {
				if target.Platform == selector.Platform && strings.EqualFold(target.MachineLabel, selector.MachineLabel) {
					if !target.Managed && inventory.OrganizationIssue != "" {
						return out, errors.New(inventory.OrganizationIssue)
					}
					matches++
				}
			}
			if matches != 1 {
				return out, fmt.Errorf("%w: select the available Linux pool, a runner with a unique custom label, or an approved Sluicework machine", state.ErrInvalid)
			}
		}
	}
	raw, _ := json.Marshal(policy)
	if err = (&state.Store{DB: b.db}).Audit(actor, "publish GitHub Actions placement", b.repository); err != nil {
		return out, err
	}
	_, exists, err := b.management.Variable(ctx, RoutingVariable)
	if err != nil {
		return out, err
	}
	if err = b.management.SaveRouting(ctx, string(raw), exists); err != nil {
		return out, err
	}
	current.Policy = policy
	current.Source = "repository variable"
	current.Revision = routingRevision(b.repository, current.Integration, string(raw))
	return current, nil
}

// ParseRouting validates a published repository policy using the admin contract.
func ParseRouting(raw []byte) (RoutingPolicy, error) { return decodeRouting(raw) }
