package codexclient

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	Bundle Bundle
	HTTP   *http.Client
}

func New(b Bundle, trustFingerprint string) (*Client, error) {
	if e := b.Validate(); e != nil {
		return nil, e
	}
	roots, e := x509.SystemCertPool()
	if e != nil {
		return nil, e
	}
	if b.CAPEM != "" {
		fingerprint, e := Fingerprint(b.CAPEM)
		if e != nil {
			return nil, e
		}
		if !strings.EqualFold(strings.ReplaceAll(trustFingerprint, ":", ""), fingerprint) {
			return nil, fmt.Errorf("verify the public CA fingerprint against the trusted Sluicework admin, then use --trust-ca %s", fingerprint)
		}
		if !roots.AppendCertsFromPEM([]byte(b.CAPEM)) {
			return nil, errors.New("invalid cluster CA")
		}
	} else if trustFingerprint != "" {
		return nil, errors.New("client file contains no private cluster CA")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	return &Client{Bundle: b, HTTP: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *Client) Models(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, "GET", c.Bundle.ServerURL+"/api/v1/harness/v1/models", nil)
	if e != nil {
		return nil, e
	}
	req.Header.Set("Authorization", "Bearer "+c.Bundle.Token)
	res, e := c.HTTP.Do(req)
	if e != nil {
		return nil, errors.New("cannot reach the Sluicework gateway; check its HTTPS URL, network and CA trust")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("Sluicework gateway returned HTTP %d; check client expiry, revocation and gateway availability", res.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&body) != nil {
		return nil, errors.New("invalid gateway model inventory")
	}
	out := []string{}
	for _, m := range body.Data {
		out = append(out, m.ID)
	}
	return out, nil
}
func (c *Client) Relay(localToken string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, e := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(host)
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if e != nil || ip == nil || !ip.IsLoopback() || r.Header.Get("Origin") != "" || subtle.ConstantTimeCompare([]byte(auth), []byte(localToken)) != 1 {
			http.Error(w, "Unauthorized", 401)
			return
		}
		if !((r.Method == "GET" && r.URL.Path == "/v1/models") || (r.Method == "POST" && r.URL.Path == "/v1/responses")) || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		if r.ContentLength > 2<<20 {
			http.Error(w, "Request too large", 413)
			return
		}
		body := http.MaxBytesReader(w, r.Body, 2<<20)
		req, e := http.NewRequestWithContext(r.Context(), r.Method, c.Bundle.ServerURL+"/api/v1/harness"+r.URL.Path, body)
		if e != nil {
			http.Error(w, "Invalid request", 400)
			return
		}
		req.Header.Set("Authorization", "Bearer "+c.Bundle.Token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream, application/json")
		req.Header.Set("User-Agent", "Sluicework Codex client")
		res, e := c.HTTP.Do(req)
		if e != nil {
			http.Error(w, "Sluicework gateway connection interrupted", 502)
			return
		}
		defer res.Body.Close()
		if res.StatusCode >= 300 && res.StatusCode < 400 {
			http.Error(w, "Gateway redirects are forbidden", 502)
			return
		}
		for _, h := range []string{"Content-Type", "Cache-Control", "Retry-After"} {
			if v := res.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.WriteHeader(res.StatusCode)
		buf := make([]byte, 8192)
		reader := io.LimitReader(res.Body, (16<<20)+1)
		written := 0
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				written += n
				if written > 16<<20 {
					return
				}
				if _, e = w.Write(buf[:n]); e != nil {
					return
				}
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			if err != nil {
				return
			}
		}
	})
}

type RunOptions struct {
	Binary, Directory, Model, Sandbox, Prompt string
	Exec                                      bool
	In                                        io.Reader
	Out, Err                                  io.Writer
}

func (c *Client) Run(ctx context.Context, o RunOptions) error {
	models, e := c.Models(ctx)
	if e != nil {
		return e
	}
	if o.Model == "" {
		o.Model = c.Bundle.Models[0]
	}
	allowed := false
	for _, m := range models {
		allowed = allowed || m == o.Model
	}
	if !allowed {
		return errors.New("selected model is not permitted by this coding client")
	}
	if o.Sandbox == "" {
		o.Sandbox = "workspace-write"
	}
	if o.Sandbox != "workspace-write" && o.Sandbox != "read-only" {
		return errors.New("choose the read-only or workspace-write sandbox")
	}
	if o.Binary == "" {
		o.Binary = "codex"
	}
	path, e := exec.LookPath(o.Binary)
	if e != nil {
		return errors.New("install Codex CLI on the client machine first")
	}
	info, e := os.Stat(o.Directory)
	if e != nil || !info.IsDir() {
		return errors.New("choose an existing local project directory")
	}
	raw := make([]byte, 32)
	if _, e = rand.Read(raw); e != nil {
		return e
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		return e
	}
	defer listener.Close()
	server := &http.Server{Handler: c.Relay(token), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, BaseContext: func(net.Listener) context.Context { return runCtx }}
	defer server.Close()
	go server.Serve(listener)
	approval := "on-request"
	if o.Exec {
		approval = "never"
	}
	args := []string{"--no-daemon", "--ask-for-approval", approval}
	if o.Exec {
		args = append(args, "exec", "--ignore-user-config", "--ephemeral", "--skip-git-repo-check", "--json")
	}
	args = append(args, "-C", o.Directory, "-m", o.Model, "--sandbox", o.Sandbox)
	settings := []string{
		`model_provider="sluicework"`,
		`model_providers.sluicework.name="Sluicework"`,
		`model_providers.sluicework.base_url=` + strconv.Quote("http://"+listener.Addr().String()+"/v1"),
		`model_providers.sluicework.wire_api="responses"`,
		`model_providers.sluicework.env_key="SLUICEWORK_CODEX_SESSION_KEY"`,
		`model_providers.sluicework.requires_openai_auth=false`,
		`model_providers.sluicework.supports_websockets=false`,
		`model_providers.sluicework.supports_standalone_web_search=false`,
		`model_providers.sluicework.capabilities.remote_compaction="unsupported"`,
		`model_providers.sluicework.capabilities.external_web_access=false`,
		`model_providers.sluicework.request_max_retries=0`,
		`model_providers.sluicework.stream_max_retries=0`,
		`model_providers.sluicework.stream_idle_timeout_ms=1860000`,
		`model_context_window=` + strconv.Itoa(c.Bundle.ContextLength),
		`model_auto_compact_token_limit=` + strconv.Itoa(c.Bundle.ContextLength*3/4),
		`model_reasoning_effort="none"`,
		`features.apps=false`,
		`features.plugins=false`,
		`features.goals=false`,
		`features.multi_agent=false`,
		`web_search="disabled"`,
		`mcp_servers={}`,
		`analytics.enabled=false`,
	}
	for _, setting := range settings {
		args = append(args, "-c", setting)
	}
	if o.Prompt != "" {
		args = append(args, o.Prompt)
	}
	cmd := exec.CommandContext(runCtx, path, args...)
	cmd.Stdin = o.In
	cmd.Stdout = o.Out
	cmd.Stderr = o.Err
	// Do not attach the new CLI to an enclosing Codex app/session. Security and
	// sandbox environment variables remain intact.
	for _, env := range os.Environ() {
		k, _, _ := strings.Cut(env, "=")
		if k != "CODEX_THREAD_ID" && k != "CODEX_SESSION_ID" && k != "CODEX_TOOL_CALL_ID" && k != "SLUICEWORK_CODEX_SESSION_KEY" {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "SLUICEWORK_CODEX_SESSION_KEY="+token)
	fmt.Fprintf(o.Err, "Using Sluicework gateway %s; model %s; project tools run on this client.\n", c.Bundle.ServerURL, o.Model)
	return cmd.Run()
}
