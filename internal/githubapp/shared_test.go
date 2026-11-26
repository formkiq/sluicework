package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sluicework/internal/state"
	"strings"
	"testing"
	"time"
)

func TestSharedWorkstationConfigurationRequiresExactSameOrganizationScope(t *testing.T) {
	good := Config{Repository: "team/primary", OrganizationRunners: true, WorkstationGroupID: 7, WorkstationRepositories: []string{"team/primary", "team/secondary"}}
	if err := good.validateSharing(); err != nil {
		t.Fatal(err)
	}
	for _, patch := range []func(*Config){func(c *Config) { c.OrganizationRunners = false }, func(c *Config) { c.WorkstationGroupID = 0 }, func(c *Config) { c.WorkstationRepositories = []string{"team/secondary"} }, func(c *Config) { c.WorkstationRepositories = []string{"team/primary", "other/private"} }, func(c *Config) { c.WorkstationRepositories = []string{"team/primary", "TEAM/PRIMARY"} }} {
		c := good
		patch(&c)
		if c.validateSharing() == nil {
			t.Fatal("unsafe shared scope accepted", c.WorkstationRepositories)
		}
	}
}
func TestSharedWorkstationUsesRestrictedOrganizationGroupAndOneJITRegistration(t *testing.T) {
	broad := false
	different := false
	creates := 0
	c := &Client{config: Config{Repository: "team/primary", OrganizationRunners: true, WorkstationGroupID: 7, WorkstationRepositories: []string{"team/primary", "team/secondary"}}, managementTokens: map[string]cachedManagementToken{"organization_self_hosted_runners": {Value: "synthetic-org", Expires: time.Now().Add(time.Hour)}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-org" || strings.HasPrefix(r.URL.Path, "/repos/") {
			t.Error("shared registration used repository runner access")
		}
		switch r.URL.Path {
		case "/orgs/team/actions/runner-groups/7":
			visibility := "selected"
			if broad {
				visibility = "all"
			}
			json.NewEncoder(w).Encode(map[string]any{"id": 7, "visibility": visibility})
		case "/orgs/team/actions/runner-groups/7/repositories":
			other := "team/secondary"
			if different {
				other = "team/unapproved"
			}
			json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "repositories": []map[string]any{{"full_name": "team/primary", "private": true}, {"full_name": other, "private": true}}})
		case "/orgs/team/actions/runners/generate-jitconfig":
			creates++
			var body struct {
				Group  int64    `json:"runner_group_id"`
				Labels []string `json:"labels"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.Group != 7 || !slices.Equal(body.Labels, []string{"self-hosted", "macOS", "ARM64"}) {
				t.Error("wrong JIT group/platform", body)
			}
			w.Write([]byte(`{"runner":{"id":77},"encoded_jit_config":"synthetic-jit"}`))
		case "/orgs/team/actions/runners/77/labels":
			var body struct {
				Labels []string `json:"labels"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if !slices.Equal(body.Labels, []string{"sluicework-mac"}) {
				t.Error("routing label missing", body)
			}
			w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected API %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c.origin = server.URL
	c.http = server.Client()
	grant, err := c.Create(context.Background(), "sluicework-example", []string{"self-hosted", "macOS", "ARM64", "sluicework-mac"})
	if err != nil || grant.Runner.ID != 77 || creates != 1 {
		t.Fatal("shared JIT failed", err)
	}
	broad = true
	if _, err = c.Create(context.Background(), "mac", nil); err == nil || creates != 1 {
		t.Fatal("broad group produced a grant", err)
	}
	broad = false
	different = true
	if _, err = c.Create(context.Background(), "mac", nil); err == nil || creates != 1 {
		t.Fatal("unapproved repository produced a grant", err)
	}
}
func TestRepositorySelectionIsAllowlistedAndRetiredWithItsConnection(t *testing.T) {
	b, _, _ := managementFixture(t)
	if _, err := b.db.Exec(activitySchema); err != nil {
		t.Fatal(err)
	}
	child := &Broker{db: b.db, repository: "team/secondary", primaryRepository: b.repository}
	b.children = map[string]*Broker{"team/secondary": child}
	if selected, err := b.ForRepository("TEAM/SECONDARY"); err != nil || selected != child || selected.machineScope() != b.repository {
		t.Fatal("shared context missing", err)
	}
	if _, err := b.ForRepository("other/private"); !errors.Is(err, state.ErrInvalid) {
		t.Fatal("arbitrary repository accepted", err)
	}
	for _, repo := range []string{b.repository, child.repository, "other/private"} {
		raw, _ := json.Marshal(Build{Repository: repo, JobID: 1, RunID: 2, MachineID: "mac", Status: "completed"})
		_, err := b.db.Exec("INSERT INTO github_builds(repository,job,run,machine,completed,started,data) VALUES(?,1,2,'mac',1,0,?)", repo, string(raw))
		if err != nil {
			t.Fatal(err)
		}
	}
	activity, err := b.Activity(context.Background())
	if err != nil || len(activity.Builds) != 2 {
		t.Fatal("wrong repository activity", activity, err)
	}
	if err = b.Replace(false, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = b.ForRepository(child.repository); !errors.Is(err, state.ErrConflict) {
		t.Fatal("retired primary reused", err)
	}
	if err = child.activeManagement(); !errors.Is(err, state.ErrConflict) {
		t.Fatal("child survived connection replacement", err)
	}
}
