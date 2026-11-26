package api_test

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"runtime"
	"testing"

	"sluicework/internal/clusterconfig"
	"sluicework/internal/state"
)

func TestSetupIsCentralAdminOnlyAndWorkerDrainIsSelfScoped(t *testing.T) {
	c := setup(t)
	manager, err := clusterconfig.Open(c.api.State.DB, clusterconfig.Seed{DataDir: t.TempDir(), Listen: "127.0.0.1:7443"})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	c.api.Configuration = manager
	c.api.State.LogLimitPolicy = manager.LogLimit
	if err = manager.StartNetwork(c.api.Handler(http.NotFoundHandler())); err != nil {
		t.Fatal(err)
	}
	worker := c.enroll("first")
	other := c.enroll("second")
	for _, path := range []string{"/settings", "/settings/worker-bundle?name=Mac&url=https%3A%2F%2Fcluster.local%3A8443&os=darwin&arch=arm64"} {
		c.do("GET", path, nil, c.operator, 403, nil)
		c.do("GET", path, nil, worker, 401, nil)
	}
	for _, item := range []struct{ method, path string }{{"PUT", "/settings/log-limit"}, {"PUT", "/settings/network"}, {"PUT", "/settings/github"}, {"POST", "/settings/github/test"}, {"PUT", "/settings/open-webui"}, {"POST", "/settings/open-webui/test"}} {
		c.do(item.method, item.path, map[string]any{}, c.operator, 403, nil)
		c.do(item.method, item.path, map[string]any{}, worker, 401, nil)
	}
	var view clusterconfig.View
	c.do("GET", "/settings", nil, c.admin, 200, &view)
	c.do("PUT", "/settings/log-limit", map[string]int{"bytes": 2 << 20}, c.admin, 200, &view)
	if view.LogLimit != 2<<20 || c.api.State.LogLimitPolicy() != 2<<20 {
		t.Fatal("output policy did not apply")
	}
	c.do("PUT", "/settings/log-limit", map[string]int{"bytes": 0}, c.admin, 400, nil)
	c.do("PUT", "/settings/network", map[string]any{"enabled": true, "hostname": "https://bad/path", "port": 8443}, c.admin, 400, nil)
	c.do("PUT", "/settings/github", map[string]any{"enabled": true, "appId": 1, "installationId": 2, "repository": "team/repo", "privateKeyPem": "not PEM"}, c.admin, 400, nil)
	// A different-platform archive includes only bootstrap/helper; no server key.
	arch := "amd64"
	if runtime.GOARCH == arch {
		arch = "arm64"
	}
	data := c.do("GET", "/settings/worker-bundle?name=Second&url=https%3A%2F%2Fcluster.local%3A8443&os=darwin&arch="+arch, nil, c.admin, 200, nil)
	if _, err = zip.NewReader(bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal("invalid setup download", err)
	}
	var first, second state.Machine
	c.do("GET", "/worker/status", nil, worker, 200, &first)
	c.do("GET", "/worker/status", nil, other, 200, &second)
	if first.ID == second.ID {
		t.Fatal("worker identity scope lost")
	}
	c.do("POST", "/worker/drain", map[string]any{}, worker, 200, &first)
	c.do("GET", "/worker/status", nil, other, 200, &second)
	if !first.Draining || second.Draining {
		t.Fatal("worker drain escaped its own identity")
	}
}

func TestGitHubSetupMasksKeysAndBlocksUnsafeScopeChanges(t *testing.T) {
	c := setup(t)
	manager, err := clusterconfig.Open(c.api.State.DB, clusterconfig.Seed{DataDir: t.TempDir(), Listen: "127.0.0.1:7443"})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	c.api.Configuration = manager
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	secret := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	input := clusterconfig.GitHubInput{Enabled: true, AppID: 123, InstallationID: 456, Repository: "team/repo", PrivateKeyPEM: secret}
	var view clusterconfig.View
	raw := c.do("PUT", "/settings/github", input, c.admin, 200, &view)
	if view.GitHub == nil || !view.GitHub.KeyConfigured || c.api.Broker() == nil || bytes.Contains(raw, []byte("PRIVATE KEY")) || bytes.Contains(raw, []byte("privateKeyFile")) {
		t.Fatal("connection setup failed or exposed a key")
	}
	if _, err = c.api.State.DB.Exec("INSERT INTO github_runner_leases(machine,runner,repository,issued) VALUES('fixture',42,'team/repo',0)"); err != nil {
		t.Fatal(err)
	}
	input.AppID = 999
	input.PrivateKeyPEM = ""
	c.do("PUT", "/settings/github", input, c.admin, 409, nil)
	c.do("GET", "/settings", nil, c.admin, 200, &view)
	if view.GitHub.AppID != 123 {
		t.Fatal("unsafe App change was persisted")
	}
	input.AppID = 123
	input.PrivateKeyPEM = secret
	c.do("PUT", "/settings/github", input, c.admin, 200, &view)
	if view.GitHub.AppID != 123 {
		t.Fatal("same-App key rotation was rejected")
	}
}
