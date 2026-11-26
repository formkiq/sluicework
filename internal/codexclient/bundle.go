// Package codexclient connects a local Codex process to a remote Sluicework
// gateway through an ephemeral, authenticated loopback relay. The remote key
// stays in the launcher and never enters the Codex process environment.
package codexclient

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
)

type Bundle struct {
	Version         int      `json:"version"`
	ServerURL       string   `json:"serverUrl"`
	Token           string   `json:"token"`
	ClientID        string   `json:"clientId"`
	Name            string   `json:"name"`
	Models          []string `json:"models"`
	ContextLength   int      `json:"contextLength"`
	MaxOutputTokens int      `json:"maxOutputTokens"`
	Expires         string   `json:"expires"`
	CAPEM           string   `json:"caPem,omitempty"`
}

func Origin(value string) (string, error) {
	u, e := url.Parse(strings.TrimSpace(value))
	if e != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("use the Sluicework server's HTTPS origin, without credentials or paths")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || !(strings.EqualFold(u.Hostname(), "localhost") || (ip != nil && ip.IsLoopback())) {
			return "", errors.New("remote Sluicework servers require HTTPS")
		}
	}
	if u.Port() != "" {
		if _, e = net.LookupPort("tcp", u.Port()); e != nil {
			return "", errors.New("invalid server port")
		}
	}
	u.Path = ""
	return u.String(), nil
}
func Fingerprint(ca string) (string, error) {
	block, rest := pem.Decode([]byte(ca))
	if block == nil || block.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) > 0 {
		return "", errors.New("provide one public cluster CA certificate")
	}
	cert, e := x509.ParseCertificate(block.Bytes)
	if e != nil || !cert.IsCA {
		return "", errors.New("invalid cluster CA certificate")
	}
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:]), nil
}
func (b Bundle) Validate() error {
	origin, e := Origin(b.ServerURL)
	if e != nil {
		return e
	}
	if origin != b.ServerURL {
		return errors.New("client file server URL must be a normalized origin")
	}
	if b.Version != 1 || len(b.Token) != 47 || !strings.HasPrefix(b.Token, "swc_") || len(b.Models) < 1 || b.ContextLength < 4096 || b.ContextLength > 32768 || b.MaxOutputTokens < 128 || b.MaxOutputTokens > 8192 {
		return errors.New("invalid coding client file")
	}
	if b.CAPEM != "" {
		_, e = Fingerprint(b.CAPEM)
	}
	return e
}
func Load(path string) (Bundle, error) {
	var b Bundle
	info, e := os.Lstat(path)
	if e != nil {
		return b, e
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return b, fmt.Errorf("client credentials must be a private regular file; run chmod 600 %q", path)
	}
	if info.Size() > 64<<10 {
		return b, errors.New("client file is too large")
	}
	data, e := os.ReadFile(path)
	if e != nil {
		return b, e
	}
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if e = d.Decode(&b); e != nil {
		return b, errors.New("invalid coding client JSON")
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return b, errors.New("client file must contain one JSON object")
	}
	return b, b.Validate()
}
