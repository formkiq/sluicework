package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestInstallationTokenIsSignedScopedAndCached(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "app.pem")
	if err = os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{AppID: 123, InstallationID: 456, Repository: "team/pilot", PrivateKeyFile: path})
	if err != nil {
		t.Fatal(err)
	}
	var tokens, creates, labelUpdates int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/456/access_tokens" {
			tokens++
			jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			parts := strings.Split(jwt, ".")
			if len(parts) != 3 {
				t.Error("missing JWT")
				w.WriteHeader(401)
				return
			}
			sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
			digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
				t.Error("invalid signature")
			}
			payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
			var claims struct {
				Iss      int
				Iat, Exp int64
			}
			_ = json.Unmarshal(payload, &claims)
			if claims.Iss != 123 || claims.Iat > time.Now().Unix() || claims.Exp > time.Now().Add(10*time.Minute).Unix() {
				t.Errorf("invalid JWT claims: %+v", claims)
			}
			var body struct {
				Repositories []string
				Permissions  map[string]string
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.Repositories) != 1 || body.Repositories[0] != "pilot" || body.Permissions["administration"] != "write" || len(body.Permissions) != 2 {
				t.Errorf("unscoped token request: %+v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "synthetic-installation-token", "expires_at": time.Now().Add(time.Hour)})
			return
		}
		if r.URL.Path == "/repos/team/pilot/actions/runners/42/labels" && r.Method == "PUT" {
			var body struct{ Labels []string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Labels == nil || (len(body.Labels) != 0 && (len(body.Labels) != 1 || body.Labels[0] != "sluicework-pilot")) {
				t.Errorf("expected only custom routing labels: %+v", body)
			}
			labelUpdates++
			w.Write([]byte(`{"labels":[]}`))
			return
		}
		if r.URL.Path != "/repos/team/pilot/actions/runners/generate-jitconfig" || r.Header.Get("Authorization") != "Bearer synthetic-installation-token" {
			t.Errorf("wrong request: %s", r.URL.Path)
		}
		creates++
		w.Write([]byte(`{"runner":{"id":42},"encoded_jit_config":"synthetic-jit"}`))
	}))
	defer server.Close()
	client.origin = server.URL
	for range 2 {
		grant, err := client.Create(context.Background(), "pilot", []string{"sluicework-pilot"})
		if err != nil || grant.Runner.ID != 42 || grant.JIT != "synthetic-jit" {
			t.Fatal(grant, err)
		}
	}
	if err = client.Labels(context.Background(), 42, []string{"self-hosted", "macOS", "ARM64", "sluicework-pilot"}); err != nil {
		t.Fatal(err)
	}
	if err = client.Labels(context.Background(), 42, nil); err != nil {
		t.Fatal(err)
	}
	if tokens != 1 || creates != 2 || labelUpdates != 4 {
		t.Fatal("token cache", tokens, creates)
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = New(Config{AppID: 123, InstallationID: 456, Repository: "team/pilot", PrivateKeyFile: path}); err == nil {
		t.Fatal("publicly readable key accepted")
	}
}

func TestJITRoutingLabelsCanBeWithdrawnAndRestored(t *testing.T) {
	var fixed, custom []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Labels []string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /repos/team/pilot/actions/runners/generate-jitconfig":
			fixed = slices.Clone(body.Labels)
			w.Write([]byte(`{"runner":{"id":42},"encoded_jit_config":"synthetic-jit"}`))
		case "PUT /repos/team/pilot/actions/runners/42/labels":
			// Reproduce GitHub's behavior: every JIT label is read-only and
			// cannot be reassigned by the custom-label endpoint.
			for _, label := range body.Labels {
				for _, readOnly := range fixed {
					if strings.EqualFold(label, readOnly) {
						w.WriteHeader(http.StatusUnprocessableEntity)
						return
					}
				}
			}
			custom = slices.Clone(body.Labels)
			w.Write([]byte(`{"labels":[]}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := &Client{config: Config{Repository: "team/pilot"}, http: server.Client(), origin: server.URL,
		token: "synthetic-token", expires: time.Now().Add(time.Hour)}
	labels := []string{"self-hosted", "macOS", "ARM64", "sluicework-machine", "extra-capability"}
	grant, err := client.Create(context.Background(), "pilot", labels)
	if err != nil || grant.JIT != "synthetic-jit" {
		t.Fatalf("grant withheld: %+v, %v", grant, err)
	}
	if !slices.Equal(fixed, labels[:3]) || !slices.Equal(custom, labels[3:]) {
		t.Fatalf("incorrect label types: read-only=%v custom=%v", fixed, custom)
	}
	if err = client.Labels(context.Background(), 42, nil); err != nil || len(custom) != 0 {
		t.Fatalf("pause did not remove routing labels: %v %v", custom, err)
	}
	if err = client.Labels(context.Background(), 42, labels); err != nil || !slices.Equal(custom, labels[3:]) {
		t.Fatalf("resume did not restore routing labels: %v %v", custom, err)
	}
}

func TestJITGrantWithheldAndRegistrationCleanedWhenLabelsFail(t *testing.T) {
	var deleted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "POST":
			w.Write([]byte(`{"runner":{"id":42},"encoded_jit_config":"synthetic-jit"}`))
		case "PUT":
			w.WriteHeader(http.StatusUnprocessableEntity)
		case "DELETE":
			if r.URL.Path != "/repos/team/pilot/actions/runners/42" {
				t.Error("deleted a different registration")
			}
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	client := &Client{config: Config{Repository: "team/pilot"}, http: server.Client(), origin: server.URL,
		token: "synthetic-token", expires: time.Now().Add(time.Hour)}
	grant, err := client.Create(context.Background(), "pilot", []string{"sluicework-machine"})
	if err != HTTPError(422) || grant.JIT != "" || grant.Runner.ID != 0 || !deleted {
		t.Fatalf("unroutable runner leaked: %+v, %v, deleted=%v", grant, err, deleted)
	}
}

func TestConnectionCheckUsesSeparateRunnerAndActionsPermissions(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "key.pem")
	if err = os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{AppID: 123, InstallationID: 456, Repository: "team/repo", PrivateKeyFile: path})
	if err != nil {
		t.Fatal(err)
	}
	var runnerTokens, actionTokens, reads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/456/access_tokens" {
			var body struct {
				Permissions  map[string]string
				Repositories []string
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.Repositories) != 1 || body.Repositories[0] != "repo" {
				t.Error("token escaped configured repository")
			}
			token := ""
			if body.Permissions["administration"] == "write" && body.Permissions["actions"] == "" {
				runnerTokens++
				token = "runner-read-check"
			} else if body.Permissions["actions"] == "read" && body.Permissions["administration"] == "" {
				actionTokens++
				token = "activity-read-check"
			} else {
				t.Error("unscoped token")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": token, "expires_at": time.Now().Add(time.Hour)})
			return
		}
		if r.Method != "GET" {
			t.Error("connection check mutated GitHub")
		}
		if r.URL.Path == "/repos/team/repo/actions/runners" && r.Header.Get("Authorization") == "Bearer runner-read-check" {
			reads++
			w.Write([]byte("{}"))
			return
		}
		if r.URL.Path == "/repos/team/repo/actions/runs" && r.Header.Get("Authorization") == "Bearer activity-read-check" {
			reads++
			w.Write([]byte("{}"))
			return
		}
		t.Error("unexpected check request", r.URL.Path)
		w.WriteHeader(404)
	}))
	defer server.Close()
	client.origin = server.URL
	if err = client.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runnerTokens != 1 || actionTokens != 1 || reads != 2 {
		t.Fatal("connection permissions not verified", runnerTokens, actionTokens, reads)
	}
}
