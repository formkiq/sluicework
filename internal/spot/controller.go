package spot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Instance struct {
	ID, State, Reason string
	Owned             bool
}
type Runner struct {
	ID           int64
	Online, Busy bool
	Labels       []string
}
type ManagedInstance struct {
	Instance
	Token string
}
type Registration struct {
	ID      int64
	JIT     string
	Package Package
}
type Cloud interface {
	Launch(context.Context, LaunchSpec) (string, error)
	Inspect(context.Context, string) (Instance, error)
	Terminate(context.Context, string) error
	Inventory(context.Context) ([]ManagedInstance, error)
}
type Registrations interface {
	Create(context.Context, string, string) (Registration, error)
	Find(context.Context, string) (Runner, bool, error)
	Get(context.Context, int64) (Runner, bool, error)
	Remove(context.Context, int64) error
}

// A rejected launch is a definitive response proving no instance was created.
// Only the first response may clear its reservation; an earlier uncertain call
// always requires idempotent reconciliation with the original launch spec.
var ErrLaunchRejected = errors.New("Spot capacity request was rejected")
var ErrRegistrationRejected = errors.New("Spot runner registration was rejected before creation")
var ErrUnavailable = errors.New("Spot runner could not become available")

type Controller struct {
	BackendID        string
	Infrastructure   Infrastructure
	Approved         []string
	Store            Store
	Cloud            Cloud
	Runners          Registrations
	Settings         Settings
	Now              func() time.Time
	mu               sync.Mutex
	inventoryBlocked bool
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}
func (c *Controller) Validate() error {
	if !namePattern.MatchString(c.BackendID) || c.Store == nil || c.Cloud == nil || c.Runners == nil {
		return errors.New("incomplete Spot controller")
	}
	if err := c.Infrastructure.Validate(c.Approved); err != nil {
		return err
	}
	return c.Settings.Validate(c.BackendID, c.Approved)
}
func (c *Controller) SetSettings(s Settings) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := s.Validate(c.BackendID, c.Approved); e != nil {
		return e
	}
	c.Settings = s
	return nil
}
func validJob(j Job) bool {
	return namePattern.MatchString(j.Kind) && namePattern.MatchString(j.ID) && repoPattern.MatchString(j.Repository) && regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(j.WorkflowSHA) && regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(j.SourceSHA) && regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,180}$`).MatchString(j.RequestID) && strings.HasPrefix(j.WorkflowRef, j.Repository+"/.github/workflows/sluicework-build.yml@refs/heads/") && !strings.ContainsAny(j.WorkflowRef, "\r\n\x00") && !strings.Contains(j.WorkflowRef, "..") && j.Policy.Validate() == nil
}
func approvedPackage(p Package) bool {
	u, e := url.Parse(p.URL)
	return e == nil && u.Scheme == "https" && u.Host == "github.com" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && regexp.MustCompile(`^/actions/runner/releases/download/v[0-9]+\.[0-9]+\.[0-9]+/actions-runner-linux-arm64-[0-9]+\.[0-9]+\.[0-9]+\.tar\.gz$`).MatchString(u.Path) && regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(p.SHA256)
}
func (c *Controller) reserve(ctx context.Context, j Job) (Allocation, error) {
	if !validJob(j) {
		return Allocation{}, errors.New("invalid source-bound Spot request")
	}
	allow := false
	for _, r := range c.Approved {
		if r == j.Repository {
			allow = true
		}
	}
	if !allow {
		return Allocation{}, errors.New("Spot repository is not approved")
	}
	for tries := 0; tries < 8; tries++ {
		state, e := c.Store.Load(ctx)
		if e != nil {
			return Allocation{}, e
		}
		if a, ok := state.Allocations[j.ID]; ok {
			if a.Spec.Job != j {
				return Allocation{}, errors.New("Spot reservation belongs to another source or policy")
			}
			return a, nil
		}
		if c.inventoryBlocked {
			return Allocation{}, ErrUncertain
		}
		if !c.Settings.Enabled || c.Settings.MaxConcurrent == 0 {
			return Allocation{}, ErrDisabled
		}
		if len(state.Allocations) >= c.Settings.MaxConcurrent || len(state.Allocations) >= HardLimit {
			return Allocation{}, ErrCapacity
		}
		profile, e := LookupProfile(j.Policy.Profile)
		if e != nil {
			return Allocation{}, e
		}
		digest := sha256.Sum256([]byte(c.BackendID + ":" + j.ID))
		token := hex.EncodeToString(digest[:])
		now := c.now()
		spec := LaunchSpec{ClientToken: token, Name: "sluicework-spot-" + token[:24], Label: "sluicework-spot-" + token[:24], BackendID: c.BackendID, Job: j, Profile: profile, ImageID: c.Infrastructure.ImageID, RootDeviceName: c.Infrastructure.RootDeviceName, SubnetID: c.Infrastructure.SubnetIDs[int(digest[0])%len(c.Infrastructure.SubnetIDs)], SecurityGroupID: c.Infrastructure.SecurityGroupID, VolumeGiB: c.Infrastructure.VolumeGiB, Expires: now.Add(time.Duration(j.Policy.RuntimeMinutes) * time.Minute)}
		a := Allocation{Spec: spec, State: "reserved", Created: now}
		if state.Allocations == nil {
			state.Allocations = map[string]Allocation{}
		}
		state.Allocations[j.ID] = a
		if e = c.Store.Save(ctx, &state); errors.Is(e, ErrConflict) {
			continue
		} else if e != nil {
			return Allocation{}, e
		}
		return a, nil
	}
	return Allocation{}, ErrConflict
}
func (c *Controller) update(ctx context.Context, a Allocation) error {
	for n := 0; n < 8; n++ {
		s, e := c.Store.Load(ctx)
		if e != nil {
			return e
		}
		old, ok := s.Allocations[a.Spec.Job.ID]
		if !ok || old.Spec.ClientToken != a.Spec.ClientToken {
			return ErrConflict
		}
		s.Allocations[a.Spec.Job.ID] = a
		if e = c.Store.Save(ctx, &s); errors.Is(e, ErrConflict) {
			continue
		}
		return e
	}
	return ErrConflict
}
func (c *Controller) forget(ctx context.Context, a Allocation) error {
	for n := 0; n < 8; n++ {
		s, e := c.Store.Load(ctx)
		if e != nil {
			return e
		}
		old, ok := s.Allocations[a.Spec.Job.ID]
		if !ok {
			return nil
		}
		if old.Spec.ClientToken != a.Spec.ClientToken {
			return ErrConflict
		}
		delete(s.Allocations, a.Spec.Job.ID)
		if e = c.Store.Save(ctx, &s); errors.Is(e, ErrConflict) {
			continue
		}
		return e
	}
	return ErrConflict
}

// Ensure requires the queue's global lease. EC2 retries reuse the immutable
// persisted spec. GitHub worker dispatch remains the queue's separate operation.
func (c *Controller) Ensure(ctx context.Context, j Job) (PublicAllocation, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.Validate(); e != nil {
		return PublicAllocation{}, false, e
	}
	a, e := c.reserve(ctx, j)
	if e != nil {
		return PublicAllocation{}, false, e
	}
	if a.InstanceID == "" && !a.LaunchUncertain && !c.now().Before(a.Created.Add(time.Duration(j.Policy.StartupMinutes)*time.Minute)) {
		return a.Public(), false, ErrUnavailable
	}
	if !c.now().Before(a.Spec.Expires) {
		return a.Public(), false, ErrUnavailable
	}
	if (!c.Settings.Enabled || c.Settings.MaxConcurrent == 0) && (a.State == "reserved" || a.State == "registered") {
		a.State = "rejected"
		a.EndReason = "admission-paused"
		if err := c.update(ctx, a); err != nil {
			return a.Public(), false, err
		}
		return a.Public(), false, ErrDisabled
	}
	if (!c.Settings.Enabled || c.Settings.MaxConcurrent == 0) && a.LaunchUncertain {
		return a.Public(), false, ErrUncertain
	}
	if a.State == "registration-unknown" || a.State == "registering" {
		runner, found, e := c.Runners.Find(ctx, a.Spec.Name)
		if e != nil {
			return a.Public(), false, e
		}
		if !found {
			return a.Public(), false, ErrUncertain
		}
		if runner.Busy {
			return a.Public(), false, ErrUncertain
		}
		// No launch is possible before JIT has been durably saved.
		if e = c.Runners.Remove(ctx, runner.ID); e != nil {
			return a.Public(), false, e
		}
		a.State = "reserved"
		a.RunnerID = 0
		if e = c.update(ctx, a); e != nil {
			return a.Public(), false, e
		}
	}
	if a.State == "reserved" {
		a.State = "registering"
		if e = c.update(ctx, a); e != nil {
			return a.Public(), false, e
		}
		grant, err := c.Runners.Create(ctx, a.Spec.Name, a.Spec.Label)
		if err != nil {
			a.State = "registration-unknown"
			if errors.Is(err, ErrRegistrationRejected) {
				a.State = "rejected"
				a.EndReason = "runner-package-unavailable"
			}
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			_ = c.update(cleanup, a)
			cancel()
			if a.State == "rejected" {
				return a.Public(), false, ErrUnavailable
			}
			return a.Public(), false, errors.New("Spot runner registration could not be confirmed")
		}
		a.RunnerID = grant.ID
		if grant.ID <= 0 || len(grant.JIT) == 0 || len(grant.JIT) > 8192 || !approvedPackage(grant.Package) {
			if grant.ID > 0 {
				_ = c.Runners.Remove(ctx, grant.ID)
			}
			a.State = "rejected"
			a.EndReason = "invalid-runner-package"
			_ = c.update(ctx, a)
			return a.Public(), false, ErrUnavailable
		}
		a.RunnerID = grant.ID
		a.Spec.JIT = grant.JIT
		a.Spec.Package = grant.Package
		a.State = "registered"
		if e = c.update(ctx, a); e != nil {
			return a.Public(), false, e
		}
	}
	if c.inventoryBlocked && (a.State == "registered" || a.State == "launching") {
		return a.Public(), false, ErrUncertain
	}
	if a.State == "registered" || a.State == "launching" {
		first := a.State == "registered"
		a.State = "launching"
		a.LaunchUncertain = true
		if e = c.update(ctx, a); e != nil {
			return a.Public(), false, e
		}
		id, err := c.Cloud.Launch(ctx, a.Spec)
		if err != nil {
			if first && errors.Is(err, ErrLaunchRejected) {
				a.State = "rejected"
				a.LaunchUncertain = false
				a.EndReason = "capacity-rejected"
				_ = c.update(ctx, a)
				return a.Public(), false, ErrUnavailable
			}
			return a.Public(), false, ErrUncertain
		}
		if !regexp.MustCompile(`^i-[a-f0-9]{8,17}$`).MatchString(id) {
			return a.Public(), false, ErrUncertain
		}
		a.InstanceID = id
		a.LaunchUncertain = false
		a.State = "booting"
		if e = c.update(ctx, a); e != nil {
			return a.Public(), false, e
		}
	}
	if a.State == "rejected" || a.State == "terminating" || a.State == "terminated" {
		return a.Public(), false, ErrUnavailable
	}
	if a.InstanceID == "" {
		return a.Public(), false, ErrUncertain
	}
	instance, e := c.Cloud.Inspect(ctx, a.InstanceID)
	if e != nil {
		return a.Public(), false, e
	}
	if !instance.Owned || instance.ID != a.InstanceID {
		return a.Public(), false, errors.New("Spot instance ownership could not be authenticated")
	}
	if instance.State == "terminated" {
		a.State = "terminated"
		a.EndReason = instance.Reason
		_ = c.update(ctx, a)
		return a.Public(), false, ErrUnavailable
	}
	if c.now().After(a.Created.Add(time.Duration(j.Policy.StartupMinutes)*time.Minute)) && a.State != "ready" {
		return a.Public(), false, ErrUnavailable
	}
	if instance.State != "running" {
		return a.Public(), false, nil
	}
	runner, found, e := c.Runners.Get(ctx, a.RunnerID)
	if e != nil {
		return a.Public(), false, e
	}
	if !found || !runner.Online || runner.Busy {
		return a.Public(), false, nil
	}
	matches := map[string]bool{}
	for _, v := range runner.Labels {
		matches[strings.ToLower(v)] = true
	}
	if !matches[strings.ToLower(a.Spec.Label)] || !matches["linux"] || !matches["arm64"] {
		return a.Public(), false, errors.New("Spot runner does not match its reserved labels")
	}
	a.State = "ready"
	if e = c.update(ctx, a); e != nil {
		return a.Public(), false, e
	}
	return a.Public(), true, nil
}

// Retire only releases a slot after positive termination (or a definitively
// rejected launch), and after its one-job GitHub registration is gone.
func (c *Controller) Retire(ctx context.Context, id string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, e := c.Store.Load(ctx)
	if e != nil {
		return false, e
	}
	a, ok := s.Allocations[id]
	if !ok {
		return true, nil
	}
	if a.LaunchUncertain {
		return false, ErrUncertain
	}
	// A persisted terminal state is positive, authenticated EC2 evidence.
	// EC2 can age a terminated instance out of DescribeInstances; IDs never revive.
	if a.InstanceID != "" && a.State != "terminated" {
		v, e := c.Cloud.Inspect(ctx, a.InstanceID)
		if e != nil {
			return false, e
		}
		if !v.Owned || v.ID != a.InstanceID {
			return false, errors.New("refusing to terminate an unowned instance")
		}
		if v.State != "terminated" {
			a.State = "terminating"
			if e = c.update(ctx, a); e != nil {
				return false, e
			}
			if e = c.Cloud.Terminate(ctx, a.InstanceID); e != nil {
				return false, e
			}
			return false, nil
		}
		a.State = "terminated"
		a.EndReason = v.Reason
		if e = c.update(ctx, a); e != nil {
			return false, e
		}
	}
	if a.State == "registering" || a.State == "registration-unknown" {
		runner, found, e := c.Runners.Find(ctx, a.Spec.Name)
		if e != nil {
			return false, e
		}
		if found {
			a.RunnerID = runner.ID
		}
	}
	if a.RunnerID > 0 {
		runner, found, e := c.Runners.Get(ctx, a.RunnerID)
		if e != nil {
			return false, e
		}
		if found {
			if runner.Busy {
				return false, nil
			}
			if e = c.Runners.Remove(ctx, a.RunnerID); e != nil {
				return false, e
			}
		}
	}
	return true, c.forget(ctx, a)
}

// Reconcile runs even if GitHub discovery fails, ensuring lifetime limits do
// not depend on workflow availability. Uncertain launches keep their slots.
func (c *Controller) Reconcile(ctx context.Context) error {
	c.mu.Lock()
	state, e := c.Store.Load(ctx)
	if e == nil {
		inventory, err := c.Cloud.Inventory(ctx)
		c.inventoryBlocked = err != nil
		if err != nil {
			e = err
		} else {
			known := map[string]bool{}
			for id, a := range state.Allocations {
				matches := []ManagedInstance{}
				for _, v := range inventory {
					if v.Owned && v.Token == a.Spec.ClientToken {
						matches = append(matches, v)
						known[v.ID] = true
					}
				}
				if len(matches) > 1 {
					c.inventoryBlocked = true
					e = ErrUncertain
					continue
				}
				if len(matches) == 1 && a.InstanceID == "" && a.LaunchUncertain {
					a.InstanceID = matches[0].ID
					a.LaunchUncertain = false
					a.State = "booting"
					if err := c.update(ctx, a); err != nil {
						e = err
						continue
					}
					state.Allocations[id] = a
				}
			}
			for _, v := range inventory {
				if v.Owned && v.State != "terminated" && !known[v.ID] {
					c.inventoryBlocked = true
					if err := c.Cloud.Terminate(ctx, v.ID); err != nil {
						e = err
					}
				}
			}
		}
	}
	c.mu.Unlock()
	if e != nil {
		return e
	}
	var errs []error
	for id, a := range state.Allocations {
		if a.State == "terminating" {
			if _, err := c.Retire(ctx, id); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if !c.now().Before(a.Spec.Expires) && a.InstanceID != "" && a.State != "terminated" {
			// Retain the receipt until the queue has observed the worker result. Killing
			// a VM must never erase evidence needed to distinguish an interruption.
			if err := c.Cloud.Terminate(ctx, a.InstanceID); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
func (c *Controller) Status(ctx context.Context) ([]PublicAllocation, error) {
	s, e := c.Store.Load(ctx)
	if e != nil {
		return nil, e
	}
	r := []PublicAllocation{}
	for _, a := range s.Allocations {
		r = append(r, a.Public())
	}
	return r, nil
}

func (c *Controller) Policy(repository string) *Policy {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Settings.Policy(repository)
}

// Observe refreshes only public lifecycle data; it never starts another VM.
func (c *Controller) Observe(ctx context.Context, id string) (PublicAllocation, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, e := c.Store.Load(ctx)
	if e != nil {
		return PublicAllocation{}, false, e
	}
	a, ok := s.Allocations[id]
	if !ok {
		return PublicAllocation{}, false, nil
	}
	if a.InstanceID != "" && a.State != "terminated" {
		v, e := c.Cloud.Inspect(ctx, a.InstanceID)
		if e != nil {
			return a.Public(), true, e
		}
		if !v.Owned || v.ID != a.InstanceID {
			return a.Public(), true, errors.New("Spot instance ownership changed")
		}
		if v.State == "terminated" {
			a.State = "terminated"
			a.EndReason = v.Reason
			if e = c.update(ctx, a); e != nil {
				return a.Public(), true, e
			}
		}
	}
	if a.InstanceID != "" && a.State != "terminated" && a.State != "terminating" && a.RunnerID > 0 {
		runner, found, e := c.Runners.Get(ctx, a.RunnerID)
		if e != nil {
			return a.Public(), true, e
		}
		if found && runner.Busy && a.State != "running" {
			a.State = "running"
			if e = c.update(ctx, a); e != nil {
				return a.Public(), true, e
			}
		}
	}

	return a.Public(), true, nil
}
