// Package githubapp brokers repository-scoped, ephemeral GitHub Actions runners.
// Only the orchestrator reads the App key; workers receive one-run credentials.
package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Config struct {
	WorkstationGroupID      int64    `json:"workstationGroupId,omitempty"`
	WorkstationRepositories []string `json:"workstationRepositories,omitempty"`
	OrganizationRunners     bool     `json:"organizationRunners,omitempty"`
	AppID                   int64    `json:"appId"`
	InstallationID          int64    `json:"installationId"`
	PrivateKeyFile          string   `json:"privateKeyFile"`
	Repository              string   `json:"repository"`
}

type Client struct {
	managementMu     sync.Mutex
	managementTokens map[string]cachedManagementToken
	config           Config
	key              *rsa.PrivateKey
	http             *http.Client
	origin           string
	mu               sync.Mutex
	token            string
	expires          time.Time
	actionsToken     string
	actionsExpires   time.Time
	actionsMu        sync.Mutex
}

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func New(cfg Config) (*Client, error) {
	if err := cfg.validateSharing(); err != nil {
		return nil, err
	}
	if cfg.AppID <= 0 || cfg.InstallationID <= 0 || !repositoryPattern.MatchString(cfg.Repository) || strings.Contains(cfg.Repository, "..") {
		return nil, errors.New("GitHub App requires appId, installationId and owner/repository")
	}
	f, err := os.OpenFile(cfg.PrivateKeyFile, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 16384 {
		return nil, errors.New("GitHub App key must be a private regular file (chmod 600)")
	}
	b, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil {
		return nil, err
	}
	return NewWithPEM(cfg, b)
}

// NewWithPEM accepts key material supplied directly by a protected secret store.
// File-based installations must use New, which enforces file permissions.
func NewWithPEM(cfg Config, b []byte) (*Client, error) {
	if err := cfg.validateSharing(); err != nil {
		return nil, err
	}
	if cfg.AppID <= 0 || cfg.InstallationID <= 0 || !repositoryPattern.MatchString(cfg.Repository) || strings.Contains(cfg.Repository, "..") || len(b) > 16384 {
		return nil, errors.New("invalid GitHub App configuration")
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("GitHub App key must be PEM RSA")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		parsed, e := x509.ParsePKCS8PrivateKey(block.Bytes)
		var ok bool
		key, ok = parsed.(*rsa.PrivateKey)
		if e != nil || !ok {
			return nil, errors.New("GitHub App key must be RSA")
		}
	}
	if key.N.BitLen() < 2048 {
		return nil, errors.New("GitHub App RSA key must be at least 2048 bits")
	}
	return &Client{config: cfg, key: key, origin: "https://api.github.com", http: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) jwt(now time.Time) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(5 * time.Minute).Unix(), "iss": c.config.AppID})
	message := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(message))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, digest[:])
	return message + "." + base64.RawURLEncoding.EncodeToString(sig), err
}

type HTTPError int

func (e HTTPError) Error() string { return fmt.Sprintf("GitHub returned HTTP %d", e) }

func (c *Client) request(ctx context.Context, method, path, token string, body, out any) error {
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.origin+path, payload)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "Sluicework")
	resp, err := c.http.Do(req)
	if err != nil {
		return errors.New("GitHub request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return responseError(resp)
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(out)
	}
	return nil
}

func (c *Client) accessToken(ctx context.Context) (string, error) {
	return c.installationToken(ctx, false)
}

func (c *Client) installationToken(ctx context.Context, actions bool) (string, error) {
	mu := &c.mu
	if actions {
		mu = &c.actionsMu
	}
	mu.Lock()
	defer mu.Unlock()
	token, expires := &c.token, &c.expires
	permissions := map[string]string{"administration": "write", "metadata": "read"}
	if actions {
		token, expires = &c.actionsToken, &c.actionsExpires
		permissions = map[string]string{"actions": "read", "metadata": "read"}
	}
	if time.Until(*expires) > time.Minute {
		return *token, nil
	}
	jwt, err := c.jwt(time.Now())
	if err != nil {
		return "", err
	}
	var out struct {
		Token     string
		ExpiresAt time.Time `json:"expires_at"`
	}
	err = c.request(ctx, "POST", fmt.Sprintf("/app/installations/%d/access_tokens", c.config.InstallationID), jwt, map[string]any{
		"repositories": []string{strings.Split(c.config.Repository, "/")[1]},
		"permissions":  permissions,
	}, &out)
	if err != nil {
		return "", err
	}
	if out.Token == "" || time.Until(out.ExpiresAt) < time.Minute {
		return "", errors.New("invalid GitHub installation token")
	}
	*token, *expires = out.Token, out.ExpiresAt
	return *token, nil
}

func (c *Client) repo(ctx context.Context, method, path string, body, out any) error {
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	return c.request(ctx, method, "/repos/"+c.config.Repository+"/actions/runners"+path, token, body, out)
}

// Check verifies repository runner access and the separate Actions-read token.
// It neither registers runners nor changes repository configuration.
func (c *Client) Check(ctx context.Context) error {
	if c.config.WorkstationGroupID > 0 {
		if err := c.checkWorkstationGroup(ctx); err != nil {
			return err
		}
	}
	if err := c.repo(ctx, "GET", "?per_page=1", nil, nil); err != nil {
		return err
	}
	token, err := c.installationToken(ctx, true)
	if err != nil {
		return err
	}
	return c.request(ctx, "GET", "/repos/"+c.config.Repository+"/actions/runs?per_page=1", token, nil, nil)
}

type Runner struct {
	// GroupName is set from trusted group inventory, never from a runner response.
	GroupName string        `json:"-"`
	ID        int64         `json:"id"`
	Name      string        `json:"name"`
	OS        string        `json:"os"`
	Labels    []RunnerLabel `json:"labels"`
	Busy      bool          `json:"busy"`
	Status    string        `json:"status"`
}
type Grant struct {
	Runner Runner `json:"runner"`
	JIT    string `json:"encoded_jit_config,omitempty"`
}

func (c *Client) Create(ctx context.Context, name string, labels []string) (Grant, error) {
	// GitHub makes every label in a JIT configuration read-only, including
	// application routing labels. Add routing labels through the custom-label
	// endpoint before handing the credentials to a listener, so pause can remove
	// them and enable can restore them.
	fixed := []string{"self-hosted"}
	seen := map[string]bool{"self-hosted": true}
	for _, label := range labels {
		lower := strings.ToLower(label)
		if platformLabel(label) && !seen[lower] {
			fixed = append(fixed, label)
			seen[lower] = true
		}
	}
	group := int64(1)
	if c.config.WorkstationGroupID > 0 {
		if err := c.checkWorkstationGroup(ctx); err != nil {
			return Grant{}, err
		}
		group = c.config.WorkstationGroupID
	}
	var out Grant
	err := c.managedRunner(ctx, "POST", "/generate-jitconfig", map[string]any{"name": name, "runner_group_id": group, "labels": fixed, "work_folder": "_work"}, &out)
	if err == nil && (out.Runner.ID <= 0 || out.JIT == "") {
		err = errors.New("incomplete GitHub runner grant")
	}
	if err == nil {
		err = c.Labels(ctx, out.Runner.ID, labels)
	}
	if err != nil {
		if out.Runner.ID > 0 {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			_ = c.Delete(cleanup, out.Runner.ID)
			cancel()
		}
		return Grant{}, err
	}
	return out, err
}

func platformLabel(label string) bool {
	switch strings.ToLower(label) {
	case "self-hosted", "linux", "macos", "windows", "arm64", "x64", "arm":
		return true
	default:
		return false
	}
}

func (c *Client) Get(ctx context.Context, id int64) (Runner, error) {
	var out Runner
	err := c.managedRunner(ctx, "GET", fmt.Sprintf("/%d", id), nil, &out)
	if err == nil && (out.ID != id || (out.Status != "online" && out.Status != "offline")) {
		err = errors.New("invalid GitHub runner status")
	}
	return out, err
}
func (c *Client) Labels(ctx context.Context, id int64, labels []string) error {
	// GitHub owns the read-only platform labels. This endpoint replaces only
	// custom routing labels; including platform labels can fail validation.
	custom := []string{}
	for _, label := range labels {
		if !platformLabel(label) {
			custom = append(custom, label)
		}
	}
	return c.managedRunner(ctx, "PUT", fmt.Sprintf("/%d/labels", id), map[string]any{"labels": custom}, nil)
}
func (c *Client) Delete(ctx context.Context, id int64) error {
	return c.managedRunner(ctx, "DELETE", fmt.Sprintf("/%d", id), nil, nil)
}
