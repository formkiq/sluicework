package githubqueue

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"time"

	"sluicework/internal/githubapp"
)

// RequestFile is uploaded by the actual parent run, not supplied to a public API.
// Policy and repository identity come from the dispatcher's allowlist instead.
type RequestFile struct {
	Version       int    `json:"version"`
	BackendID     string `json:"backendId"`
	ID            string `json:"id"`
	ParentID      int64  `json:"parentId"`
	ParentAttempt int    `json:"parentAttempt"`
	CheckID       int64  `json:"checkId"`
	CheckSHA      string `json:"checkSha"`
	SourceSHA     string `json:"sourceSha"`
	WorkflowSHA   string `json:"workflowSha"`
	Ref           string `json:"ref"`
	Kind          string `json:"kind"`
	Placement     string `json:"placement"`
	PR            int    `json:"prNumber"`
	BaseSHA       string `json:"baseSha"`
}
type RepositoryAPI interface {
	QueueRequest(context.Context, string, string, any, any) error
	QueueRunners(context.Context) ([]githubapp.Runner, error)
	QueueArtifact(context.Context, int64, string) ([]byte, error)
}
type RecordReader interface {
	Get(context.Context, string) (Request, error)
}
type Peer struct {
	Config        Config
	Clients       map[string]RepositoryAPI
	Store         RecordReader
	Now           func() time.Time
	inventory     map[string][]githubapp.Runner
	seen          map[string]bool
	placements    map[string]string
	receiptIDs    map[string]int64
	workflowIDs   map[string]cachedWorkflowID
	heartbeatNext map[string]time.Time
}

type cachedWorkflowID struct {
	id      int64
	fetched time.Time
}

func (p *Peer) workflowID(ctx context.Context, repository, file string) (int64, error) {
	key := repository + ":" + file
	if cached, ok := p.workflowIDs[key]; ok && p.now().Sub(cached.fetched) < 5*time.Minute {
		return cached.id, nil
	}
	var workflow struct{ ID int64 }
	if err := p.api(ctx, repository, "GET", "/actions/workflows/"+file, nil, &workflow); err != nil {
		return 0, err
	}
	if workflow.ID <= 0 {
		return 0, errors.New("invalid registered workflow identity")
	}
	if p.workflowIDs == nil {
		p.workflowIDs = map[string]cachedWorkflowID{}
	}
	p.workflowIDs[key] = cachedWorkflowID{workflow.ID, p.now()}
	return workflow.ID, nil
}

type repoName struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
}
type workflowRun struct {
	ID             int64                 `json:"id"`
	Attempt        int                   `json:"run_attempt"`
	WorkflowID     int64                 `json:"workflow_id"`
	Path           string                `json:"path"`
	Event          string                `json:"event"`
	SHA            string                `json:"head_sha"`
	Branch         string                `json:"head_branch"`
	Title          string                `json:"display_title"`
	Status         string                `json:"status"`
	Conclusion     string                `json:"conclusion"`
	Repository     repoName              `json:"repository"`
	HeadRepository repoName              `json:"head_repository"`
	Actor          struct{ Type string } `json:"actor"`
	PRs            []struct {
		Number int `json:"number"`
	} `json:"pull_requests"`
}
type checkRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	SHA        string `json:"head_sha"`
	ExternalID string `json:"external_id"`
	Status     string `json:"status"`
	App        struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
	} `json:"app"`
	Output struct {
		Text string `json:"text"`
	} `json:"output"`
}
type artifact struct {
	ID      int64     `json:"id"`
	Name    string    `json:"name"`
	Digest  string    `json:"digest"`
	Size    int64     `json:"size_in_bytes"`
	Expired bool      `json:"expired"`
	Created time.Time `json:"created_at"`
	Run     struct {
		ID  int64  `json:"id"`
		SHA string `json:"head_sha"`
	} `json:"workflow_run"`
}

func (p *Peer) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}
func repoPath(name, path string) string { return "/repos/" + name + path }
func (p *Peer) api(ctx context.Context, repo, method, path string, body, out any) error {
	client := p.Clients[repo]
	if client == nil {
		return errors.New("repository outside dispatcher allowlist")
	}
	return client.QueueRequest(ctx, method, repoPath(repo, path), body, out)
}
func (p *Peer) Discover(ctx context.Context) ([]Request, error) {
	if err := p.Config.Validate(); err != nil {
		return nil, err
	}
	p.inventory = map[string][]githubapp.Runner{}
	p.placements = map[string]string{}
	if p.seen == nil {
		p.seen = map[string]bool{}
	}
	if err := p.heartbeat(ctx); err != nil {
		return nil, err
	}
	result := []Request{}
	for _, repo := range p.Config.Repositories {
		policy, enabled, err := p.repositoryPolicy(ctx, repo)
		if err != nil {
			return nil, err
		}
		if !enabled {
			continue
		}
		for _, coordinator := range repo.Coordinators {
			workflowID, err := p.workflowID(ctx, repo.Name, coordinator.File)
			if err != nil {
				return nil, err
			}
			for page := 1; page <= 10; page++ {
				var runs struct {
					Total int           `json:"total_count"`
					Runs  []workflowRun `json:"workflow_runs"`
				}
				path := fmt.Sprintf("/actions/workflows/%s/runs?status=completed&created=%s&per_page=100&page=%d", coordinator.File, url.QueryEscape(">="+p.discoverySince().Format(time.RFC3339)), page)
				if err := p.api(ctx, repo.Name, "GET", path, nil, &runs); err != nil {
					return nil, err
				}
				for _, run := range runs.Runs {
					if run.WorkflowID != workflowID || run.Path != ".github/workflows/"+coordinator.File || run.Status != "completed" || run.Conclusion != "success" || run.Repository.FullName != repo.Name || run.Repository.ID != repo.ID || !run.Repository.Private || run.HeadRepository.FullName != repo.Name || !shaPattern.MatchString(run.SHA) {
						continue
					}
					if run.Event != "push" && run.Event != "pull_request" && run.Event != "workflow_dispatch" {
						continue
					}
					if run.Event == "push" && run.Branch != repo.Branch {
						continue
					}
					key := fmt.Sprintf("%d:%d:%d:%s", repo.ID, run.ID, run.Attempt, coordinator.Kind)
					if p.seen[key] {
						continue
					}
					if _, err := p.Store.Get(ctx, key); err == nil {
						p.seen[key] = true
						continue
					} else if !errors.Is(err, ErrNotFound) {
						return nil, err
					}
					var list struct {
						Total     int        `json:"total_count"`
						Artifacts []artifact `json:"artifacts"`
					}
					if err := p.api(ctx, repo.Name, "GET", fmt.Sprintf("/actions/runs/%d/artifacts?per_page=100", run.ID), nil, &list); err != nil {
						return nil, err
					}
					if list.Total > 100 {
						continue
					}
					var found *artifact
					for i := range list.Artifacts {
						a := &list.Artifacts[i]
						if a.Name != fmt.Sprintf("sluicework-request-%d", run.Attempt) {
							continue
						}
						if found != nil {
							found = nil
							break
						}
						found = a
					}
					if found == nil {
						p.seen[key] = true
						continue
					}
					if found.Expired || found.ID <= 0 || found.Run.ID != run.ID || found.Run.SHA != run.SHA || found.Size <= 0 || found.Size > 32768 || found.Created.IsZero() {
						p.seen[key] = true
						continue
					}
					data, err := p.Clients[repo.Name].QueueArtifact(ctx, found.ID, found.Digest)
					if err != nil {
						continue
					} // retry downloads next tick without blocking other requests
					var file RequestFile
					decoder := json.NewDecoder(bytes.NewReader(data))
					decoder.DisallowUnknownFields()
					if err = decoder.Decode(&file); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
						p.seen[key] = true
						continue
					}
					if file.Version != 3 || file.BackendID != p.Config.BackendID {
						p.seen[key] = true
						continue
					}
					if !uuidPattern.MatchString(file.ID) || file.ParentID != run.ID || file.ParentAttempt != run.Attempt || file.Kind != coordinator.Kind || file.CheckID <= 0 || file.CheckSHA != run.SHA || !shaPattern.MatchString(file.SourceSHA) || !shaPattern.MatchString(file.WorkflowSHA) || !branchPattern.MatchString(file.Ref) || strings.Contains(file.Ref, "..") || !validPlacement(file.Placement) {
						p.seen[key] = true
						continue
					}
					// Trusted coordinators may remove cloud fallback, never grant local
					// execution when the administrator selected cloud-only placement.
					if run.Event != "workflow_dispatch" && file.Placement != p.placements[repo.Name] && !(file.Placement == "local-only" && p.placements[repo.Name] == "prefer-local") {
						continue
					}
					if err = p.authenticate(ctx, repo, run, file); err != nil {
						if errors.Is(err, errRejected) || errors.Is(err, githubapp.HTTPError(404)) {
							p.seen[key] = true
							continue
						}
						return nil, err
					}
					var check checkRun
					if err = p.api(ctx, repo.Name, "GET", fmt.Sprintf("/check-runs/%d", file.CheckID), nil, &check); err != nil {
						if errors.Is(err, githubapp.HTTPError(404)) {
							p.seen[key] = true
							continue
						}
						return nil, err
					}
					if check.Status == "completed" {
						p.seen[key] = true
						continue
					}
					if check.ID != file.CheckID || check.Name != coordinator.Check || check.SHA != run.SHA || check.App.Slug != "github-actions" || check.ExternalID != fmt.Sprintf("sluicework-v3:%d:%d:%s", run.ID, run.Attempt, file.ID) {
						p.seen[key] = true
						continue
					}
					result = append(result, Request{ID: file.ID, Repository: repo.Name, RepositoryID: repo.ID, ParentID: run.ID, ParentAttempt: run.Attempt, ParentWorkflow: coordinator.File, CheckID: file.CheckID, CheckSHA: file.CheckSHA, SourceSHA: file.SourceSHA, WorkflowSHA: file.WorkflowSHA, Ref: file.Ref, Kind: file.Kind, ArtifactID: found.ID, Created: found.Created, Placement: file.Placement, State: "queued", Policy: policy})
				}
				if page*100 >= runs.Total {
					break
				}
				if page == 10 {
					return nil, errors.New("parent request pagination limit exceeded")
				}
			}
		}
	}
	return result, nil
}
func validPlacement(s string) bool {
	return s == "prefer-local" || s == "local-only" || s == "cloud-only"
}
func (p *Peer) authenticate(ctx context.Context, repo Repository, parent workflowRun, file RequestFile) error {
	if parent.Event == "pull_request" {
		bound := false
		for _, pr := range parent.PRs {
			if pr.Number == file.PR {
				bound = true
			}
		}
		var pr struct {
			State string
			Head  struct {
				SHA, Ref string
				Repo     repoName
			}
			Base struct {
				Ref  string
				Repo repoName
			}
			User        struct{ Type string }
			Association string `json:"author_association"`
		}
		if !bound || file.PR <= 0 {
			return fmt.Errorf("%w: request has no parent PR binding", errRejected)
		}
		if err := p.api(ctx, repo.Name, "GET", fmt.Sprintf("/pulls/%d", file.PR), nil, &pr); err != nil {
			return err
		}
		if pr.State != "open" || pr.Head.Repo.FullName != repo.Name || pr.Base.Repo.FullName != repo.Name || pr.Base.Ref != repo.Branch || pr.Head.SHA != parent.SHA || pr.User.Type != "User" || parent.Actor.Type != "User" || (pr.Association != "OWNER" && pr.Association != "MEMBER" && pr.Association != "COLLABORATOR") || file.WorkflowSHA != pr.Head.SHA || file.Ref != pr.Head.Ref || !shaPattern.MatchString(file.BaseSHA) {
			return fmt.Errorf("%w: untrusted PR queue request", errRejected)
		}
		var commit struct{ Parents []struct{ SHA string } }
		if err := p.api(ctx, repo.Name, "GET", "/git/commits/"+file.SourceSHA, nil, &commit); err != nil {
			return err
		}
		head, base := false, false
		for _, parent := range commit.Parents {
			head = head || parent.SHA == file.CheckSHA
			base = base || parent.SHA == file.BaseSHA
		}
		if !head || !base {
			return fmt.Errorf("%w: source is not the requested PR merge", errRejected)
		}
	} else if file.PR != 0 || file.SourceSHA != parent.SHA || file.WorkflowSHA != parent.SHA || file.Ref != parent.Branch {
		return fmt.Errorf("%w: source does not match parent revision", errRejected)
	}
	var current struct{ Object struct{ SHA string } }
	if err := p.api(ctx, repo.Name, "GET", "/git/ref/heads/"+file.Ref, nil, &current); err != nil {
		return err
	}
	if current.Object.SHA != file.WorkflowSHA {
		return fmt.Errorf("%w: workflow branch moved before queue acceptance", errRejected)
	}
	return nil
}

func (p *Peer) Observe(ctx context.Context, r Request) (Observation, error) {
	workerID, err := p.workflowID(ctx, r.Repository, "sluicework-build.yml")
	if err != nil {
		return Observation{}, err
	}
	var run workflowRun
	if r.Attempt.RunID == 0 {
		var list struct {
			Runs []workflowRun `json:"workflow_runs"`
		}
		if err := p.api(ctx, r.Repository, "GET", "/actions/workflows/sluicework-build.yml/runs?event=workflow_dispatch&per_page=100", nil, &list); err != nil {
			return Observation{}, err
		}
		matches := 0
		for _, candidate := range list.Runs {
			if candidate.Title == r.RunTitle() {
				run = candidate
				matches++
			}
		}
		if matches == 0 {
			return Observation{}, nil
		}
		if matches != 1 {
			return Observation{}, errors.New("ambiguous worker dispatch; refusing another build")
		}
	} else if err := p.api(ctx, r.Repository, "GET", fmt.Sprintf("/actions/runs/%d", r.Attempt.RunID), nil, &run); err != nil {
		return Observation{}, err
	}
	if run.ID <= 0 || run.Attempt != 1 || run.WorkflowID != workerID || run.Event != "workflow_dispatch" || run.Repository.FullName != r.Repository || run.HeadRepository.FullName != r.Repository || run.Repository.ID != r.RepositoryID || !run.Repository.Private || run.SHA != r.WorkflowSHA || run.Branch != r.Ref || run.Title != r.RunTitle() {
		return Observation{}, errors.New("worker differs from authenticated request")
	}
	var list struct {
		Total int `json:"total_count"`
		Jobs  []struct {
			RunnerID           int64  `json:"runner_id"`
			RunnerName         string `json:"runner_name"`
			Status, Conclusion string
			Steps              []struct {
				Status  string
				Started *time.Time `json:"started_at"`
			}
		} `json:"jobs"`
	}
	if err := p.api(ctx, r.Repository, "GET", fmt.Sprintf("/actions/runs/%d/jobs?per_page=100", run.ID), nil, &list); err != nil {
		return Observation{}, err
	}
	if list.Total > 100 {
		return Observation{}, errors.New("too many worker jobs")
	}
	assigned := false
	for _, job := range list.Jobs {
		assigned = assigned || job.RunnerID > 0 || job.RunnerName != "" || job.Status == "in_progress" || (job.Status == "completed" && job.Conclusion != "cancelled" && job.Conclusion != "skipped")
		for _, step := range job.Steps {
			assigned = assigned || step.Started != nil || step.Status == "in_progress"
		}
	}
	return Observation{Found: true, ID: run.ID, Status: run.Status, Conclusion: run.Conclusion, Assigned: assigned, JobCount: len(list.Jobs)}, nil
}
func (p *Peer) Available(ctx context.Context, r Request, target Target) (int, error) {
	client := p.Clients[r.Repository]
	if client == nil {
		return 0, errors.New("unknown queue repository")
	}
	runners, cached := p.inventory[r.Repository]
	if !cached {
		var err error
		runners, err = client.QueueRunners(ctx)
		if err != nil {
			return 0, err
		}
		p.inventory[r.Repository] = runners
	}
	count := 0
	for _, runner := range runners {
		if runner.Status != "online" || runner.Busy {
			continue
		}
		labels := map[string]bool{}
		for _, label := range runner.Labels {
			labels[strings.ToLower(label.Name)] = true
		}
		os, arch, _ := strings.Cut(target.Platform, "-")
		if target.Platform == "linux" && target.Label == "self-hosted" && strings.EqualFold(runner.GroupName, "Default") && labels["self-hosted"] && labels["linux"] {
			count++
		} else if arch != "" && labels[strings.ToLower(target.Label)] && labels[os] && labels[arch] {
			count++
		}
	}
	return count, nil
}
func (p *Peer) Dispatch(ctx context.Context, r Request) error {
	var parent workflowRun
	if err := p.api(ctx, r.Repository, "GET", fmt.Sprintf("/actions/runs/%d", r.ParentID), nil, &parent); err != nil {
		return err
	}
	if parent.Attempt != r.ParentAttempt || parent.Status != "completed" || parent.Conclusion != "success" {
		return ErrSuperseded
	}

	var current struct{ Object struct{ SHA string } }
	if err := p.api(ctx, r.Repository, "GET", "/git/ref/heads/"+r.Ref, nil, &current); err != nil {
		return err
	}
	if current.Object.SHA != r.WorkflowSHA {
		return ErrSuperseded
	}
	label := r.Attempt.Target.Label
	if r.Attempt.Target.Host == "cloud" && label == "" {
		// The shared workflow requires this input even when hosted placement ignores it.
		label = "unused"
	}
	return p.api(ctx, r.Repository, "POST", "/actions/workflows/sluicework-build.yml/dispatches", map[string]any{"ref": r.Ref, "inputs": map[string]string{"request_id": r.Attempt.RequestID, "source_sha": r.SourceSHA, "host": r.Attempt.Target.Host, "build_kind": r.Kind, "local_platform": r.Attempt.Target.Platform, "machine_label": label}}, nil)
}
func (p *Peer) Cancel(ctx context.Context, r Request) error {
	if r.Attempt.RunID <= 0 {
		return errors.New("cannot cancel an unconfirmed dispatch")
	}
	return p.api(ctx, r.Repository, "POST", fmt.Sprintf("/actions/runs/%d/cancel", r.Attempt.RunID), nil, nil)
}

type Receipt struct {
	Version   int     `json:"version"`
	BackendID string  `json:"backendId"`
	Request   Request `json:"request"`
}

func (p *Peer) findCheck(ctx context.Context, repo, sha, name, external string) (checkRun, bool, error) {
	var match checkRun
	count := 0
	for page := 1; page <= 10; page++ {
		var list struct {
			Total  int        `json:"total_count"`
			Checks []checkRun `json:"check_runs"`
		}
		path := fmt.Sprintf("/commits/%s/check-runs?check_name=%s&filter=all&per_page=100&page=%d", sha, url.QueryEscape(name), page)
		if err := p.api(ctx, repo, "GET", path, nil, &list); err != nil {
			return match, false, err
		}
		for _, check := range list.Checks {
			if check.App.ID == p.Config.AppID && check.Name == name && check.SHA == sha && check.ExternalID == external {
				match = check
				count++
			}
		}
		if page*100 >= list.Total {
			break
		}
		if page == 10 {
			return match, false, errors.New("queue check pagination limit exceeded")
		}
	}
	if count > 1 {
		return match, false, errors.New("ambiguous App-owned queue receipt")
	}
	return match, count == 1, nil
}
func (p *Peer) Publish(ctx context.Context, r Request) error {
	snapshot := r
	snapshot.Version = 0
	snapshot.CallbackSent = false
	text, err := json.Marshal(Receipt{Version: 3, BackendID: p.Config.BackendID, Request: snapshot})
	if err != nil {
		return err
	}
	key := r.Repository + ":" + r.CheckSHA + ":" + r.ExternalID()
	var check checkRun
	var exists bool
	if id := p.receiptIDs[key]; id > 0 {
		// A just-created check may not be searchable yet. Use its returned ID
		// for later transitions, and fail closed if that identity changes.
		err = p.api(ctx, r.Repository, "GET", fmt.Sprintf("/check-runs/%d", id), nil, &check)
		if err == nil && (check.ID != id || check.App.ID != p.Config.AppID || check.Name != "Sluicework queue" || check.SHA != r.CheckSHA || check.ExternalID != r.ExternalID()) {
			err = errors.New("cached queue receipt identity changed")
		}
		exists = err == nil
	} else {
		check, exists, err = p.findCheck(ctx, r.Repository, r.CheckSHA, "Sluicework queue", r.ExternalID())
	}
	if err != nil {
		return err
	}
	if exists {
		if check.ID <= 0 {
			return errors.New("invalid queue receipt ID")
		}
		if p.receiptIDs == nil {
			p.receiptIDs = map[string]int64{}
		}
		p.receiptIDs[key] = check.ID
	}
	status := "in_progress"
	if r.Terminal() {
		status = "completed"
	}
	if exists && check.Output.Text == string(text) && check.Status == status {
		return nil
	}
	link := fmt.Sprintf("https://github.com/%s/actions/runs/%d", r.Repository, r.ParentID)
	if r.Attempt != nil && r.Attempt.RunID > 0 {
		link = fmt.Sprintf("https://github.com/%s/actions/runs/%d", r.Repository, r.Attempt.RunID)
	}
	body := map[string]any{"name": "Sluicework queue", "head_sha": r.CheckSHA, "external_id": r.ExternalID(), "status": status, "details_url": link, "output": map[string]string{"title": "Repository priority queue", "summary": fmt.Sprintf("%s priority · %s · Primary pool window %s. Required build results are reported after artifact verification.", r.Policy.Priority, r.State, r.Policy.Wait()), "text": string(text)}}
	if r.Terminal() {
		body["conclusion"] = "neutral"
	}
	if exists {
		return p.api(ctx, r.Repository, "PATCH", fmt.Sprintf("/check-runs/%d", check.ID), body, nil)
	}
	if err := p.api(ctx, r.Repository, "POST", "/check-runs", body, &check); err != nil {
		return err
	}
	if check.ID <= 0 || check.App.ID != p.Config.AppID || check.Name != "Sluicework queue" || check.SHA != r.CheckSHA || check.ExternalID != r.ExternalID() {
		return errors.New("invalid created queue receipt")
	}
	if p.receiptIDs == nil {
		p.receiptIDs = map[string]int64{}
	}
	p.receiptIDs[key] = check.ID
	return nil
}
func (p *Peer) Complete(ctx context.Context, r Request) error {
	var repo *Repository
	for i := range p.Config.Repositories {
		if p.Config.Repositories[i].Name == r.Repository {
			repo = &p.Config.Repositories[i]
		}
	}
	if repo == nil {
		return errors.New("completion outside configured repositories")
	}
	return p.api(ctx, r.Repository, "POST", "/actions/workflows/sluicework-complete.yml/dispatches", map[string]any{"ref": repo.Branch, "inputs": map[string]string{"parent_run_id": fmt.Sprint(r.ParentID), "parent_attempt": fmt.Sprint(r.ParentAttempt), "request_id": r.ID}}, nil)
}

var _ GitHub = (*Peer)(nil)

// Heartbeats use a fixed trusted commit so a new push does not race readiness.
// Output is refreshed at most every five minutes, not on every scheduler tick.
func (p *Peer) heartbeat(ctx context.Context) error {
	now := p.now()
	for _, repo := range p.Config.Repositories {
		external := "sluicework-dispatcher:" + p.Config.BackendID
		key := fmt.Sprintf("%s:%d:%s:%s", repo.Name, p.Config.AppID, repo.HeartbeatSHA, external)
		if now.Before(p.heartbeatNext[key]) {
			continue
		}
		old, exists, err := p.findCheck(ctx, repo.Name, repo.HeartbeatSHA, "Sluicework dispatcher", external)
		if err != nil {
			return err
		}
		var previous struct {
			At time.Time `json:"at"`
		}
		if p.heartbeatNext == nil {
			p.heartbeatNext = map[string]time.Time{}
		}
		if exists && json.Unmarshal([]byte(old.Output.Text), &previous) == nil && now.Sub(previous.At) >= -30*time.Second && now.Sub(previous.At) < 5*time.Minute {
			p.heartbeatNext[key] = previous.At.Add(5 * time.Minute)
			continue
		}
		encoded, _ := json.Marshal(map[string]any{"version": 3, "backendId": p.Config.BackendID, "at": now, "ready": true})
		body := map[string]any{"name": "Sluicework dispatcher", "head_sha": repo.HeartbeatSHA, "external_id": external, "status": "completed", "conclusion": "neutral", "output": map[string]string{"title": "Priority dispatcher available", "summary": "Queue decisions run without keeping a GitHub-hosted runner waiting.", "text": string(encoded)}}
		path := "/check-runs"
		method := "POST"
		if exists {
			path = fmt.Sprintf("/check-runs/%d", old.ID)
			method = "PATCH"
		}
		if err = p.api(ctx, repo.Name, method, path, body, nil); err != nil {
			return err
		}
		p.heartbeatNext[key] = now.Add(5 * time.Minute)
	}
	return nil
}

var errRejected = errors.New("rejected queue request")

func (p *Peer) discoverySince() time.Time {
	since := p.now().Add(-7 * 24 * time.Hour)
	if p.Config.Since.After(since) {
		return p.Config.Since
	}
	return since
}

// Policies come from protected repository settings/default-branch manifests,
// never from a request artifact or PR checkout.
func (p *Peer) repositoryPolicy(ctx context.Context, repo Repository) (Policy, bool, error) {
	var file struct{ Encoding, Content string }
	path := "/contents/.github/sluicework.json?ref=" + url.QueryEscape(repo.Branch)
	err := p.api(ctx, repo.Name, "GET", path, nil, &file)
	if errors.Is(err, githubapp.HTTPError(404)) {
		return Policy{}, false, nil
	}
	if err != nil {
		return Policy{}, false, err
	}
	if file.Encoding != "base64" || len(file.Content) > 32768 {
		return Policy{}, false, errors.New("invalid queue integration manifest")
	}
	data, err := base64.StdEncoding.DecodeString(file.Content)
	if err != nil {
		return Policy{}, false, err
	}
	var manifest githubapp.Integration
	if err = json.Unmarshal(data, &manifest); err != nil {
		return Policy{}, false, err
	}
	if manifest.Version != 3 {
		return Policy{}, false, nil
	}
	binding := manifest.Dispatcher
	if binding == nil || binding.AppID != p.Config.AppID || binding.BackendID != p.Config.BackendID || binding.HeartbeatSHA != repo.HeartbeatSHA {
		return Policy{}, false, errors.New("repository selected a different dispatcher")
	}
	routing := manifest.Defaults
	var variable struct{ Value string }
	err = p.api(ctx, repo.Name, "GET", "/actions/variables/SLUICEWORK_ROUTING", nil, &variable)
	if err == nil {
		routing, err = githubapp.ParseRouting([]byte(variable.Value))
	}
	if err != nil && !errors.Is(err, githubapp.HTTPError(404)) {
		return Policy{}, false, err
	}
	if routing.Version != 3 {
		return Policy{}, false, nil
	}
	if err = routing.Validate(); err != nil {
		return Policy{}, false, err
	}
	for _, selector := range routing.Selectors() {
		if !slices.Contains(manifest.Platforms, selector.Platform) {
			return Policy{}, false, errors.New("queue target is unsupported by the repository workflow")
		}
	}
	if !slices.Contains(manifest.Platforms, repo.CloudPlatform) {
		return Policy{}, false, errors.New("hosted target is unsupported by the repository workflow")
	}
	p.placements[repo.Name] = routing.Placement
	policy := Policy{Priority: Priority(routing.Priority), PrimaryMinutes: routing.QueueMinutes, Primary: Target{Platform: routing.Platform, Label: routing.MachineLabel, Host: "local"}, Fallback: Target{Platform: routing.FallbackRunner.Platform, Label: routing.FallbackRunner.MachineLabel, Host: "local"}, Cloud: Target{Platform: repo.CloudPlatform, Host: "cloud"}, FallbackMinutes: routing.FallbackQueueMinutes}
	return policy, true, policy.Validate()
}
