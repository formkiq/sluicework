package githubapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sluicework/internal/spot"
	"sluicework/internal/state"
	"time"
)

type SpotHealth struct {
	Version     int                     `json:"version"`
	BackendID   string                  `json:"backendId"`
	Provisioned bool                    `json:"provisioned"`
	At          string                  `json:"at"`
	Region      string                  `json:"region"`
	HardLimit   int                     `json:"hardLimit"`
	Profiles    []spot.Profile          `json:"profiles"`
	Allocations []spot.PublicAllocation `json:"allocations"`
}
type SpotState struct {
	Repository   string         `json:"repository"`
	Repositories []string       `json:"repositories"`
	Settings     spot.Settings  `json:"settings"`
	Profiles     []spot.Profile `json:"profiles"`
	Revision     string         `json:"revision"`
	Ready        bool           `json:"ready"`
	Issue        string         `json:"issue,omitempty"`
	Health       *SpotHealth    `json:"health,omitempty"`
}
type spotManagement interface {
	SpotHealth(context.Context, DispatcherBinding) (*SpotHealth, error)
	SaveSpot(context.Context, string, bool) error
}

func (c *Client) SaveSpot(ctx context.Context, raw string, exists bool) error {
	method, path := "POST", "/actions/variables"
	if exists {
		method = "PATCH"
		path += "/" + spot.Variable
	}
	return c.management(ctx, "variables", method, path, map[string]string{"name": spot.Variable, "value": raw}, nil)
}
func (c *Client) SpotHealth(ctx context.Context, binding DispatcherBinding) (*SpotHealth, error) {
	if binding.AppID != c.config.AppID {
		return nil, errors.New("Spot dispatcher belongs to another GitHub App")
	}
	var out struct {
		Total  int `json:"total_count"`
		Checks []struct {
			SHA      string `json:"head_sha"`
			External string `json:"external_id"`
			Status   string `json:"status"`
			App      struct {
				ID int64 `json:"id"`
			} `json:"app"`
			Output struct {
				Text string `json:"text"`
			} `json:"output"`
		} `json:"check_runs"`
	}
	path := "/repos/" + c.config.Repository + "/commits/" + binding.HeartbeatSHA + "/check-runs?check_name=Sluicework%20Spot%20fleet&filter=all&per_page=100"
	if e := c.QueueRequest(ctx, "GET", path, nil, &out); e != nil {
		return nil, e
	}
	if out.Total > 100 {
		return nil, errors.New("Spot health history is ambiguous")
	}
	var health *SpotHealth
	for _, check := range out.Checks {
		if check.App.ID != binding.AppID || check.SHA != binding.HeartbeatSHA || check.External != "sluicework-spot:"+binding.BackendID || check.Status != "completed" {
			continue
		}
		if health != nil {
			return nil, errors.New("Spot health identity is ambiguous")
		}
		var h SpotHealth
		if len(check.Output.Text) > 16384 || json.Unmarshal([]byte(check.Output.Text), &h) != nil || h.Version != 1 || h.BackendID != binding.BackendID || h.HardLimit != spot.HardLimit || len(h.Allocations) > spot.HardLimit {
			return nil, errors.New("invalid Spot dispatcher health")
		}
		at, e := time.Parse(time.RFC3339, h.At)
		if e != nil || time.Since(at) > 3*time.Minute || time.Until(at) > time.Minute {
			return nil, errors.New("Spot dispatcher heartbeat is stale")
		}
		health = &h
	}
	return health, nil
}
func spotRevision(repository, raw string, binding DispatcherBinding, repositories []string) string {
	b, _ := json.Marshal([]any{repository, raw, binding, repositories})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (b *Broker) Spot(ctx context.Context) (SpotState, error) {
	out := SpotState{Repository: b.repository, Repositories: b.Repositories(), Profiles: spot.Profiles()}
	if b.repository != b.PrimaryRepository() {
		return out, fmt.Errorf("%w: use the primary connection for the shared Spot fleet", state.ErrInvalid)
	}
	if e := b.activeManagement(); e != nil {
		return out, e
	}
	integration, e := b.management.Integration(ctx)
	if e != nil {
		return out, e
	}
	if integration.Version != 3 || integration.Dispatcher == nil {
		return out, fmt.Errorf("%w: configure the priority dispatcher integration first", state.ErrInvalid)
	}
	raw, exists, e := b.management.Variable(ctx, spot.Variable)
	if e != nil {
		return out, e
	}
	out.Settings = spot.Defaults(integration.Dispatcher.BackendID)
	if exists {
		out.Settings, e = spot.ParseSettings(raw, integration.Dispatcher.BackendID, out.Repositories)
		if e != nil {
			return out, fmt.Errorf("%w: %v", state.ErrInvalid, e)
		}
	}
	out.Revision = spotRevision(b.repository, raw, *integration.Dispatcher, out.Repositories)
	client, ok := b.management.(spotManagement)
	if !ok {
		return out, errors.New("Spot management is unavailable")
	}
	health, e := client.SpotHealth(ctx, *integration.Dispatcher)
	if e != nil {
		out.Issue = "Spot dispatcher health could not be verified: " + e.Error()
		return out, nil
	}
	out.Health = health
	out.Ready = health != nil && health.Provisioned
	if !out.Ready {
		out.Issue = "Provision the optional AWS Spot infrastructure and dedicated private runner group before enabling runners."
	}
	return out, nil
}
func (b *Broker) PublishSpot(ctx context.Context, actor string, settings spot.Settings, revision string) (SpotState, error) {
	b.adminMu.Lock()
	defer b.adminMu.Unlock()
	out, e := b.Spot(ctx)
	if e != nil {
		return out, e
	}
	if e = settings.Validate(out.Settings.BackendID, out.Repositories); e != nil {
		return out, fmt.Errorf("%w: %v", state.ErrInvalid, e)
	}
	if revision == "" || revision != out.Revision {
		return out, fmt.Errorf("%w: Spot settings changed; reload before saving", state.ErrConflict)
	}
	if settings.Enabled && settings.MaxConcurrent > 0 && !out.Ready {
		return out, fmt.Errorf("%w: verify the provisioned Spot dispatcher before enabling runners", state.ErrInvalid)
	}
	raw, _ := json.Marshal(settings)
	if e = (&state.Store{DB: b.db}).Audit(actor, "publish shared AWS Spot runner settings", b.repository); e != nil {
		return out, e
	}
	_, exists, e := b.management.Variable(ctx, spot.Variable)
	if e != nil {
		return out, e
	}
	if e = b.management.(spotManagement).SaveSpot(ctx, string(raw), exists); e != nil {
		return out, e
	}
	out.Settings = settings
	// Read the immutable dispatcher binding for the resulting revision.
	integration, e := b.management.Integration(ctx)
	if e != nil {
		return out, e
	}
	out.Revision = spotRevision(b.repository, string(raw), *integration.Dispatcher, out.Repositories)
	return out, nil
}
