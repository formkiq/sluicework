package api_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sluicework/internal/githubapp"
)

func TestGitHubActivityRequiresConsoleSessionAndHandlesUnconfiguredApp(t *testing.T) {
	c := setup(t)
	worker := c.enroll("worker")
	c.do("GET", "/github/activity", nil, identity{}, 401, nil)
	c.do("GET", "/github/activity", nil, worker, 401, nil)
	for _, id := range []identity{c.admin, c.operator} {
		var activity githubapp.Activity
		body := c.do("GET", "/github/activity", nil, id, 200, &activity)
		if activity.Configured || activity.Builds == nil || !strings.Contains(string(body), `"builds":[]`) {
			t.Fatalf("unconfigured activity is not explicit: %s", body)
		}
	}
}

func TestGitHubActivityReadsOnlyConfiguredRepositoryCache(t *testing.T) {
	c := setup(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "synthetic-app.pem")
	if err = os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}
	c.api.GitHub, err = githubapp.Open(c.api.State.DB, githubapp.Config{AppID: 123, InstallationID: 456, PrivateKeyFile: path, Repository: "team/pilot"})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/github/runners?repository=another/private-repo", "/github/routing?repository=another/private-repo"} {
		c.do("GET", path, nil, c.admin, 400, nil)
	}
	for _, repository := range []string{"team/pilot", "another/private-repo"} {
		build := githubapp.Build{Repository: repository, JobID: 200, RunID: 100, MachineID: "machine", Status: "in_progress", URL: "https://github.com/" + repository + "/actions/runs/100/job/200"}
		raw, _ := json.Marshal(build)
		if _, err = c.api.State.DB.Exec("INSERT INTO github_builds(repository,job,run,machine,completed,started,data) VALUES(?,200,100,'machine',0,0,?)", repository, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []identity{c.admin, c.operator} {
		var activity githubapp.Activity
		c.do("GET", "/github/activity", nil, id, 200, &activity)
		if !activity.Configured || activity.Repository != "team/pilot" || len(activity.Builds) != 1 || activity.Builds[0].Repository != "team/pilot" {
			t.Fatalf("repository cache escaped: %+v", activity)
		}
	}
	if _, err = c.api.GitHub.Activity(context.Background()); err != nil {
		t.Fatal(err)
	}
}
