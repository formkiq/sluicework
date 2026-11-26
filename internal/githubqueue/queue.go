// Package githubqueue schedules repository builds without occupying a runner
// while they wait. The same state machine runs in Lambda and an always-on server.
package githubqueue

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"sluicework/internal/spot"
	"sort"
	"strings"
	"time"
)

var ErrNotFound = errors.New("queue request not found")
var ErrExists = errors.New("queue request already exists")
var ErrConflict = errors.New("queue record changed")

var ErrSuperseded = errors.New("queue request superseded before dispatch")

type Priority string

const (
	High Priority = "high"
	Low  Priority = "low"
)

type Target struct {
	Platform string `json:"platform"`
	Label    string `json:"label"`
	Host     string `json:"host"`
}

func (t Target) Key() string { return strings.ToLower(t.Platform + ":" + t.Label) }

type Policy struct {
	Spot            *spot.Policy `json:"spot,omitempty"`
	Priority        Priority     `json:"priority"`
	PrimaryMinutes  int          `json:"primaryMinutes,omitempty"`
	Primary         Target       `json:"primary"`
	Fallback        Target       `json:"fallback"`
	Cloud           Target       `json:"cloud"`
	FallbackMinutes int          `json:"fallbackMinutes"`
}

func (p Policy) Wait() time.Duration {
	if p.Priority == High {
		if p.PrimaryMinutes == 15 {
			return 15 * time.Minute
		}
		// Older persisted requests predate the explicit primary-window snapshot.
		return 5 * time.Minute
	}
	return time.Hour
}
func validTarget(t Target) bool {
	switch t.Platform {
	case "linux-x64", "linux-arm64", "macos-x64", "macos-arm64":
	default:
		return false
	}
	return namePattern.MatchString(t.Label) && t.Label != "self-hosted" && t.Label != "linux" && t.Label != "macos" && t.Label != "x64" && t.Label != "arm64"
}
func (p Policy) CloudTier() int {
	if p.Spot != nil {
		return 3
	}
	return 2
}
func (p Policy) Validate() error {
	if p.Spot != nil {
		if e := p.Spot.Validate(); e != nil {
			return e
		}
	}
	if p.Priority != High && p.Priority != Low {
		return errors.New("select high or low repository priority")
	}
	if (p.Priority == High && p.PrimaryMinutes != 0 && p.PrimaryMinutes != 5 && p.PrimaryMinutes != 15) || (p.Priority == Low && p.PrimaryMinutes != 0 && p.PrimaryMinutes != 60) {
		return errors.New("invalid primary queue window")
	}
	if p.Primary.Host != "local" || p.Fallback.Host != "local" || p.Cloud.Host != "cloud" || p.Primary.Key() == p.Fallback.Key() || !(validTarget(p.Primary) || (p.Primary.Platform == "linux" && p.Primary.Label == "self-hosted")) || !validTarget(p.Fallback) || (p.Cloud.Platform != "linux-x64" && p.Cloud.Platform != "linux-arm64") || p.FallbackMinutes < 1 || p.FallbackMinutes > 5 {
		return errors.New("invalid ordered build targets")
	}
	return nil
}

type Attempt struct {
	RequestID       string    `json:"requestId"`
	Target          Target    `json:"target"`
	Tier            int       `json:"tier"`
	RunID           int64     `json:"runId,omitempty"`
	Started         time.Time `json:"started"`
	CancelRequested bool      `json:"cancelRequested,omitempty"`
	CancelReason    string    `json:"cancelReason,omitempty"`
	CancelAt        time.Time `json:"cancelAt,omitempty"`
}

type SpotClaim struct {
	QueuedAt   time.Time              `json:"queuedAt"`
	Allocation *spot.PublicAllocation `json:"allocation,omitempty"`
	Released   bool                   `json:"released,omitempty"`
}
type Request struct {
	Spot           *SpotClaim `json:"spot,omitempty"`
	ID             string     `json:"id"`
	Repository     string     `json:"repository"`
	RepositoryID   int64      `json:"repositoryId"`
	ParentID       int64      `json:"parentId"`
	ParentAttempt  int        `json:"parentAttempt"`
	ParentWorkflow string     `json:"parentWorkflow"`
	CheckID        int64      `json:"checkId"`
	CheckSHA       string     `json:"checkSha"`
	SourceSHA      string     `json:"sourceSha"`
	WorkflowSHA    string     `json:"workflowSha"`
	Ref            string     `json:"ref"`
	Kind           string     `json:"kind"`
	ArtifactID     int64      `json:"artifactId"`
	Created        time.Time  `json:"created"`
	Policy         Policy     `json:"policy"`
	Placement      string     `json:"placement"`
	Attempt        *Attempt   `json:"attempt,omitempty"`
	History        []Attempt  `json:"history,omitempty"`
	NextTier       int        `json:"nextTier"`
	State          string     `json:"state"`
	Error          string     `json:"error,omitempty"`
	Conclusion     string     `json:"conclusion,omitempty"`
	CallbackSent   bool       `json:"callbackSent,omitempty"`
	Version        int64      `json:"version"`
}

func (r Request) Key() string {
	return fmt.Sprintf("%d:%d:%d:%s", r.RepositoryID, r.ParentID, r.ParentAttempt, r.Kind)
}
func (r Request) Active() bool {
	return !r.Terminal() || !r.CallbackSent || (r.Spot != nil && r.Spot.Allocation != nil && !r.Spot.Released)
}
func (r Request) Terminal() bool { return r.State == "completed" || r.State == "failed" }
func (r Request) ExternalID() string {
	return fmt.Sprintf("sluicework-queue-v3:%d:%d:%s", r.ParentID, r.ParentAttempt, r.ID)
}
func (r Request) RunTitle() string {
	return "Sluicework build " + r.Attempt.RequestID + " @ " + r.SourceSHA
}

type Observation struct {
	Found      bool
	ID         int64
	Status     string
	Conclusion string
	Assigned   bool
	JobCount   int
}

type Store interface {
	// Lease serializes dispatch decisions across retries and concurrent processors.
	Lease(context.Context, string, time.Time, time.Duration) (bool, error)
	Release(context.Context, string) error
	Active(context.Context) ([]Request, error)
	Save(context.Context, *Request) error // compare-and-swap on Version
}
type GitHub interface {
	Discover(context.Context) ([]Request, error) // authenticated, allowlisted parent artifacts
	Observe(context.Context, Request) (Observation, error)
	Available(context.Context, Request, Target) (int, error)
	Dispatch(context.Context, Request) error
	Cancel(context.Context, Request) error
	Publish(context.Context, Request) error  // App-owned receipt, not the required build check
	Complete(context.Context, Request) error // trusted default-branch completion workflow
}

type Fleet interface {
	Refresh(context.Context) error
	Reconcile(context.Context) error
	Policy(string) *spot.Policy
	Ensure(context.Context, spot.Job) (spot.PublicAllocation, bool, error)
	Allocation(context.Context, string) (spot.PublicAllocation, bool, error)
	Retire(context.Context, string) (bool, error)
}
type Engine struct {
	Fleet  Fleet
	Store  Store
	GitHub GitHub
	ID     string
	Now    func() time.Time
}

func (e *Engine) Tick(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	owner := e.ID + ":" + uuid.NewString()
	now := time.Now().UTC()
	if e.Now != nil {
		now = e.Now()
	}
	ok, err := e.Store.Lease(ctx, owner, now, 2*time.Minute)
	if err != nil || !ok {
		return err
	}
	defer func() {
		releaseCtx, done := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer done()
		_ = e.Store.Release(releaseCtx, owner)
	}()
	var fleetErr error
	if e.Fleet != nil {
		// Cleanup is independent of GitHub discovery or policy availability.
		fleetErr = e.Fleet.Reconcile(ctx)
		fleetErr = errors.Join(fleetErr, e.Fleet.Refresh(ctx))
		if h, ok := e.Fleet.(interface{ Heartbeat(context.Context) error }); ok {
			fleetErr = errors.Join(fleetErr, h.Heartbeat(ctx))
		}
	}
	incoming, err := e.GitHub.Discover(ctx)
	if err != nil {
		return errors.Join(err, fleetErr)
	}
	for i := range incoming {
		candidate := incoming[i]
		candidate.Version = 0
		if e.Fleet != nil && candidate.Placement != "local-only" {
			candidate.Policy.Spot = e.Fleet.Policy(candidate.Repository)
		}
		if err = candidate.Policy.Validate(); err != nil {
			return err
		}
		if err = e.Store.Save(ctx, &candidate); err != nil && !errors.Is(err, ErrExists) {
			return err
		}
		if err == nil {
			if err = e.GitHub.Publish(ctx, candidate); err != nil {
				return err
			}
		}
	}
	requests, err := e.Store.Active(ctx)
	if err != nil {
		return err
	}
	var observationErrors []error
	// Complete observations before making new assignments. Errors preserve the
	// reservation: uncertainty never permits a second build on another target.
	for i := range requests {
		r := &requests[i]
		if r.Attempt == nil || r.Terminal() {
			continue
		}
		o, observeErr := e.GitHub.Observe(ctx, *r)
		if observeErr != nil {
			r.State = "blocked"
			r.Error = "Worker could not be authenticated; reservation retained without another dispatch"
			if err = e.Store.Save(ctx, r); err != nil {
				return err
			}
			if err = e.GitHub.Publish(ctx, *r); err != nil {
				return err
			}
			observationErrors = append(observationErrors, fmt.Errorf("%s parent %d: %w", r.Repository, r.ParentID, observeErr))
			continue
		}
		r.Error = ""
		if isSpotAttempt(*r) && e.Fleet != nil {
			allocation, found, err := e.Fleet.Allocation(ctx, r.ID)
			if err != nil {
				return err
			}
			if found {
				r.Spot.Allocation = &allocation
			}
			if found && allocation.State == "terminated" && interrupted(allocation.EndReason) {
				if !o.Found {
					r.State = "blocked"
					r.Error = "Spot interruption occurred before dispatch could be authenticated"
					if err = e.Store.Save(ctx, r); err != nil {
						return err
					}
					continue
				}
				if o.Status == "completed" {
					previous := *r.Attempt
					r.History = append(r.History, previous)
					r.Attempt = nil
					r.NextTier = r.Policy.CloudTier()
					r.State = "queued"
					if err = e.Store.Save(ctx, r); err != nil {
						return err
					}
					continue
				}
				if !r.Attempt.CancelRequested {
					r.Attempt.CancelRequested = true
					r.Attempt.CancelReason = "spot-interruption"
					r.Attempt.CancelAt = now
					if err = e.Store.Save(ctx, r); err != nil {
						return err
					}
					if err = e.GitHub.Cancel(ctx, *r); err != nil {
						return err
					}
				}
				r.State = "waiting"
				if err = e.Store.Save(ctx, r); err != nil {
					return err
				}
				continue
			}
		}
		if !o.Found {
			if now.Sub(r.Attempt.Started) >= 2*time.Minute {
				r.State = "blocked"
				r.Error = "Dispatch could not be confirmed; refusing another attempt"
			}
		} else {
			if r.Attempt.RunID != 0 && r.Attempt.RunID != o.ID {
				return errors.New("worker identity changed")
			}
			r.Attempt.RunID = o.ID
			switch {
			case r.Attempt.CancelRequested:
				if o.Status == "completed" {
					if o.Conclusion != "cancelled" || o.Assigned || o.JobCount == 0 {
						fail(r, "Cancellation raced assignment; refusing duplicate execution")
					} else {
						previous := *r.Attempt
						r.History = append(r.History, previous)
						r.Attempt = nil
						r.State = "queued"
						if previous.CancelReason == "priority" {
							r.NextTier = 0
						} else {
							r.NextTier = previous.Tier + 1
						}
					}
				} else if !now.Before(r.Attempt.CancelAt.Add(2 * time.Minute)) {
					r.State = "blocked"
					r.Error = "Cancellation was not confirmed; refusing fallback"
				}
			case o.Status == "completed":
				r.State = "completed"
				r.Conclusion = o.Conclusion
			case o.Assigned:
				r.State = "running"
			default:
				r.State = "waiting"
			}
		}
		if err = e.Store.Save(ctx, r); err != nil {
			return err
		}
		if err = e.GitHub.Publish(ctx, *r); err != nil {
			return err
		}
	}
	sort.SliceStable(requests, func(i, j int) bool {
		if requests[i].Policy.Priority != requests[j].Policy.Priority {
			return requests[i].Policy.Priority == High
		}
		if !requests[i].Created.Equal(requests[j].Created) {
			return requests[i].Created.Before(requests[j].Created)
		}
		return requests[i].ID < requests[j].ID
	})
	// At most one unassigned dispatch per physical target. Running jobs continue;
	// priority only affects work that has not started.
	occupied := map[string]int{}
	for _, r := range requests {
		if r.Attempt != nil && !r.Terminal() && r.State != "running" && r.Attempt.Target.Host == "local" {
			occupied[r.Attempt.Target.Key()]++
		}
	}
	for i := range requests {
		r := &requests[i]
		if r.Terminal() {
			if r.Spot != nil && r.Spot.Allocation != nil && !r.Spot.Released {
				if e.Fleet == nil {
					return errors.New("Spot cleanup requires the configured fleet")
				}
				done, err := e.Fleet.Retire(ctx, r.ID)
				if err != nil {
					return err
				}
				r.Spot.Released = done
				if err = e.Store.Save(ctx, r); err != nil {
					return err
				}
			}
			if !r.CallbackSent {
				if err = e.GitHub.Complete(ctx, *r); err != nil {
					return err
				}
				r.CallbackSent = true
				if err = e.Store.Save(ctx, r); err != nil {
					return err
				}
			}
			continue
		}
		if r.Attempt != nil {
			if r.State == "running" || r.State == "blocked" || r.Attempt.CancelRequested || r.Attempt.RunID == 0 || r.Attempt.Target.Host == "cloud" {
				continue
			}
			preempt := false
			if r.Policy.Priority == Low && r.Attempt.Tier == 0 {
				for _, h := range requests {
					if !h.Terminal() && h.Attempt == nil && h.Policy.Priority == High && h.NextTier == 0 && h.Policy.Primary.Key() == r.Attempt.Target.Key() && now.Before(h.Created.Add(h.Policy.Wait())) {
						preempt = true
						break
					}
				}
			}
			if !preempt && now.Before(deadline(*r, now)) {
				continue
			}
			// Re-read immediately before cancelling a queued attempt.
			o, err := e.GitHub.Observe(ctx, *r)
			if err != nil {
				return err
			}
			if !o.Found || o.Assigned || o.Status == "completed" {
				continue
			}
			r.Attempt.CancelRequested = true
			r.Attempt.CancelAt = now
			r.Attempt.CancelReason = "timeout"
			if preempt {
				r.Attempt.CancelReason = "priority"
			}
			if err = e.Store.Save(ctx, r); err != nil {
				return err
			}
			if err = e.GitHub.Cancel(ctx, *r); err != nil {
				return err
			}
			continue
		}
		if err = e.GitHub.Publish(ctx, *r); err != nil {
			return err
		}
		tier := r.NextTier
		if r.Placement == "cloud-only" && tier < 2 {
			tier = 2
		}
		if tier == 0 && !now.Before(r.Created.Add(r.Policy.Wait())) {
			tier = 1
		}
		target := r.Policy.Primary
		if tier == 1 {
			target = r.Policy.Fallback
		}
		if tier < 2 {
			available, err := e.GitHub.Available(ctx, *r, target)
			if err != nil {
				return err
			}
			if available <= occupied[target.Key()] {
				if tier == 0 {
					continue
				}
				// The Mac is a fallback only when it can offer capacity now.
				tier = 2
			}
		}
		if tier >= 2 && r.Placement == "local-only" {
			fail(r, "No selected local runner is available; cloud fallback is disabled")
			if err = e.Store.Save(ctx, r); err != nil {
				return err
			}
			continue
		}
		if tier == 2 && r.Policy.Spot != nil {
			selected, wait, err := e.spotTarget(ctx, r, now, fleetErr)
			if err != nil {
				return err
			}
			if wait {
				continue
			}
			if selected == nil {
				tier = r.Policy.CloudTier()
			} else {
				target = *selected
			}
		}
		if tier >= r.Policy.CloudTier() {
			// Before any hosted fallback, positively retire its allocated Spot VM.
			if r.Spot != nil && r.Spot.Allocation != nil && !r.Spot.Released {
				if e.Fleet == nil {
					return errors.New("Spot cleanup requires the configured fleet")
				}
				done, err := e.Fleet.Retire(ctx, r.ID)
				if err != nil {
					return err
				}
				r.Spot.Released = done
				if err = e.Store.Save(ctx, r); err != nil {
					return err
				}
				if !done {
					continue
				}
			}
			tier = r.Policy.CloudTier()
			target = r.Policy.Cloud
		}
		suffix := "local"
		if target.Host == "cloud" {
			suffix = "cloud"
		}
		if len(r.History) > 0 {
			suffix = fmt.Sprintf("fallback%d-%s", len(r.History), suffix)
		}
		r.Attempt = &Attempt{RequestID: fmt.Sprintf("%d-%d-%s-%s", r.ParentID, r.ParentAttempt, r.ID, suffix), Target: target, Tier: tier, Started: now}
		r.State = "dispatching"
		// Persist before POST. An interrupted/uncertain dispatch is discovered by
		// exact correlation on the next tick, and is never blindly retried.
		if err = e.Store.Save(ctx, r); err != nil {
			return err
		}
		if target.Host == "local" {
			occupied[target.Key()]++
		}
		if err = e.GitHub.Dispatch(ctx, *r); err != nil {
			if !errors.Is(err, ErrSuperseded) {
				return err
			}
			// The adapter proves it has not made a dispatch POST.
			r.Attempt = nil
			fail(r, "Source or parent attempt changed before dispatch")
			if err = e.Store.Save(ctx, r); err != nil {
				return err
			}
		}
		if err = e.GitHub.Publish(ctx, *r); err != nil {
			return err
		}
	}
	return errors.Join(append(observationErrors, fleetErr)...)
}
func deadline(r Request, now time.Time) time.Time {
	if r.Attempt.Tier == 0 {
		return r.Created.Add(r.Policy.Wait())
	}
	return r.Attempt.Started.Add(time.Duration(r.Policy.FallbackMinutes) * time.Minute)
}
func fail(r *Request, message string) {
	r.State = "failed"
	r.Error = message
	r.Conclusion = "failure"
}

func isSpotAttempt(r Request) bool {
	return r.Policy.Spot != nil && r.Attempt != nil && r.Attempt.Tier == 2 && r.Spot != nil && r.Spot.Allocation != nil
}
func interrupted(reason string) bool {
	return reason == "Server.SpotInstanceShutdown" || reason == "Server.SpotInstanceTermination"
}
func attemptID(r Request, host string) string {
	suffix := host
	if len(r.History) > 0 {
		suffix = fmt.Sprintf("fallback%d-%s", len(r.History), host)
	}
	return fmt.Sprintf("%d-%d-%s-%s", r.ParentID, r.ParentAttempt, r.ID, suffix)
}
func (e *Engine) spotTarget(ctx context.Context, r *Request, now time.Time, fleetErr error) (*Target, bool, error) {
	if e.Fleet == nil {
		return nil, false, errors.New("accepted Spot request requires its configured fleet")
	}
	if r.Spot == nil {
		r.Spot = &SpotClaim{QueuedAt: now}
		r.NextTier = 2
		if err := e.Store.Save(ctx, r); err != nil {
			return nil, false, err
		}
	}
	if r.Spot.Released {
		return nil, false, nil
	}
	// A persisted queue deadline applies only before a VM has been reserved.
	if r.Spot.Allocation == nil && (!now.Before(r.Spot.QueuedAt.Add(r.Policy.Spot.QueueWindow())) || fleetErr != nil) {
		r.NextTier = r.Policy.CloudTier()
		r.Spot.Released = true
		return nil, false, e.Store.Save(ctx, r)
	}
	branch := strings.TrimPrefix(r.Ref, "refs/heads/")
	j := spot.Job{ID: r.ID, Kind: r.Kind, Repository: r.Repository, WorkflowRef: r.Repository + "/.github/workflows/sluicework-build.yml@refs/heads/" + branch, WorkflowSHA: r.WorkflowSHA, SourceSHA: r.SourceSHA, RequestID: attemptID(*r, "local"), Policy: *r.Policy.Spot}
	a, ready, err := e.Fleet.Ensure(ctx, j)
	if a.ID != "" {
		r.Spot.Allocation = &a
	}
	if errors.Is(err, spot.ErrCapacity) {
		r.State = "waiting-spot"
		r.Error = "Waiting for one of the three shared Spot runner slots"
		return nil, true, e.Store.Save(ctx, r)
	}
	if errors.Is(err, spot.ErrDisabled) || errors.Is(err, spot.ErrUnavailable) {
		r.NextTier = r.Policy.CloudTier()
		if saveErr := e.Store.Save(ctx, r); saveErr != nil {
			return nil, false, saveErr
		}
		return nil, false, nil
	}
	if err != nil {
		r.State = "blocked"
		r.Error = "Spot allocation is unconfirmed; its reservation is retained"
		if saveErr := e.Store.Save(ctx, r); saveErr != nil {
			return nil, true, saveErr
		}
		return nil, true, nil
	}
	r.State = "starting-spot"
	r.Error = ""
	if err = e.Store.Save(ctx, r); err != nil {
		return nil, false, err
	}
	if !ready {
		return nil, true, nil
	}
	return &Target{Host: "local", Platform: "linux-arm64", Label: a.Label}, false, nil
}
