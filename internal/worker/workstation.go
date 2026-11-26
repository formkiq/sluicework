package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"sluicework/internal/githubapp"
	"sluicework/internal/state"
)

type workstationControl struct {
	EnabledUntil   time.Time `json:"enabledUntil"`
	ChangedAt      time.Time `json:"changedAt,omitempty"`
	AppliedControl string    `json:"appliedControl,omitempty"`
}
type workstationReport struct {
	Settings        state.WorkstationSettings `json:"settings"`
	DesiredSettings state.WorkstationSettings `json:"desiredSettings"`
	State           string                    `json:"state"`
	Reason          string                    `json:"reason,omitempty"`
	Updated         time.Time                 `json:"updated"`
	EnabledUntil    time.Time                 `json:"enabledUntil"`
	RunnerID        int64                     `json:"runnerId,omitempty"`
}

func atomicJSON(path string, value any) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".workstation-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = json.NewEncoder(f).Encode(value); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func control(path string) (workstationControl, error) {
	var c workstationControl
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if len(b) > 4096 {
		return c, errors.New("invalid workstation control file")
	}
	err = json.Unmarshal(b, &c)
	return c, err
}

func workstationConfig(cfg Config) (WorkstationConfig, string, error) {
	if cfg.Workstation == nil {
		return WorkstationConfig{}, "", errors.New("configure workstation.runnerDir and workDir")
	}
	w := cfg.Workstation.defaults()
	if !filepath.IsAbs(w.RunnerDir) || !filepath.IsAbs(cfg.WorkDir) || filepath.Clean(w.RunnerDir) == "/" || filepath.Clean(cfg.WorkDir) == "/" {
		return w, "", errors.New("workstation runnerDir and workDir must be absolute private directories")
	}
	// Keep controller state and the worker credential out of runner checkouts.
	inside := func(root, path string) bool {
		rel, err := filepath.Rel(root, path)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
	}
	if inside(w.RunnerDir, cfg.WorkDir) || inside(cfg.WorkDir, w.RunnerDir) || inside(w.RunnerDir, cfg.CredentialFile) {
		return w, "", errors.New("runnerDir, workDir and credentials must be separate")
	}
	if err := w.validate(); err != nil {
		return w, "", err
	}
	return w, filepath.Join(cfg.WorkDir, "workstation"), nil
}

// WorkstationCommand manages an opt-in foreground controller. It installs no
// LaunchDaemon, systemd unit, model runtime, or login item.
func WorkstationCommand(ctx context.Context, cfg Config, command string, out io.Writer) error {
	if os.Geteuid() == 0 {
		return errors.New("run workstation commands as an unprivileged runner account")
	}
	w, dir, err := workstationConfig(cfg)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	switch command {
	case "workstation-install":
		return installRunner(ctx, w.RunnerDir)
	case "workstation-enable":
		w = cachedSettings(filepath.Join(dir, "settings.json"), w)
		_, err := updateControl(dir, func(c *workstationControl) {
			c.EnabledUntil = time.Now().Add(time.Duration(w.EnableMinutes) * time.Minute)
			c.ChangedAt = time.Now()
		})
		return err
	case "workstation-pause":
		_, err := updateControl(dir, func(c *workstationControl) { c.EnabledUntil = time.Time{}; c.ChangedAt = time.Now() })
		return err
	case "workstation-status":
		b, err := os.ReadFile(filepath.Join(dir, "status.json"))
		if err != nil {
			return err
		}
		var r workstationReport
		if err = json.Unmarshal(b, &r); err != nil {
			return err
		}
		if time.Since(r.Updated) > 30*time.Second {
			r.State = "offline"
			r.Reason = "controller report expired"
		}
		return json.NewEncoder(out).Encode(r)
	case "workstation-check":
		c, err := control(filepath.Join(dir, "control.json"))
		if err != nil {
			return err
		}
		if !time.Now().Before(c.EnabledUntil) {
			return errors.New("workstation is paused")
		}
		client, err := newClient(cfg)
		if err != nil {
			return err
		}
		defer client.transport.CloseIdleConnections()
		if err = client.authenticate(); err != nil {
			return err
		}
		var machine state.Machine
		info := client.info()
		local := cachedSettings(filepath.Join(dir, "local-settings.json"), w)
		info.Workstation = &state.WorkstationStatus{SettingsVersion: 1, ControlVersion: 1, AppliedControl: c.AppliedControl, Local: local.settings(), Effective: w.settings(), EnabledUntil: c.EnabledUntil.Format(time.RFC3339)}
		if _, err = client.short(ctx, "heartbeat", info, &machine); err != nil {
			return errors.New("workstation server unavailable")
		}
		if machine.Draining || machine.Revoked || machine.Runner == nil || !machine.Runner.Workstation {
			return errors.New("workstation approval withdrawn")
		}
		grant, err := client.runner(ctx, "status")
		if err != nil || grant.Runner.ID == 0 {
			return errors.New("workstation GitHub connection or runner lease unavailable")
		}
		h, err := resources(ctx, w)
		if err != nil {
			return errors.New("workstation resource checks unavailable")
		}
		if reason := w.eligible(h); reason != "" {
			return errors.New(reason)
		}
		return os.WriteFile(filepath.Join(dir, "job-started"), []byte("started\n"), 0600)
	case "workstation":
		return runWorkstation(ctx, cfg, w, dir)
	default:
		return errors.New("unknown workstation command")
	}
}

func (c *client) runner(ctx context.Context, operation string) (githubapp.Grant, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var out githubapp.Grant
	_, err := c.post(ctx, "runner", map[string]string{"operation": operation}, &out)
	return out, err
}

type workstationRuntime struct {
	poll, drain, refresh time.Duration
	sample               func(context.Context, WorkstationConfig) (hostResources, error)
}

func runWorkstation(ctx context.Context, cfg Config, w WorkstationConfig, dir string) error {
	return runWorkstationRuntime(ctx, cfg, w, dir, workstationRuntime{poll: 5 * time.Second, drain: 75 * time.Second, refresh: time.Minute, sample: resources})
}

func runWorkstationRuntime(ctx context.Context, cfg Config, w WorkstationConfig, dir string, rt workstationRuntime) error {
	if len(cfg.Roles) != 1 || cfg.Roles[0] != "ci" {
		return errors.New("workstation config must use roles: [\"ci\"]; use a separate script/agent worker")
	}
	lock, err := os.OpenFile(filepath.Join(dir, "controller.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("workstation controller is already running")
	}
	if _, err = os.Stat(filepath.Join(w.RunnerDir, ".sluicework-runner")); err != nil {
		return errors.New("run workstation-install first")
	}
	runnerLock, err := os.OpenFile(filepath.Join(w.RunnerDir, ".sluicework-controller.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer runnerLock.Close()
	if err = syscall.Flock(int(runnerLock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another controller owns this runner directory")
	}
	c, err := newClient(cfg)
	if err != nil {
		return err
	}
	defer c.transport.CloseIdleConnections()
	if err = c.authenticate(); err != nil {
		return err
	}
	// Every new controller session starts paused, including after a crash/reboot.
	sessionStarted := time.Now()
	if _, err = updateControl(dir, func(c *workstationControl) { c.EnabledUntil = time.Time{}; c.ChangedAt = sessionStarted }); err != nil {
		return err
	}
	if err = atomicJSON(filepath.Join(dir, "worker.json"), cfg); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	hook := filepath.Join(dir, "job-start.sh")
	if err = os.WriteFile(hook, []byte("#!/bin/sh\nexec "+quote(exe)+" workstation-check --config "+quote(filepath.Join(dir, "worker.json"))+"\n"), 0700); err != nil {
		return err
	}
	var process *exec.Cmd
	var done chan error
	var runnerID int64
	var withdrawnAt time.Time
	var nextEnable time.Time
	var shutdown bool
	local := w
	desired := w.settings()
	if err := atomicJSON(filepath.Join(dir, "local-settings.json"), local.settings()); err != nil {
		return err
	}
	if prior, e := c.runner(ctx, "status"); e == nil {
		runnerID = prior.Runner.ID
	}
	status := workstationReport{State: "paused"}
	report := func() {
		status.Settings, status.DesiredSettings = w.settings(), desired
		status.Updated = time.Now()
		status.RunnerID = runnerID
		_ = atomicJSON(filepath.Join(dir, "status.json"), status)
	}
	defer func() { status.State = "offline"; report() }()
	// API calls during drain must outlive the foreground cancellation signal.
	background := context.WithoutCancel(ctx)
	for {
		if ctx.Err() != nil && !shutdown {
			shutdown = true
			// Close the start-hook gate as well as withdrawing remote labels.
			// In-flight assignments must not start fresh work during shutdown.
			if _, err := updateControl(dir, func(c *workstationControl) { c.EnabledUntil = time.Time{}; c.ChangedAt = time.Now() }); err != nil {
				log.Print("workstation could not persist its paused state; retry remote withdrawal")
			}
		}
		if done != nil {
			select {
			case runErr := <-done:
				process = nil
				done = nil
				if runErr != nil {
					log.Print("workstation runner exited; eligibility will be checked before retry")
				}
				// Ephemeral credentials/checkouts never carry into another job.
				_ = os.RemoveAll(filepath.Join(w.RunnerDir, "_work"))
				cleanup, cancel := context.WithTimeout(background, 30*time.Second)
				_, _ = c.runner(cleanup, "release")
				cancel()
				runnerID = 0
			default:
			}
		}
		ctrl, controlErr := control(filepath.Join(dir, "control.json"))
		status.EnabledUntil = ctrl.EnabledUntil
		enabled := !shutdown && controlErr == nil && time.Now().Before(ctrl.EnabledUntil)
		status.State = "paused"
		status.Reason = ""
		info := c.info()
		info.RunnerStatus = status.State
		info.Workstation = &state.WorkstationStatus{SettingsVersion: 1, ControlVersion: 1, AppliedControl: ctrl.AppliedControl, Local: local.settings(), Effective: w.settings(), EnabledUntil: ctrl.EnabledUntil.Format(time.RFC3339)}
		var m state.Machine
		_, heartErr := c.short(background, "heartbeat", info, &m)
		if !shutdown && heartErr == nil && m.Runner != nil && m.Runner.Control != nil {
			ctrl, controlErr = applyControl(dir, *m.Runner.Control, sessionStarted)
			enabled = controlErr == nil && time.Now().Before(ctrl.EnabledUntil)
			status.EnabledUntil = ctrl.EnabledUntil
		}
		if heartErr != nil || m.Draining || m.Revoked || m.Runner == nil || !m.Runner.Workstation {
			enabled = false
			status.Reason = "waiting for server approval"
		}
		desired = local.settings()
		if heartErr != nil {
			desired = w.settings()
		}
		if heartErr == nil && m.Runner != nil && m.Runner.Settings != nil {
			desired = *m.Runner.Settings
		}
		settingsValid := desired.Validate(runtime.GOOS) == nil
		if !settingsValid {
			enabled = false
			status.Reason = "invalid administrator workstation settings"
		}
		if settingsValid && heartErr == nil {
			_ = atomicJSON(filepath.Join(dir, "settings.json"), desired)
			if process == nil && runnerID == 0 {
				w = local.withSettings(desired)
			}
		}
		settingsPending := settingsValid && w.settings() != desired
		if settingsPending {
			enabled = false
			status.Reason = "workstation settings will apply after the current runner finishes"
		}
		if enabled {
			h, e := rt.sample(background, w)
			if e != nil {
				enabled = false
				status.Reason = "resource checks unavailable"
			} else if reason := w.eligible(h); reason != "" {
				enabled = false
				status.Reason = reason
			}
		}
		if enabled && !(process == nil && runnerID > 0) {
			if process == nil {
				grant, e := c.runner(background, "acquire")
				if e != nil {
					status.Reason = "waiting for GitHub runner grant"
				} else {
					runnerID = grant.Runner.ID
					if grant.JIT == "" {
						// A previous process may still own this lease. Withdraw first;
						// never launch a second listener for an existing registration.
						_, _ = c.runner(background, "pause")
						status.Reason = "previous runner lease requires cleanup"
					} else {
						_ = os.Remove(filepath.Join(dir, "job-started"))
						// The hook uses this immutable per-listener settings snapshot.
						snapshot := cfg
						snapshot.Workstation = &w
						if e = atomicJSON(filepath.Join(dir, "worker.json"), snapshot); e != nil {
							return e
						}
						process, e = startListener(w, dir, hook, grant.JIT)
						if e == nil {
							done = make(chan error, 1)
							p, ch := process, done
							go func() { ch <- p.Wait() }()
							withdrawnAt = time.Time{}
							nextEnable = time.Now().Add(rt.refresh)
						} else {
							process = nil
							_, _ = c.runner(background, "release")
							status.Reason = "runner failed to start"
						}
					}
				}
			} else if !withdrawnAt.IsZero() || !time.Now().Before(nextEnable) {
				// Reconcile periodically: the server may have withdrawn labels
				// during a temporary heartbeat outage, or after restarting.
				_, e := c.runner(background, "enable")
				if e != nil {
					enabled = false
					status.Reason = "GitHub eligibility update failed"
				} else {
					withdrawnAt = time.Time{}
					nextEnable = time.Now().Add(rt.refresh)
				}
			}
			if enabled && process != nil {
				status.State = "accepting"
			}
		}
		if (!enabled || process == nil) && runnerID > 0 && withdrawnAt.IsZero() {
			if _, e := c.runner(background, "pause"); e == nil {
				if withdrawnAt.IsZero() {
					withdrawnAt = time.Now()
				}
			} else {
				status.Reason = "retrying GitHub label withdrawal"
			}
		}
		var busy bool
		if runnerID > 0 {
			grant, e := c.runner(background, "status")
			busy = e != nil || grant.Runner.Busy // Unknown is treated as busy.
			if busy {
				status.State = "building"
				if !enabled {
					status.State = "draining"
				}
			}
		}
		if process == nil && runnerID > 0 && !busy && !withdrawnAt.IsZero() && time.Since(withdrawnAt) > rt.drain {
			if _, e := c.runner(background, "release"); e == nil {
				runnerID = 0
				withdrawnAt = time.Time{}
			}
		}
		if shutdown {
			status.State = "draining"
			// GitHub can take 60 seconds to deliver an assignment. After label
			// withdrawal, allow that window before stopping an idle listener.
			if process == nil && runnerID == 0 {
				return nil
			}
			_, started := os.Stat(filepath.Join(dir, "job-started"))
			if process != nil && !busy && !withdrawnAt.IsZero() && time.Since(withdrawnAt) > rt.drain && os.IsNotExist(started) {
				_ = process.Process.Signal(syscall.SIGTERM)
			}
		}
		if settingsPending && process != nil && !busy && !withdrawnAt.IsZero() && time.Since(withdrawnAt) > rt.drain {
			// Honor the same assignment-settling window as shutdown. An assigned
			// build keeps its original profile and is never killed for an edit.
			if _, e := os.Stat(filepath.Join(dir, "job-started")); os.IsNotExist(e) {
				_ = process.Process.Signal(syscall.SIGTERM)
			}
		}
		report()
		// Report the actual controller state, rather than just launchd/systemd.
		info.RunnerStatus = status.State
		info.RunnerReason = status.Reason
		info.Workstation.Effective = w.settings()
		info.Workstation.AppliedControl = ctrl.AppliedControl
		info.Workstation.EnabledUntil = ctrl.EnabledUntil.Format(time.RFC3339)
		_, _ = c.short(background, "heartbeat", info, &m)
		if heartErr != nil && unauthorized(heartErr) && process == nil {
			return heartErr
		}
		timer := time.NewTimer(rt.poll)
		if shutdown {
			<-timer.C
		} else {
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
			case <-timer.C:
			}
		}
	}
}

func startListener(w WorkstationConfig, dir, hook, jit string) (*exec.Cmd, error) {
	listener := filepath.Join(w.RunnerDir, "bin", "Runner.Listener")
	args := []string{"-n", "15", listener, "run"}
	command := "/usr/bin/nice"
	if runtime.GOOS == "darwin" {
		command = "/usr/sbin/taskpolicy"
		args = append([]string{"-b", "/usr/bin/nice"}, args...)
	}
	cmd := exec.Command(command, args...)
	cmd.Dir = w.RunnerDir
	// Do not pass the controller's cloud credentials or App secrets into jobs.
	for _, key := range []string{"HOME", "USER", "LOGNAME", "PATH", "LANG", "LC_ALL", "TMPDIR", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	threads := strconv.Itoa(w.BuildThreads)
	cmd.Env = append(cmd.Env, "ACTIONS_RUNNER_INPUT_JITCONFIG="+jit, "ACTIONS_RUNNER_HOOK_JOB_STARTED="+hook, "CARGO_BUILD_JOBS="+threads, "CMAKE_BUILD_PARALLEL_LEVEL="+threads, "MAKEFLAGS=-j"+threads)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	f, err := os.OpenFile(filepath.Join(dir, "runner.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cmd.Stdout = f
	cmd.Stderr = f
	if err = cmd.Start(); err != nil {
		return nil, fmt.Errorf("start runner: %w", err)
	}
	return cmd, nil
}
