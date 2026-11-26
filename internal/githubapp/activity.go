package githubapp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"sluicework/internal/state"
)

// Build is a GitHub-verified job executed by an enrolled Sluicework runner.
// Logs and artifacts remain on GitHub; this cache contains metadata only.
type Build struct {
	Repository  string     `json:"repository"`
	MachineID   string     `json:"machineId"`
	RunnerID    int64      `json:"runnerId"`
	RunID       int64      `json:"runId"`
	JobID       int64      `json:"jobId"`
	Attempt     int        `json:"attempt"`
	Workflow    string     `json:"workflow"`
	Name        string     `json:"name"`
	Branch      string     `json:"branch"`
	SHA         string     `json:"sha"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion,omitempty"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	Step        string     `json:"step,omitempty"`
	URL         string     `json:"url"`
}

type Activity struct {
	Repositories []string   `json:"repositories,omitempty"`
	Configured   bool       `json:"configured"`
	Repository   string     `json:"repository,omitempty"`
	SyncedAt     *time.Time `json:"syncedAt,omitempty"`
	Issue        string     `json:"issue,omitempty"`
	Builds       []Build    `json:"builds"`
}

type workflowRun struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Branch     string    `json:"head_branch"`
	SHA        string    `json:"head_sha"`
	Attempt    int       `json:"run_attempt"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	Updated    time.Time `json:"updated_at"`
}

type workflowJob struct {
	ID          int64      `json:"id"`
	RunID       int64      `json:"run_id"`
	Attempt     int        `json:"run_attempt"`
	Name        string     `json:"name"`
	RunnerID    int64      `json:"runner_id"`
	RunnerName  string     `json:"runner_name"`
	Labels      []string   `json:"labels"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
	Steps       []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"steps"`
}

type activitySource interface {
	ActivityRuns(context.Context) ([]workflowRun, error)
	ActivityRun(context.Context, int64) (workflowRun, error)
	ActivityJobs(context.Context, int64) ([]workflowJob, error)
}

func (c *Client) actions(ctx context.Context, path string, out any) error {
	token, err := c.installationToken(ctx, true)
	if err != nil {
		return err
	}
	return c.request(ctx, "GET", "/repos/"+c.config.Repository+"/actions"+path, token, nil, out)
}

// Discover the most recent 50 workflow runs and all active runs (up to 500).
// Previously tracked unfinished runs are refreshed separately, even if older.
func (c *Client) ActivityRuns(ctx context.Context) ([]workflowRun, error) {
	var recent struct {
		WorkflowRuns []workflowRun `json:"workflow_runs"`
	}
	if err := c.actions(ctx, "/runs?per_page=50", &recent); err != nil {
		return nil, err
	}
	runs := recent.WorkflowRuns
	for page := 1; page <= 5; page++ {
		var active struct {
			Total int           `json:"total_count"`
			Runs  []workflowRun `json:"workflow_runs"`
		}
		if err := c.actions(ctx, fmt.Sprintf("/runs?status=in_progress&per_page=100&page=%d", page), &active); err != nil {
			return nil, err
		}
		if active.Total > 500 {
			return nil, errors.New("too many active GitHub workflow runs to synchronize")
		}
		runs = append(runs, active.Runs...)
		if page*100 >= active.Total {
			break
		}
	}
	return runs, nil
}

func (c *Client) ActivityRun(ctx context.Context, id int64) (workflowRun, error) {
	var run workflowRun
	err := c.actions(ctx, fmt.Sprintf("/runs/%d", id), &run)
	return run, err
}

func (c *Client) ActivityJobs(ctx context.Context, id int64) ([]workflowJob, error) {
	jobs := []workflowJob{}
	for page := 1; page <= 5; page++ {
		var result struct {
			Total int           `json:"total_count"`
			Jobs  []workflowJob `json:"jobs"`
		}
		if err := c.actions(ctx, fmt.Sprintf("/runs/%d/jobs?filter=all&per_page=100&page=%d", id, page), &result); err != nil {
			return nil, err
		}
		if result.Total > 500 {
			return nil, errors.New("too many GitHub workflow jobs to synchronize")
		}
		jobs = append(jobs, result.Jobs...)
		if page*100 >= result.Total {
			return jobs, nil
		}
	}
	return jobs, nil
}

const activitySchema = `
CREATE TABLE IF NOT EXISTS github_builds (repository TEXT NOT NULL, job INTEGER NOT NULL, run INTEGER NOT NULL, machine TEXT NOT NULL, completed INTEGER NOT NULL, started INTEGER NOT NULL, data TEXT NOT NULL, PRIMARY KEY(repository,job));
CREATE INDEX IF NOT EXISTS github_builds_history ON github_builds(repository,started DESC,job DESC);
CREATE TABLE IF NOT EXISTS github_activity_runs (repository TEXT NOT NULL, run INTEGER NOT NULL, fingerprint TEXT NOT NULL, PRIMARY KEY(repository,run));
CREATE TABLE IF NOT EXISTS github_activity_sync (repository TEXT PRIMARY KEY, synced TEXT NOT NULL DEFAULT '', issue TEXT NOT NULL DEFAULT '');
`

func (b *Broker) activity(ctx context.Context) (Activity, error) {
	result := Activity{Configured: true, Repository: b.repository, Builds: []Build{}}
	var synced string
	err := b.db.QueryRowContext(ctx, "SELECT synced,issue FROM github_activity_sync WHERE repository=?", b.repository).Scan(&synced, &result.Issue)
	if err == nil && synced != "" {
		at, parseErr := time.Parse(time.RFC3339Nano, synced)
		if parseErr != nil {
			return result, parseErr
		}
		result.SyncedAt = &at
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	rows, err := b.db.QueryContext(ctx, "SELECT data FROM github_builds WHERE repository=? ORDER BY completed,started DESC,job DESC LIMIT 100", b.repository)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var build Build
		if err = rows.Scan(&raw); err != nil {
			return result, err
		}
		if err = json.Unmarshal([]byte(raw), &build); err != nil {
			return result, err
		}
		result.Builds = append(result.Builds, build)
	}
	return result, rows.Err()
}

// RefreshActivity runs outside the runner-control mutex. A slow or forbidden
// Actions API request must never block registration, admission or graceful drain.
func (b *Broker) RefreshActivity(ctx context.Context, machines []state.Machine) {
	b.refreshActivity(ctx, machines)
	for _, child := range b.children {
		child.refreshActivity(ctx, machines)
	}
}
func (b *Broker) refreshActivity(ctx context.Context, machines []state.Machine) {
	b.activityMu.Lock()
	defer b.activityMu.Unlock()
	if b.activityClient == nil || time.Now().Before(b.activityNext) {
		return
	}
	b.activityNext = time.Now().Add(time.Minute)
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	err := b.syncActivity(ctx, machines)
	issue := ""
	if err != nil {
		issue = "GitHub activity sync is temporarily unavailable. Showing last known build records."
		if errors.Is(err, HTTPError(401)) || errors.Is(err, HTTPError(403)) || errors.Is(err, HTTPError(422)) {
			issue = "GitHub activity access was rejected. Check the App's Actions: read permission and GitHub API limits. Runner availability is unaffected."
			b.activityNext = time.Now().Add(5 * time.Minute)
		}
		_, _ = b.db.Exec("INSERT INTO github_activity_sync(repository,issue) VALUES(?,?) ON CONFLICT(repository) DO UPDATE SET issue=excluded.issue", b.repository, issue)
		return
	}
	_, _ = b.db.Exec("INSERT INTO github_activity_sync(repository,synced,issue) VALUES(?,?,'') ON CONFLICT(repository) DO UPDATE SET synced=excluded.synced,issue=''", b.repository, time.Now().UTC().Format(time.RFC3339Nano))
}

func (b *Broker) syncActivity(ctx context.Context, machines []state.Machine) error {
	var registered bool
	for _, m := range machines {
		if m.Runner != nil && m.Runner.Workstation && strings.EqualFold(m.Runner.Scope, b.machineScope()) {
			registered = true
			break
		}
	}
	if !registered {
		var unfinished int
		if err := b.db.QueryRowContext(ctx, "SELECT count(*) FROM github_builds WHERE repository=? AND completed=0", b.repository).Scan(&unfinished); err != nil {
			return err
		}
		if unfinished == 0 {
			return nil
		}
	}
	runs, err := b.activityClient.ActivityRuns(ctx)
	if err != nil {
		return err
	}
	byID := map[int64]workflowRun{}
	for _, run := range runs {
		byID[run.ID] = run
	}
	rows, err := b.db.QueryContext(ctx, "SELECT DISTINCT run FROM github_builds WHERE repository=? AND completed=0", b.repository)
	if err != nil {
		return err
	}
	missing := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if _, found := byID[id]; !found {
			missing = append(missing, id)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var unavailable bool
	for _, id := range missing {
		run, err := b.activityClient.ActivityRun(ctx, id)
		if errors.Is(err, HTTPError(404)) {
			// A removed workflow cannot establish an outcome; retain its last state
			// and surface a sync issue instead of inventing a successful completion.
			unavailable = true
			continue
		}
		if err != nil {
			return err
		}
		byID[id] = run
	}
	for _, run := range byID {
		if run.ID <= 0 {
			continue
		}
		fingerprint := fmt.Sprintf("%d:%s:%s:%s", run.Attempt, run.Status, run.Conclusion, run.Updated.Format(time.RFC3339Nano))
		var previous string
		_ = b.db.QueryRowContext(ctx, "SELECT fingerprint FROM github_activity_runs WHERE repository=? AND run=?", b.repository, run.ID).Scan(&previous)
		if run.Status == "completed" && previous == fingerprint {
			continue
		}
		jobs, err := b.activityClient.ActivityJobs(ctx, run.ID)
		if err != nil {
			return err
		}
		tx, err := b.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, job := range jobs {
			var machineID string
			for _, m := range machines {
				if len(m.ID) < 12 || m.Runner == nil || !m.Runner.Workstation || !strings.EqualFold(m.Runner.Scope, b.machineScope()) || job.RunnerID <= 0 || job.RunnerName != "sluicework-"+m.ID {
					continue
				}
				for _, label := range job.Labels {
					if strings.EqualFold(label, "sluicework-"+m.ID[:12]) {
						machineID = m.ID
						break
					}
				}
				if machineID != "" {
					break
				}
			}
			if job.ID <= 0 || job.RunID != run.ID {
				continue
			}
			if machineID == "" {
				// Keep updating a previously verified assignment after its machine
				// is revoked or its runner approval is removed.
				var raw string
				var prior Build
				if e := tx.QueryRowContext(ctx, "SELECT data FROM github_builds WHERE repository=? AND job=?", b.repository, job.ID).Scan(&raw); e == nil {
					if json.Unmarshal([]byte(raw), &prior) == nil && prior.RunID == run.ID && prior.RunnerID == job.RunnerID && job.RunnerName == "sluicework-"+prior.MachineID {
						machineID = prior.MachineID
					}
				}
			}
			if machineID == "" {
				continue
			}
			build := Build{Repository: b.repository, MachineID: machineID, RunnerID: job.RunnerID, RunID: run.ID, JobID: job.ID, Attempt: job.Attempt, Workflow: run.Name, Name: job.Name, Branch: run.Branch, SHA: run.SHA, Status: job.Status, Conclusion: job.Conclusion, StartedAt: job.StartedAt, CompletedAt: job.CompletedAt,
				URL: fmt.Sprintf("https://github.com/%s/actions/runs/%d/job/%d", url.PathEscape(strings.Split(b.repository, "/")[0])+"/"+url.PathEscape(strings.Split(b.repository, "/")[1]), run.ID, job.ID)}
			if build.Attempt == 0 {
				build.Attempt = run.Attempt
			}
			for _, step := range job.Steps {
				if job.Status == "in_progress" && step.Status == "in_progress" {
					build.Step = step.Name
					break
				}
			}
			raw, err := json.Marshal(build)
			if err != nil {
				tx.Rollback()
				return err
			}
			var started int64
			if build.StartedAt != nil {
				started = build.StartedAt.UnixMilli()
			}
			_, err = tx.ExecContext(ctx, "INSERT INTO github_builds(repository,job,run,machine,completed,started,data) VALUES(?,?,?,?,?,?,?) ON CONFLICT(repository,job) DO UPDATE SET completed=excluded.completed,data=excluded.data,started=excluded.started", b.repository, job.ID, run.ID, machineID, job.Status == "completed", started, string(raw))
			if err != nil {
				tx.Rollback()
				return err
			}
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO github_activity_runs(repository,run,fingerprint) VALUES(?,?,?) ON CONFLICT(repository,run) DO UPDATE SET fingerprint=excluded.fingerprint", b.repository, run.ID, fingerprint)
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	_, err = b.db.ExecContext(ctx, "DELETE FROM github_builds WHERE repository=? AND completed=1 AND job NOT IN (SELECT job FROM github_builds WHERE repository=? ORDER BY started DESC,job DESC LIMIT 1000)", b.repository, b.repository)
	if err != nil {
		return err
	}
	_, err = b.db.ExecContext(ctx, "DELETE FROM github_activity_runs WHERE repository=? AND run NOT IN (SELECT run FROM github_activity_runs WHERE repository=? ORDER BY run DESC LIMIT 1000)", b.repository, b.repository)
	if err == nil && unavailable {
		return errors.New("previously tracked GitHub workflow is unavailable")
	}
	return err
}

// Activity combines only the explicitly configured repositories.
func (b *Broker) Activity(ctx context.Context) (Activity, error) {
	result, err := b.activity(ctx)
	if len(b.children) > 0 {
		result.Repositories = b.Repositories()
	}
	if err != nil {
		return result, err
	}
	for _, child := range b.children {
		activity, e := child.activity(ctx)
		if e != nil {
			return result, e
		}
		result.Builds = append(result.Builds, activity.Builds...)
		if activity.Issue != "" {
			result.Issue = activity.Issue
		}
		if activity.SyncedAt != nil && (result.SyncedAt == nil || activity.SyncedAt.Before(*result.SyncedAt)) {
			result.SyncedAt = activity.SyncedAt
		}
	}
	sort.SliceStable(result.Builds, func(i, j int) bool {
		a, z := result.Builds[i], result.Builds[j]
		if (a.Status == "completed") != (z.Status == "completed") {
			return a.Status != "completed"
		}
		if a.StartedAt == nil {
			return false
		}
		if z.StartedAt == nil {
			return true
		}
		return a.StartedAt.After(*z.StartedAt)
	})
	if len(result.Builds) > 100 {
		result.Builds = result.Builds[:100]
	}
	return result, nil
}
