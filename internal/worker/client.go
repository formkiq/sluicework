// Package worker implements the outbound worker protocol.
package worker

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"sluicework/internal/state"
)

type Config struct {
	Workstation      *WorkstationConfig `json:"workstation,omitempty"`
	URL              string             `json:"url"`
	CAFile           string             `json:"caFile,omitempty"`
	CredentialFile   string             `json:"credentialFile"`
	Name             string             `json:"name"`
	Version          string             `json:"version"`
	WorkDir          string             `json:"workDir"`
	Roles            []string           `json:"roles"`
	RunnerService    string             `json:"runnerService,omitempty"`
	RunnerStatusFile string             `json:"runnerStatusFile,omitempty"`
	Slots            int                `json:"slots,omitempty"`
	InputRoot        string             `json:"inputRoot,omitempty"`
}

type credential struct {
	state.Enrollment
	URL string `json:"url"`
}

type client struct {
	config     Config
	url        string
	transport  *http.Client
	credential string
}

func newClient(cfg Config) (*client, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.EscapedPath() != "" && u.EscapedPath() != "/") {
		return nil, errors.New("worker URL must be a server origin")
	}
	host := u.Hostname()
	loopback := net.ParseIP(host).IsLoopback() || strings.EqualFold(host, "localhost")
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return nil, errors.New("HTTPS is required for non-loopback orchestrators")
	}
	if cfg.CredentialFile == "" {
		return nil, errors.New("configure a worker credential file")
	}
	var roots *x509.CertPool
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("invalid cluster CA file")
		}
	}
	transport := &http.Transport{
		DialContext:            (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		IdleConnTimeout:        60 * time.Second,
		TLSHandshakeTimeout:    5 * time.Second,
		MaxResponseHeaderBytes: 32 << 10,
	}
	u.Path = ""
	return &client{config: cfg, url: u.String(), transport: &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

type statusError int

func (e statusError) Error() string { return fmt.Sprintf("orchestrator returned HTTP %d", e) }
func unauthorized(err error) bool {
	return errors.Is(err, statusError(http.StatusUnauthorized)) || errors.Is(err, statusError(http.StatusForbidden))
}

// Permanent responses can never succeed by resending the same payload.
func permanent(err error) bool {
	var code statusError
	return errors.As(err, &code) && code >= 300 && code < 500 && code != http.StatusRequestTimeout && code != http.StatusTooManyRequests
}

func (c *client) post(ctx context.Context, path string, payload, out any) (int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/api/v1/worker/"+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.credential != "" {
		req.Header.Set("Authorization", "Bearer "+c.credential)
	}
	resp, err := c.transport.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code := statusError(resp.StatusCode)
		// Only the authenticated orchestrator's broker returns operator-facing
		// provider diagnostics. Proxies and non-JSON error pages are omitted.
		if strings.HasSuffix(path, "/infer") && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			var diagnosis struct {
				Error string `json:"error"`
			}
			if json.NewDecoder(io.LimitReader(resp.Body, 1024)).Decode(&diagnosis) == nil && diagnosis.Error != "" && len(diagnosis.Error) <= 512 {
				return resp.StatusCode, fmt.Errorf("%w: %s", code, diagnosis.Error)
			}
		}
		return resp.StatusCode, code
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 26<<20)).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	} else {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	}
	return resp.StatusCode, nil
}

func (c *client) short(ctx context.Context, path string, payload, out any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.post(ctx, path, payload, out)
}

func (c *client) info() state.WorkerInfo {
	roles := c.config.Roles
	if roles == nil {
		roles = []string{"script", "agent"}
	}
	name := c.config.Name
	if name == "" {
		name, _ = os.Hostname()
	}
	version := c.config.Version
	if version == "" {
		version = "dev"
	}
	return state.WorkerInfo{Name: name, Version: version, Roles: roles, OS: runtime.GOOS, Arch: runtime.GOARCH}
}

// Join consumes one enrollment token. An existing credential cannot be overwritten.
func Join(ctx context.Context, cfg Config, token string) error {
	if strings.TrimSpace(token) == "" {
		return errors.New("enrollment token is required")
	}
	c, err := newClient(cfg)
	if err != nil {
		return err
	}
	defer c.transport.CloseIdleConnections()
	if err := os.MkdirAll(filepath.Dir(cfg.CredentialFile), 0700); err != nil {
		return err
	}
	if _, err := os.Lstat(cfg.CredentialFile); !os.IsNotExist(err) {
		return errors.New("worker credential file already exists or is inaccessible")
	}
	file, err := os.CreateTemp(filepath.Dir(cfg.CredentialFile), ".enrolling-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	var saved credential
	_, err = c.short(ctx, "enroll", map[string]any{"token": token, "info": c.info()}, &saved.Enrollment)
	if err != nil {
		return err
	}
	if saved.ID == "" || saved.Credential == "" {
		return errors.New("incomplete enrollment response")
	}
	saved.URL = c.url
	if err := json.NewEncoder(file).Encode(saved); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Link(file.Name(), cfg.CredentialFile)
}

func (c *client) authenticate() error {
	f, err := os.Open(c.config.CredentialFile)
	if err != nil {
		return err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	if !stat.Mode().IsRegular() || stat.Mode().Perm()&0077 != 0 {
		return errors.New("worker credentials must be a private regular file (0600)")
	}
	var saved credential
	if err := json.NewDecoder(io.LimitReader(f, 64<<10)).Decode(&saved); err != nil {
		return errors.New("invalid worker credential file")
	}
	if saved.URL != c.url || saved.Credential == "" || saved.ID == "" {
		return errors.New("worker credential is invalid for the configured origin")
	}
	c.credential = saved.Credential
	return nil
}
