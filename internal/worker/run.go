package worker

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"sluicework/internal/state"
)

var errExpired = errors.New("worker lost its attempt lease")
var errCancelled = errors.New("cancellation requested")

// Run keeps one outbound worker connection pool and polls for work until shutdown.
func Run(ctx context.Context, cfg Config) error {
	if os.Geteuid() == 0 {
		return errors.New("run the worker under its dedicated unprivileged account")
	}
	c, err := newClient(cfg)
	if err != nil {
		return err
	}
	defer c.transport.CloseIdleConnections()
	if err := c.authenticate(); err != nil {
		return err
	}
	if cfg.WorkDir == "" {
		return errors.New("configure a worker scratch directory")
	}
	c.config.WorkDir, err = filepath.Abs(cfg.WorkDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.config.WorkDir, 0700); err != nil {
		return err
	}
	ctx, stop := context.WithCancel(ctx)
	var running atomic.Int64
	var wg sync.WaitGroup
	defer func() { stop(); wg.Wait() }()
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	var nextHeartbeat time.Time
	var slots int
	service := cfg.RunnerService
	var lastError string
	report := func(err error) {
		if err == nil {
			lastError = ""
			return
		}
		if ctx.Err() == nil && err.Error() != lastError {
			lastError = err.Error()
			log.Printf("worker control request failed: %s", lastError)
		}
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		if !time.Now().Before(nextHeartbeat) {
			info := c.info()
			info.RunnerStatus = runnerStatus(ctx, service, cfg.RunnerStatusFile)
			var m state.Machine
			_, err := c.short(ctx, "heartbeat", info, &m)
			report(err)
			if unauthorized(err) {
				return err
			}
			if err != nil {
				slots = 0
			} else {
				slots = m.Slots
				if cfg.Slots > 0 {
					slots = min(slots, cfg.Slots)
				}
				if m.Draining || m.Revoked {
					slots = 0
				}
				service = cfg.RunnerService
				if m.Runner != nil && m.Runner.Service != "" {
					service = m.Runner.Service
				}
			}
			nextHeartbeat = time.Now().Add(5 * time.Second)
		}
		for running.Load() < int64(slots) {
			var claim state.Claim
			status, err := c.short(ctx, "claim", struct{}{}, &claim)
			report(err)
			if unauthorized(err) {
				return err
			}
			if err != nil || status == http.StatusNoContent || claim.AttemptID == "" {
				break
			}
			running.Add(1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer running.Add(-1)
				c.attempt(ctx, claim)
			}()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-poll.C:
		}
	}
}

func attemptRoute(id, operation string) string {
	return "attempts/" + url.PathEscape(id) + "/" + operation
}

// supervise renews independently of job execution, including while logs are being
// uploaded. The expiry deadline bounds every renewal request and network retry.
func (c *client) supervise(ctx context.Context, claim state.Claim, stopLease, stopJob context.CancelCauseFunc) {
	until := claim.LeaseUntil
	for {
		remaining := time.Until(until)
		if remaining <= 0 {
			stopLease(errExpired)
			return
		}
		delay := min(remaining/3, 3*time.Second)
		if delay < 30*time.Millisecond {
			delay = min(30*time.Millisecond, remaining)
		}
		if !pause(ctx, delay) {
			return
		}
		request, cancel := context.WithDeadline(ctx, until)
		var renewal state.Renewal
		_, err := c.short(request, attemptRoute(claim.AttemptID, "renew"), struct{}{}, &renewal)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if permanent(err) {
			stopLease(errExpired)
			return
		}
		if err != nil {
			continue
		}
		if renewal.CancelRequested {
			stopJob(errCancelled)
		}
		until = renewal.LeaseUntil
	}
}

func pause(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (c *client) attempt(parent context.Context, claim state.Claim) {
	if time.Until(claim.LeaseUntil) <= 0 {
		return
	}
	lease, stopLease := context.WithCancelCause(parent)
	job, stopJob := context.WithCancelCause(lease)
	monitorDone := make(chan struct{})
	go func() { defer close(monitorDone); c.supervise(lease, claim, stopLease, stopJob) }()
	defer func() { stopLease(nil); stopJob(nil); <-monitorDone }()
	run, cancelTimeout := context.WithTimeout(job, time.Duration(claim.Job.TimeoutSeconds)*time.Second)
	completion := c.execute(run, claim)
	cause := context.Cause(run)
	cancelTimeout()
	if errors.Is(cause, context.DeadlineExceeded) {
		completion.State = "timed_out"
		completion.Error = "wall-time limit exceeded"
	}
	report, cancelReport := context.WithTimeout(lease, 30*time.Second)
	defer cancelReport()
	for report.Err() == nil {
		if errors.Is(context.Cause(job), errCancelled) {
			completion.State = "cancelled"
			completion.Error = ""
		}
		_, err := c.short(report, attemptRoute(claim.AttemptID, "complete"), completion, nil)
		if err == nil || permanent(err) {
			return
		}
		if !pause(report, 500*time.Millisecond) {
			return
		}
	}
}
