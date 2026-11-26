package githubapp

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

type RunnerLabel struct {
	Name string `json:"name"`
	Type string `json:"type"`
}
type RunnerApplication struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Filename     string `json:"filename"`
	DownloadURL  string `json:"download_url"`
	SHA256       string `json:"sha256_checksum"`
}
type RegistrationToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (c *Client) ListRunners(ctx context.Context) ([]Runner, error) {
	result := []Runner{}
	for page := 1; page <= 20; page++ {
		var out struct {
			Total   int      `json:"total_count"`
			Runners []Runner `json:"runners"`
		}
		if err := c.repo(ctx, "GET", fmt.Sprintf("?per_page=100&page=%d", page), nil, &out); err != nil {
			return nil, err
		}
		if out.Total > 2000 {
			return nil, errors.New("too many repository runners")
		}
		result = append(result, out.Runners...)
		if len(out.Runners) < 100 || len(result) >= out.Total {
			return result, nil
		}
	}
	return nil, errors.New("runner pagination limit reached")
}

type cachedManagementToken struct {
	Value   string
	Expires time.Time
}

func (c *Client) Registration(ctx context.Context, scope string) (RegistrationToken, error) {
	var out RegistrationToken
	err := c.scopedRunner(ctx, scope, "POST", "/registration-token", nil, &out)
	if err == nil && (out.Token == "" || !out.ExpiresAt.After(time.Now())) {
		err = errors.New("invalid runner registration token")
	}
	return out, err
}
func (c *Client) Applications(ctx context.Context, scope string) ([]RunnerApplication, error) {
	var out []RunnerApplication
	err := c.scopedRunner(ctx, scope, "GET", "/downloads", nil, &out)
	return out, err
}
func (c *Client) ScopedRunner(ctx context.Context, scope string, id int64) (Runner, error) {
	var out Runner
	err := c.scopedRunner(ctx, scope, "GET", fmt.Sprintf("/%d", id), nil, &out)
	if err == nil && (out.ID != id || (out.Status != "online" && out.Status != "offline")) {
		err = errors.New("invalid runner response")
	}
	return out, err
}
func (c *Client) SetRunnerLabels(ctx context.Context, scope string, id int64, labels []string) error {
	return c.scopedRunner(ctx, scope, "PUT", fmt.Sprintf("/%d/labels", id), map[string]any{"labels": labels}, nil)
}
func (c *Client) RemoveRunner(ctx context.Context, scope string, id int64) error {
	return c.scopedRunner(ctx, scope, "DELETE", fmt.Sprintf("/%d", id), nil, nil)
}

// Optional management permissions use separate short-lived tokens. A missing
// Variables/Contents grant must not affect runner admission or Actions reads.
func (c *Client) managementToken(ctx context.Context, permission string) (string, error) {
	if permission != "variables" && permission != "contents" && permission != "organization_self_hosted_runners" {
		return "", errors.New("unsupported management permission")
	}
	if permission == "organization_self_hosted_runners" && !c.config.OrganizationRunners {
		return "", errors.New("enable organization runner management in GitHub connection settings")
	}
	c.managementMu.Lock()
	defer c.managementMu.Unlock()
	if cached := c.managementTokens[permission]; time.Until(cached.Expires) > time.Minute {
		return cached.Value, nil
	}
	jwt, err := c.jwt(time.Now())
	if err != nil {
		return "", err
	}
	apiPermission := permission
	if permission == "variables" {
		apiPermission = "actions_variables"
	}
	var out RegistrationToken
	err = c.request(ctx, "POST", fmt.Sprintf("/app/installations/%d/access_tokens", c.config.InstallationID), jwt, map[string]any{
		"repositories": []string{strings.Split(c.config.Repository, "/")[1]},
		"permissions":  map[string]string{apiPermission: map[string]string{"variables": "write", "contents": "read", "organization_self_hosted_runners": "write"}[permission], "metadata": "read"},
	}, &out)
	if err == nil && (out.Token == "" || !out.ExpiresAt.After(time.Now().Add(time.Minute))) {
		err = errors.New("invalid management token")
	}
	if err == nil {
		if c.managementTokens == nil {
			c.managementTokens = map[string]cachedManagementToken{}
		}
		c.managementTokens[permission] = cachedManagementToken{out.Token, out.ExpiresAt}
	}
	return out.Token, err
}
func (c *Client) management(ctx context.Context, permission, method, path string, body, out any) error {
	token, err := c.managementToken(ctx, permission)
	if err != nil {
		return err
	}
	return c.request(ctx, method, "/repos/"+c.config.Repository+path, token, body, out)
}

const RoutingVariable = "SLUICEWORK_ROUTING"

func (c *Client) Integration(ctx context.Context) (Integration, error) {
	var file struct{ Encoding, Content string }
	if err := c.management(ctx, "contents", "GET", "/contents/.github/sluicework.json", nil, &file); err != nil {
		return Integration{}, err
	}
	if file.Encoding != "base64" || len(file.Content) > 32768 {
		return Integration{}, errors.New("invalid workflow integration manifest")
	}
	raw, err := base64.StdEncoding.DecodeString(file.Content)
	if err != nil {
		return Integration{}, err
	}
	var out Integration
	if err = strictJSON(raw, &out); err != nil {
		return out, err
	}
	if (out.Version != 1 && out.Version != 2 && out.Version != 3) || out.Defaults.Version > out.Version || out.Variable != RoutingVariable || len(out.Platforms) == 0 || len(out.Platforms) > 5 || out.Defaults.Validate() != nil {
		return out, errors.New("unsupported workflow integration")
	}
	if out.Version == 3 && (out.Dispatcher == nil || out.Dispatcher.AppID <= 0 || !labelPattern.MatchString(out.Dispatcher.BackendID) || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(out.Dispatcher.HeartbeatSHA)) {
		return out, errors.New("configure the trusted priority dispatcher")
	}
	for _, p := range out.Platforms {
		if !validPlatform(p) {
			return out, errors.New("unsupported workflow platform")
		}
	}
	for _, selector := range out.Defaults.Selectors() {
		if !slices.Contains(out.Platforms, selector.Platform) {
			return out, errors.New("default workflow platform is not supported")
		}
	}
	if len(out.Workflow) == 0 || len(out.Workflow) > 200 || !strings.HasPrefix(out.Workflow, ".github/workflows/") || strings.Contains(out.Workflow, "..") || (!strings.HasSuffix(out.Workflow, ".yml") && !strings.HasSuffix(out.Workflow, ".yaml")) {
		return out, errors.New("invalid workflow path")
	}
	return out, nil
}
func (c *Client) Variable(ctx context.Context, name string) (string, bool, error) {
	var out struct {
		Value string `json:"value"`
	}
	err := c.management(ctx, "variables", "GET", "/actions/variables/"+url.PathEscape(name), nil, &out)
	if errors.Is(err, HTTPError(404)) {
		return "", false, nil
	}
	return out.Value, err == nil, err
}
func (c *Client) SaveRouting(ctx context.Context, raw string, exists bool) error {
	method, path := "POST", "/actions/variables"
	if exists {
		method, path = "PATCH", path+"/"+RoutingVariable
	}
	return c.management(ctx, "variables", method, path, map[string]string{"name": RoutingVariable, "value": raw}, nil)
}

// Read only the legacy keys used by the published integration.
func (c *Client) LegacyVariables(ctx context.Context) (map[string]string, error) {
	result := map[string]string{}
	// Use one token for this bounded sequence rather than four new grants.
	token, err := c.managementToken(ctx, "variables")
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"SLUICEWORK_MACHINE_LABEL", "SLUICEWORK_LOCAL_PLATFORM", "SLUICEWORK_LOCAL_QUEUE_MINUTES"} {
		var out struct{ Value string }
		err = c.request(ctx, "GET", "/repos/"+c.config.Repository+"/actions/variables/"+name, token, nil, &out)
		if errors.Is(err, HTTPError(404)) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result[name] = out.Value
	}
	return result, nil
}
