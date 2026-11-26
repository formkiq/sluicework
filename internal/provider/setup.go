package provider

import (
	"context"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"
)

type RuntimeInfo struct {
	Endpoint string           `json:"endpoint"`
	Version  string           `json:"version"`
	Models   []InstalledModel `json:"models"`
}

// ProbeLocal checks Ollama-compatible runtimes using exactly the inference
// client's private-address, TLS and redirect policy. No model is loaded or pulled.
func ProbeLocal(ctx context.Context, endpoint string) (RuntimeInfo, error) {
	return ProbeLocalWithCA(ctx, endpoint, "")
}

func ProbeLocalWithCA(ctx context.Context, endpoint, caFile string) (RuntimeInfo, error) {
	cfg := Config{Name: "Runtime check", Kind: "ollama", Endpoint: endpoint, Models: []string{"qwen3.5:4b"}}
	if err := normalize(&cfg); err != nil {
		return RuntimeInfo{}, err
	}
	s := &Service{local: newClient(true)}
	defer s.local.CloseIdleConnections()
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return RuntimeInfo{}, err
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			return RuntimeInfo{}, err
		}
		if !roots.AppendCertsFromPEM(pem) {
			return RuntimeInfo{}, fmt.Errorf("invalid CA certificate")
		}
		s.local.Transport.(*http.Transport).TLSClientConfig.RootCAs = roots
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c := Connection{Kind: cfg.Kind, Endpoint: cfg.Endpoint}
	models, err := s.installed(ctx, c)
	if err != nil {
		return RuntimeInfo{}, fmt.Errorf("cannot reach local inference runtime: %w", err)
	}
	info := RuntimeInfo{Endpoint: cfg.Endpoint, Version: "unknown", Models: models}
	var version struct {
		Version string `json:"version"`
	}
	// Routers may implement tags without exposing an Ollama runtime version.
	if s.request(ctx, c, "", http.MethodGet, "/api/version", nil, &version) == nil && version.Version != "" {
		info.Version = version.Version
	}
	return info, nil
}

// EnsureLocal registers an installer-selected route without replacing an
// existing connection's models, credentials, disabled state or resource settings.
func (s *Service) EnsureLocal(ctx context.Context, cfg Config) (Connection, error) {
	if cfg.Kind != "ollama" {
		return Connection{}, fmt.Errorf("%w: installer connections must be local", ErrInvalid)
	}
	if err := normalize(&cfg); err != nil {
		return Connection{}, err
	}
	connections, err := s.List(ctx)
	if err != nil {
		return Connection{}, err
	}
	for _, c := range connections {
		if c.Kind == "ollama" && c.Endpoint == cfg.Endpoint {
			return c, nil
		}
	}
	return s.Create(ctx, cfg)
}
