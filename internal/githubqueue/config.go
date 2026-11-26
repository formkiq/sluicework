package githubqueue

import (
	"errors"
	"regexp"
	"sluicework/internal/spot"
	"strings"
	"time"
)

type Coordinator struct {
	File   string `json:"file"`
	Kind   string `json:"kind"`
	Check  string `json:"check"`
	Deploy bool   `json:"deploy"`
}
type Repository struct {
	Name          string        `json:"name"`
	ID            int64         `json:"id"`
	Branch        string        `json:"branch"`
	HeartbeatSHA  string        `json:"heartbeatSha"`
	Priority      Priority      `json:"priority"`
	Coordinators  []Coordinator `json:"coordinators"`
	CloudPlatform string        `json:"cloudPlatform"`
}
type Config struct {
	Spot            *spot.Infrastructure `json:"spot,omitempty"`
	BackendID       string               `json:"backendId"`
	AppID           int64                `json:"appId"`
	InstallationID  int64                `json:"installationId"`
	StateFile       string               `json:"stateFile,omitempty"`
	PrivateKeyFile  string               `json:"privateKeyFile,omitempty"`
	Repositories    []Repository         `json:"repositories"`
	Primary         Target               `json:"primary"`
	Fallback        Target               `json:"fallback"`
	FallbackMinutes int                  `json:"fallbackMinutes"`
	Since           time.Time            `json:"since"`
}

var shaPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)
var branchPattern = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)
var uuidPattern = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$`)

func (c Config) Validate() error {
	if !namePattern.MatchString(c.BackendID) || c.AppID <= 0 || c.InstallationID <= 0 || len(c.Repositories) < 1 || len(c.Repositories) > 10 || c.Since.IsZero() {
		return errors.New("configure a backend ID, GitHub App, start time and 1–10 repositories")
	}
	seen := map[string]bool{}
	for _, r := range c.Repositories {
		parts := strings.Split(r.Name, "/")
		if len(parts) != 2 || !namePattern.MatchString(parts[0]) || !namePattern.MatchString(parts[1]) || r.ID <= 0 || !branchPattern.MatchString(r.Branch) || strings.Contains(r.Branch, "..") || !shaPattern.MatchString(r.HeartbeatSHA) || seen[strings.ToLower(r.Name)] || len(r.Coordinators) == 0 {
			return errors.New("invalid queue repository allowlist")
		}
		seen[strings.ToLower(r.Name)] = true
		for _, coordinator := range r.Coordinators {
			if !strings.HasSuffix(coordinator.File, ".yml") || !namePattern.MatchString(coordinator.File) || !namePattern.MatchString(coordinator.Kind) || coordinator.Check == "" || len(coordinator.Check) > 100 {
				return errors.New("invalid queue workflow integration")
			}
		}
		if err := (Policy{Priority: r.Priority, Primary: c.Primary, Fallback: c.Fallback, Cloud: Target{Platform: r.CloudPlatform, Host: "cloud"}, FallbackMinutes: c.FallbackMinutes}).Validate(); err != nil {
			return err
		}
	}
	if c.Spot != nil {
		repos := []string{}
		for _, r := range c.Repositories {
			repos = append(repos, r.Name)
		}
		if err := c.Spot.Validate(repos); err != nil {
			return err
		}
	}
	return nil
}
