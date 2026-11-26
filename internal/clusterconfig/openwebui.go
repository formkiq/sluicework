package clusterconfig

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"sluicework/internal/state"
)

type OpenWebUI struct {
	Enabled       bool   `json:"enabled"`
	URL           string `json:"url"`
	OllamaBaseURL string `json:"ollamaBaseUrl,omitempty"`
}

type OpenWebUIInput struct {
	Enabled       bool   `json:"enabled"`
	URL           string `json:"url"`
	OllamaBaseURL string `json:"ollamaBaseUrl,omitempty"`
}

type OpenWebUIView struct {
	Enabled       bool   `json:"enabled"`
	URL           string `json:"url"`
	OllamaBaseURL string `json:"ollamaBaseUrl,omitempty"`
}

type OpenWebUIStatus struct {
	Configured    bool   `json:"configured"`
	Enabled       bool   `json:"enabled"`
	URL           string `json:"url,omitempty"`
	OllamaBaseURL string `json:"ollamaBaseUrl,omitempty"`
	Healthy       bool   `json:"healthy"`
	Error         string `json:"error,omitempty"`
}

func loopbackOriginHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}

func normalizeOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("%w: endpoint URL is required", state.ErrInvalid)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: use an HTTP(S) origin without credentials, paths or query parameters", state.ErrInvalid)
	}
	if u.Scheme == "http" && !loopbackOriginHost(u.Hostname()) {
		return "", fmt.Errorf("%w: remote Open WebUI and Ollama endpoints require HTTPS", state.ErrInvalid)
	}
	u.Path = ""
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), nil
}

func dialLocalLAN(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	for _, raw := range ips {
		ip := raw.Unmap()
		if !(ip.IsLoopback() || ip.IsPrivate()) || (strings.EqualFold(host, "localhost") && !ip.IsLoopback()) {
			continue
		}
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, errors.New("no reachable local or private address")
}

// ProbeOpenWebUI checks Open WebUI's /health endpoint using private-address,
// TLS 1.2+ and no-redirect rules without sending user credentials.
func ProbeOpenWebUI(ctx context.Context, endpoint, caFile string) error {
	origin, err := normalizeOrigin(endpoint)
	if err != nil {
		return err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = dialLocalLAN
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pemData, err := os.ReadFile(caFile)
		if err != nil {
			return err
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			return err
		}
		if !roots.AppendCertsFromPEM(pemData) {
			return errors.New("invalid CA certificate")
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	client := &http.Client{
		Transport:     transport,
		Timeout:       3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()

	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, origin+"/health", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return errors.New("cannot reach Open WebUI; check that the service is running, its URL and TLS")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("Open WebUI health check returned HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return errors.New("Open WebUI health response interrupted")
	}
	var payload struct {
		Status bool `json:"status"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || !payload.Status {
		return errors.New("endpoint did not return an Open WebUI health response")
	}
	return nil
}

func (m *Manager) SetOpenWebUI(actor string, in OpenWebUIInput) error {
	m.write.Lock()
	defer m.write.Unlock()
	cfg := m.current()
	if !in.Enabled && strings.TrimSpace(in.URL) == "" {
		cfg.OpenWebUI = nil
		return m.store(actor, cfg)
	}
	origin, err := normalizeOrigin(in.URL)
	if err != nil {
		return err
	}
	ollamaOrigin := ""
	if strings.TrimSpace(in.OllamaBaseURL) != "" {
		ollamaOrigin, err = normalizeOrigin(in.OllamaBaseURL)
		if err != nil {
			return err
		}
	}
	cfg.OpenWebUI = &OpenWebUI{
		Enabled:       in.Enabled,
		URL:           origin,
		OllamaBaseURL: ollamaOrigin,
	}
	return m.store(actor, cfg)
}

// EnsureOpenWebUI registers an installer-configured Open WebUI endpoint without
// overwriting an existing configuration or re-enabling a disabled one.
func (m *Manager) EnsureOpenWebUI(actor, endpoint, ollamaURL string) (OpenWebUIView, error) {
	origin, err := normalizeOrigin(endpoint)
	if err != nil {
		return OpenWebUIView{}, err
	}
	ollamaOrigin := ""
	if strings.TrimSpace(ollamaURL) != "" {
		ollamaOrigin, err = normalizeOrigin(ollamaURL)
		if err != nil {
			return OpenWebUIView{}, err
		}
	}
	m.write.Lock()
	defer m.write.Unlock()
	cfg := m.current()
	if cfg.OpenWebUI != nil {
		return OpenWebUIView{
			Enabled:       cfg.OpenWebUI.Enabled,
			URL:           cfg.OpenWebUI.URL,
			OllamaBaseURL: cfg.OpenWebUI.OllamaBaseURL,
		}, nil
	}
	cfg.OpenWebUI = &OpenWebUI{
		Enabled:       true,
		URL:           origin,
		OllamaBaseURL: ollamaOrigin,
	}
	if err := m.store(actor, cfg); err != nil {
		return OpenWebUIView{}, err
	}
	return OpenWebUIView{
		Enabled:       cfg.OpenWebUI.Enabled,
		URL:           cfg.OpenWebUI.URL,
		OllamaBaseURL: cfg.OpenWebUI.OllamaBaseURL,
	}, nil
}

func (m *Manager) OpenWebUIStatus(ctx context.Context) OpenWebUIStatus {
	cfg := m.current()
	if cfg.OpenWebUI == nil {
		return OpenWebUIStatus{}
	}
	status := OpenWebUIStatus{
		Configured:    true,
		Enabled:       cfg.OpenWebUI.Enabled,
		URL:           cfg.OpenWebUI.URL,
		OllamaBaseURL: cfg.OpenWebUI.OllamaBaseURL,
	}
	if !cfg.OpenWebUI.Enabled {
		return status
	}
	if err := ProbeOpenWebUI(ctx, cfg.OpenWebUI.URL, ""); err != nil {
		status.Error = err.Error()
		return status
	}
	status.Healthy = true
	return status
}
