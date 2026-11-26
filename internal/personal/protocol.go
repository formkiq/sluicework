// Package personal separates a user's subscription runtime from the shared controller.
package personal

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"sluicework/internal/provider"
)

var ErrDenied = errors.New("personal agent permission denied")
var ErrInvalid = errors.New("invalid personal agent request")

// A proposal is inert. Only a private local approval can authorize inference.
type Manifest struct {
	Name                 string              `json:"name"`
	Prompt               string              `json:"prompt"`
	Model                string              `json:"model"`
	Cron                 string              `json:"cron"`
	Timezone             string              `json:"timezone"`
	Expires              time.Time           `json:"expires"`
	MaxRunsPerDay        int                 `json:"maxRunsPerDay"`
	TimeoutSeconds       int                 `json:"timeoutSeconds"`
	OutputLimit          int                 `json:"outputLimit"`
	ShareResults         bool                `json:"shareResults"`
	WebSearch            *provider.WebSearch `json:"webSearch,omitempty"`
	DesktopNotifications bool                `json:"desktopNotifications,omitempty"`
}
type Proposal struct {
	ID        string   `json:"id"`
	Revision  int      `json:"revision"`
	RuntimeID string   `json:"runtimeId"`
	Owner     string   `json:"owner"`
	Manifest  Manifest `json:"manifest"`
}

func (m Manifest) Validate(now time.Time) error {
	if m.WebSearch != nil {
		if err := m.WebSearch.Validate(); err != nil {
			return fmt.Errorf("%w: invalid web search domains", ErrInvalid)
		}
	}
	if strings.TrimSpace(m.Name) == "" || len(m.Name) > 128 || len(m.Prompt) == 0 || len(m.Prompt) > 65536 || len(m.Model) == 0 || len(m.Model) > 191 || strings.ContainsAny(m.Model, " /\r\n\x00") || strings.ContainsAny(m.Name, "\r\n\x00") {
		return ErrInvalid
	}
	if len(strings.Fields(m.Cron)) != 5 || strings.Contains(m.Cron, "=") || len(m.Cron) > 256 || m.Timezone == "" || m.Timezone == "Local" {
		return fmt.Errorf("%w: provide five cron fields and an IANA timezone", ErrInvalid)
	}
	if _, err := time.LoadLocation(m.Timezone); err != nil {
		return fmt.Errorf("%w: unknown timezone", ErrInvalid)
	}
	if _, err := cron.ParseStandard(m.Cron); err != nil {
		return fmt.Errorf("%w: invalid cron", ErrInvalid)
	}
	if !m.Expires.After(now) || m.Expires.After(now.Add(366*24*time.Hour)) || m.MaxRunsPerDay < 1 || m.MaxRunsPerDay > 24 || m.TimeoutSeconds < 15 || m.TimeoutSeconds > 1800 || m.OutputLimit < 128 || m.OutputLimit > 65536 {
		return fmt.Errorf("%w: approval expires within a year; allow 1–24 runs/day, 15–1800 seconds and 128–65536 output bytes", ErrInvalid)
	}
	return nil
}
func (p Proposal) Digest(server string) string {
	data, _ := json.Marshal(struct {
		Server   string
		Proposal Proposal
	}{server, p})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func Origin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || (u.Path != "" && u.Path != "/") {
		return "", ErrInvalid
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()))) {
		return "", fmt.Errorf("%w: remote clusters require verified HTTPS", ErrInvalid)
	}
	u.Path = ""
	return u.String(), nil
}
func RuntimeID(key ed25519.PublicKey) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])
}

type ApprovalStatus struct {
	Revision             int                 `json:"revision"`
	Cron                 string              `json:"cron"`
	Timezone             string              `json:"timezone"`
	Model                string              `json:"model"`
	Expires              time.Time           `json:"expires"`
	ID                   string              `json:"id"`
	Digest               string              `json:"digest"`
	Paused               bool                `json:"paused"`
	WebSearch            *provider.WebSearch `json:"webSearch,omitempty"`
	DesktopNotifications bool                `json:"desktopNotifications,omitempty"`
}
type Run struct {
	ID           string              `json:"id"`
	ScheduleID   string              `json:"scheduleId"`
	Slot         string              `json:"slot"`
	Started      time.Time           `json:"started"`
	State        string              `json:"state"`
	Message      string              `json:"message,omitempty"`
	Result       string              `json:"result,omitempty"`
	Citations    []provider.Citation `json:"citations,omitempty"`
	WebSearched  bool                `json:"webSearched,omitempty"`
	Notification string              `json:"notification,omitempty"`
}
type Report struct {
	Paused    bool             `json:"paused"`
	SignedIn  bool             `json:"signedIn"`
	Models    []string         `json:"models"`
	Approvals []ApprovalStatus `json:"approvals"`
	Runs      []Run            `json:"runs"`
}
type Runtime struct {
	ID       string    `json:"id"`
	Owner    string    `json:"owner"`
	Name     string    `json:"name"`
	LastSeen time.Time `json:"lastSeen"`
	Report   Report    `json:"report"`
}
type Envelope struct {
	RuntimeID string          `json:"runtimeId"`
	Timestamp int64           `json:"timestamp"`
	Nonce     string          `json:"nonce"`
	Payload   json.RawMessage `json:"payload"`
	Signature string          `json:"signature"`
}

func (e Envelope) message() []byte {
	sum := sha256.Sum256(e.Payload)
	return []byte(fmt.Sprintf("sluicework-personal-sync-v1\n%s\n%d\n%s\n%x", e.RuntimeID, e.Timestamp, e.Nonce, sum))
}
func (e *Envelope) Sign(key ed25519.PrivateKey) {
	e.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, e.message()))
}
func (e Envelope) Verify(key ed25519.PublicKey, now time.Time) bool {
	sig, err := base64.RawURLEncoding.DecodeString(e.Signature)
	nonce, nerr := base64.RawURLEncoding.DecodeString(e.Nonce)
	return err == nil && nerr == nil && len(nonce) == 24 && e.RuntimeID == RuntimeID(key) && e.Timestamp >= now.Add(-2*time.Minute).Unix() && e.Timestamp <= now.Add(2*time.Minute).Unix() && len(e.Payload) <= 8<<20 && ed25519.Verify(key, e.message(), sig)
}
