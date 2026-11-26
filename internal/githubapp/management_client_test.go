package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestManagementTokensAreSeparateScopedCachedAndCheckOrgRepositoryAccess(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	grants := map[string]int{}
	c := &Client{config: Config{AppID: 123, InstallationID: 456, Repository: "team/pilot", OrganizationRunners: true}, key: key}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/456/access_tokens" {
			var input struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			json.NewDecoder(r.Body).Decode(&input)
			if len(input.Repositories) != 1 || input.Repositories[0] != "pilot" || len(input.Permissions) != 2 || input.Permissions["metadata"] != "read" {
				t.Errorf("unscoped management grant: %+v", input)
			}
			permission := ""
			for p, value := range input.Permissions {
				if p != "metadata" {
					permission = p
					if p != "contents" && p != "actions_variables" && p != "organization_self_hosted_runners" {
						t.Errorf("unsupported GitHub API permission name %s", p)
						w.WriteHeader(http.StatusUnprocessableEntity)
						return
					}
					if (p == "contents" && value != "read") || (p != "contents" && value != "write") {
						t.Errorf("wrong permission %s %s", p, value)
					}
				}
			}
			grants[permission]++
			json.NewEncoder(w).Encode(map[string]any{"token": "synthetic-" + permission, "expires_at": time.Now().Add(time.Hour)})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/orgs/") {
			if r.Header.Get("Authorization") != "Bearer synthetic-organization_self_hosted_runners" {
				t.Error("organization API used another token")
			}
			switch r.URL.Path {
			case "/orgs/team/actions/runners/registration-token":
				raw, _ := io.ReadAll(r.Body)
				if r.Method != "POST" || len(raw) != 0 {
					t.Error("registration token request should have an empty body")
				}
				json.NewEncoder(w).Encode(map[string]any{"token": "synthetic-registration-token", "expires_at": time.Now().Add(time.Hour)})
			case "/orgs/team/actions/runner-groups":
				if r.URL.Query().Get("visible_to_repository") == "pilot" {
					w.Write([]byte(`{"total_count":1,"runner_groups":[{"id":1,"name":"Allowed"}]}`))
				} else {
					w.Write([]byte(`{"total_count":2,"runner_groups":[{"id":1,"name":"Allowed"},{"id":2,"name":"Other repo"}]}`))
				}
			case "/orgs/team/actions/runner-groups/1/runners":
				w.Write([]byte(`{"total_count":1,"runners":[{"id":123,"name":"linux-runner-01","os":"linux","status":"online","labels":[]}]}`))
			case "/orgs/team/actions/runner-groups/2/runners":
				w.Write([]byte(`{"total_count":1,"runners":[{"id":456,"name":"elsewhere","os":"linux","status":"online","labels":[]}]}`))
			default:
				t.Errorf("unexpected organization path %s", r.URL.Path)
				w.WriteHeader(404)
			}
			return
		}
		if strings.HasPrefix(r.URL.Path, "/repos/team/pilot/actions/variables/") {
			if r.Header.Get("Authorization") != "Bearer synthetic-actions_variables" {
				t.Error("variables used another token")
			}
			w.WriteHeader(404)
			return
		}
		if r.URL.Path == "/repos/team/pilot/contents/.github/sluicework.json" {
			if r.Header.Get("Authorization") != "Bearer synthetic-contents" {
				t.Error("contents used another token")
			}
			raw := `{"version":1,"variable":"SLUICEWORK_ROUTING","workflow":".github/workflows/main.yml","platforms":["linux-x64"],"defaults":{"version":1,"placement":"prefer-local","platform":"linux-x64","machineLabel":"linux-runner-01","queueMinutes":2}}`
			json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(raw))})
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(404)
	}))
	defer server.Close()
	c.origin = server.URL
	c.http = server.Client()
	ctx := context.Background()
	org, err := c.OrganizationInventory(ctx)
	if err != nil || len(org) != 2 || !org[0].Available || org[1].Available {
		t.Fatal("runner group access was not respected", org, err)
	}
	if token, err := c.Registration(ctx, "organization"); err != nil || token.Token == "" {
		t.Fatal("scoped registration failed", err)
	}
	if _, err = c.LegacyVariables(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Integration(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err = c.Variable(ctx, RoutingVariable); err != nil {
		t.Fatal(err)
	}
	if grants["organization_self_hosted_runners"] != 1 || grants["actions_variables"] != 1 || grants["contents"] != 1 {
		t.Fatal("management grants not cached and isolated", grants)
	}
	c.config.OrganizationRunners = false
	if _, err = c.OrganizationInventory(ctx); err == nil {
		t.Fatal("organization opt-in bypassed by cached token")
	}
}
