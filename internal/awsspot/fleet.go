package awsspot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"sluicework/internal/githubapp"
	"sluicework/internal/spot"
	"time"
)

type Fleet struct {
	Controller                   *spot.Controller
	Control                      *githubapp.Client
	ControlRepository, BackendID string
	Approved                     []string
	HeartbeatSHA                 string
	AppID                        int64
}

func NewFleet(ctx context.Context, infra spot.Infrastructure, backend string, approved []string, app githubapp.Config, key []byte, awsConfig aws.Config, store spot.Store) (*Fleet, error) {
	if e := infra.Validate(approved); e != nil {
		return nil, e
	}
	if awsConfig.Region != infra.Region {
		return nil, errors.New("AWS identity region differs from the Spot deployment")
	}
	identity, e := sts.NewFromConfig(awsConfig).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if e != nil || identity == nil || aws.ToString(identity.Account) != infra.AccountID {
		return nil, errors.New("AWS execution identity differs from the Spot deployment account")
	}
	app.Repository = infra.ControlRepository
	app.OrganizationRunners = true
	app.WorkstationGroupID = infra.RunnerGroupID
	app.WorkstationRepositories = append([]string{}, approved...)
	reg, e := githubapp.NewWithPEM(app, key)
	if e != nil {
		return nil, e
	}
	// Policy reads use the queue's down-scoped token; registration has its own token.
	readApp := app
	readApp.WorkstationGroupID = 0
	readApp.WorkstationRepositories = nil
	readApp.OrganizationRunners = false
	control, e := githubapp.NewWithPEM(readApp, key)
	if e != nil {
		return nil, e
	}
	c := &spot.Controller{BackendID: backend, Infrastructure: infra, Approved: approved, Store: store, Cloud: &EC2Cloud{Client: ec2.NewFromConfig(awsConfig), BackendID: backend, AccountID: infra.AccountID}, Runners: &GitHubRunners{Client: reg}, Settings: spot.Defaults(backend)}
	if e = c.Validate(); e != nil {
		return nil, e
	}
	return &Fleet{Controller: c, Control: control, ControlRepository: infra.ControlRepository, BackendID: backend, Approved: approved, AppID: app.AppID}, nil
}
func utcNow() string { return time.Now().UTC().Format(time.RFC3339) }
func (f *Fleet) Refresh(ctx context.Context) error {
	var value struct {
		Value string `json:"value"`
	}
	e := f.Control.QueueRequest(ctx, "GET", "/repos/"+f.ControlRepository+"/actions/variables/"+spot.Variable, nil, &value)
	settings := spot.Defaults(f.BackendID)
	if errors.Is(e, githubapp.HTTPError(404)) {
		return f.Controller.SetSettings(settings)
	}
	if e != nil {
		settings.Enabled = false
		_ = f.Controller.SetSettings(settings)
		return errors.New("Spot configuration could not be refreshed; new admissions are paused")
	}
	settings, e = spot.ParseSettings(value.Value, f.BackendID, f.Approved)
	if e != nil {
		_ = f.Controller.SetSettings(spot.Defaults(f.BackendID))
		return e
	}
	return f.Controller.SetSettings(settings)
}
func (f *Fleet) Policy(repository string) *spot.Policy { return f.Controller.Policy(repository) }
func (f *Fleet) Reconcile(ctx context.Context) error   { return f.Controller.Reconcile(ctx) }
func (f *Fleet) Ensure(ctx context.Context, j spot.Job) (spot.PublicAllocation, bool, error) {
	return f.Controller.Ensure(ctx, j)
}
func (f *Fleet) Retire(ctx context.Context, id string) (bool, error) {
	return f.Controller.Retire(ctx, id)
}
func (f *Fleet) Allocation(ctx context.Context, id string) (spot.PublicAllocation, bool, error) {
	return f.Controller.Observe(ctx, id)
}
func (f *Fleet) Publish(ctx context.Context, sha string, appID int64) error {
	all, e := f.Controller.Status(ctx)
	if e != nil {
		return e
	}
	health := struct {
		Version     int                     `json:"version"`
		BackendID   string                  `json:"backendId"`
		Provisioned bool                    `json:"provisioned"`
		At          string                  `json:"at"`
		Region      string                  `json:"region"`
		HardLimit   int                     `json:"hardLimit"`
		Profiles    []spot.Profile          `json:"profiles"`
		Allocations []spot.PublicAllocation `json:"allocations"`
	}{1, f.BackendID, true, utcNow(), f.Controller.Infrastructure.Region, spot.HardLimit, spot.Profiles(), all}
	text, e := json.Marshal(health)
	if e != nil {
		return e
	}
	external := "sluicework-spot:" + f.BackendID
	var list struct {
		Total  int `json:"total_count"`
		Checks []struct {
			ID       int64  `json:"id"`
			SHA      string `json:"head_sha"`
			External string `json:"external_id"`
			App      struct {
				ID int64 `json:"id"`
			} `json:"app"`
		} `json:"check_runs"`
	}
	e = f.Control.QueueRequest(ctx, "GET", "/repos/"+f.ControlRepository+"/commits/"+sha+"/check-runs?check_name=Sluicework%20Spot%20fleet&filter=all&per_page=100", nil, &list)
	if e != nil {
		return e
	}
	if list.Total > 100 {
		return errors.New("Spot health history is ambiguous")
	}
	method, path := "POST", "/repos/"+f.ControlRepository+"/check-runs"
	count := 0
	for _, c := range list.Checks {
		if c.SHA == sha && c.External == external && c.App.ID == appID {
			count++
			method = "PATCH"
			path = fmt.Sprintf("/repos/%s/check-runs/%d", f.ControlRepository, c.ID)
		}
	}
	if count > 1 {
		return errors.New("Spot health identity is ambiguous")
	}
	body := map[string]any{"name": "Sluicework Spot fleet", "head_sha": sha, "external_id": external, "status": "completed", "conclusion": "neutral", "output": map[string]string{"title": "On-demand ARM Spot runners", "summary": fmt.Sprintf("%d reservations; at most %d runners across all repositories.", len(all), spot.HardLimit), "text": string(text)}}
	return f.Control.QueueRequest(ctx, method, path, body, nil)
}

func (f *Fleet) Heartbeat(ctx context.Context) error {
	if f.HeartbeatSHA == "" {
		return errors.New("configure the Spot control heartbeat SHA")
	}
	return f.Publish(ctx, f.HeartbeatSHA, f.AppID)
}
