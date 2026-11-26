package codexclient

import (
	"context"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func bundleFor(t *testing.T, server *httptest.Server) Bundle {
	t.Helper()
	return Bundle{Version: 1, ServerURL: server.URL, Token: "swc_" + strings.Repeat("a", 43), Models: []string{"qwen"}, ContextLength: 16384, MaxOutputTokens: 2048, CAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))}
}
func TestRemoteTLSRequiresMatchingCAAndScopedBearer(t *testing.T) {
	token := "swc_" + strings.Repeat("a", 43)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("missing gateway credential")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"id":"qwen"}]}`)
	}))
	defer server.Close()
	b := bundleFor(t, server)
	fp, e := Fingerprint(b.CAPEM)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = New(b, "wrong-fingerprint"); e == nil {
		t.Fatal("unverified CA accepted")
	}
	client, e := New(b, fp)
	if e != nil {
		t.Fatal(e)
	}
	models, e := client.Models(context.Background())
	if e != nil || len(models) != 1 {
		t.Fatalf("TLS connect: %v %v", models, e)
	}
	b.CAPEM = ""
	client, e = New(b, "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = client.Models(context.Background()); e == nil {
		t.Fatal("untrusted TLS certificate accepted")
	}
	if _, e = Origin("http://10.0.0.10:7443"); e == nil {
		t.Fatal("remote plaintext accepted")
	}
}
func TestRelayNeverOffersNativeRuntimeOrAnonymousAccess(t *testing.T) {
	var path string
	remote := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if r.Header.Get("Authorization") != "Bearer swc_"+strings.Repeat("a", 43) {
			t.Error("wrong remote credential")
		}
		io.WriteString(w, `{"data":[]}`)
	}))
	defer remote.Close()
	b := bundleFor(t, remote)
	fp, _ := Fingerprint(b.CAPEM)
	client, e := New(b, fp)
	if e != nil {
		t.Fatal(e)
	}
	relay := httptest.NewServer(client.Relay("ephemeral-local-token"))
	defer relay.Close()
	for _, tc := range []struct {
		path, token string
		code        int
	}{{"/v1/models", "", 401}, {"/api/tags", "ephemeral-local-token", 404}, {"/v1/models?endpoint=other", "ephemeral-local-token", 404}, {"/v1/models", "ephemeral-local-token", 200}} {
		req, _ := http.NewRequest("GET", relay.URL+tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+tc.token)
		res, e := relay.Client().Do(req)
		if e != nil {
			t.Fatal(e)
		}
		res.Body.Close()
		if res.StatusCode != tc.code {
			t.Fatalf("%s status %d", tc.path, res.StatusCode)
		}
	}
	if path != "/api/v1/harness/v1/models" {
		t.Fatal("relay selected wrong gateway path")
	}
}
func TestClientCredentialFileMustBePrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.json")
	if e := os.WriteFile(path, []byte(`{}`), 0644); e != nil {
		t.Fatal(e)
	}
	// The workstation runner inherits umask 0077; make the intentionally
	// unsafe fixture explicit instead of relying on file creation permissions.
	if e := os.Chmod(path, 0644); e != nil {
		t.Fatal(e)
	}
	if _, e := Load(path); e == nil || !strings.Contains(e.Error(), "chmod 600") {
		t.Fatal("public client file accepted")
	}
}
func TestLauncherKeepsDurableCredentialOutOfCodexEnvironment(t *testing.T) {
	remote := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"data":[{"id":"qwen"}]}`) }))
	defer remote.Close()
	b := bundleFor(t, remote)
	fp, _ := Fingerprint(b.CAPEM)
	c, e := New(b, fp)
	if e != nil {
		t.Fatal(e)
	}
	root := t.TempDir()
	binary := filepath.Join(root, "fake-codex")
	script := "#!/bin/sh\nprintf 'session=%s\\nthread=%s\\n' \"$SLUICEWORK_CODEX_SESSION_KEY\" \"${CODEX_THREAD_ID-unset}\"\nprintf '%s\\n' \"$@\"\n"
	if e = os.WriteFile(binary, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	var output, errors strings.Builder
	e = c.Run(context.Background(), RunOptions{Binary: binary, Directory: root, Model: "qwen", Sandbox: "read-only", Prompt: "inspect fixture", In: strings.NewReader(""), Out: &output, Err: &errors})
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(output.String(), "stream_idle_timeout_ms=1860000") {
		t.Fatal("launcher cannot wait for a thirty-minute request")
	}
	if strings.Contains(output.String(), b.Token) || strings.Contains(errors.String(), b.Token) {
		t.Fatal("durable gateway credential exposed to Codex")
	}
	if !strings.Contains(output.String(), "thread=unset") || !strings.Contains(output.String(), "--no-daemon") || !strings.Contains(output.String(), "model_provider=\"sluicework\"") {
		t.Fatal("launcher was not isolated from enclosing sessions")
	}
}
func TestGatewayRedirectNeverForwardsCredential(t *testing.T) {
	hits := 0
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer other.Close()
	remote := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, 302) }))
	defer remote.Close()
	b := bundleFor(t, remote)
	fp, _ := Fingerprint(b.CAPEM)
	c, e := New(b, fp)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.Models(context.Background()); e == nil || hits != 0 {
		t.Fatal("gateway redirect followed")
	}
}
