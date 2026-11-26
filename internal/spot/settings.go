// Package spot manages a bounded pool of single-job cloud runners.
package spot

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

const HardLimit = 3
const Variable = "SLUICEWORK_SPOT"

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,99}$`)
var repoPattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$`)

type Profile struct {
	ID           string `json:"id"`
	InstanceType string `json:"instanceType"`
	CPUs         int    `json:"cpus"`
	MemoryMiB    int    `json:"memoryMiB"`
	MaxPrice     string `json:"maxPrice"`
}

func Profiles() []Profile {
	return []Profile{{"small", "c6gd.medium", 1, 2048, "0.025"}, {"standard", "c8gn.large", 2, 4096, "0.06"}}
}
func LookupProfile(id string) (Profile, error) {
	for _, p := range Profiles() {
		if p.ID == id {
			return p, nil
		}
	}
	return Profile{}, errors.New("select a supported ARM runner profile")
}

type Infrastructure struct {
	AccountID         string   `json:"accountId"`
	RootDeviceName    string   `json:"rootDeviceName"`
	Region            string   `json:"region"`
	ImageID           string   `json:"imageId"`
	SubnetIDs         []string `json:"subnetIds"`
	SecurityGroupID   string   `json:"securityGroupId"`
	RunnerGroupID     int64    `json:"runnerGroupId"`
	ControlRepository string   `json:"controlRepository"`
	VolumeGiB         int      `json:"volumeGiB"`
}

func (c Infrastructure) Validate(approved []string) error {
	if !regexp.MustCompile(`^[0-9]{12}$`).MatchString(c.AccountID) || c.RootDeviceName != "/dev/sda1" || !regexp.MustCompile(`^[a-z]{2}(?:-[a-z]+)+-[0-9]+$`).MatchString(c.Region) || !regexp.MustCompile(`^ami-[a-f0-9]{8,17}$`).MatchString(c.ImageID) || !regexp.MustCompile(`^sg-[a-f0-9]{8,17}$`).MatchString(c.SecurityGroupID) || c.RunnerGroupID <= 0 || c.VolumeGiB < 16 || c.VolumeGiB > 128 || len(c.SubnetIDs) < 1 || len(c.SubnetIDs) > 6 {
		return errors.New("configure the Spot region, ARM image, private runner group, public subnets and encrypted root volume")
	}
	allowed := false
	for _, r := range approved {
		if r == c.ControlRepository {
			allowed = true
		}
	}
	if !allowed {
		return errors.New("Spot control repository is outside the dispatcher allowlist")
	}
	seen := map[string]bool{}
	for _, id := range c.SubnetIDs {
		if !regexp.MustCompile(`^subnet-[a-f0-9]{8,17}$`).MatchString(id) || seen[id] {
			return errors.New("invalid or repeated Spot subnet")
		}
		seen[id] = true
	}
	return nil
}

type Settings struct {
	Version        int               `json:"version"`
	BackendID      string            `json:"backendId"`
	Enabled        bool              `json:"enabled"`
	MaxConcurrent  int               `json:"maxConcurrent"`
	QueueMinutes   int               `json:"queueMinutes"`
	StartupMinutes int               `json:"startupMinutes"`
	RuntimeMinutes int               `json:"runtimeMinutes"`
	Repositories   map[string]string `json:"repositories"`
}

func Defaults(backend string) Settings {
	return Settings{Version: 1, BackendID: backend, MaxConcurrent: HardLimit, QueueMinutes: 5, StartupMinutes: 10, RuntimeMinutes: 90, Repositories: map[string]string{}}
}
func (s Settings) Validate(backend string, approved []string) error {
	if s.Version != 1 || !namePattern.MatchString(backend) || s.BackendID != backend || s.MaxConcurrent < 0 || s.MaxConcurrent > HardLimit || s.QueueMinutes < 1 || s.QueueMinutes > 30 || s.StartupMinutes < 1 || s.StartupMinutes > 15 || s.RuntimeMinutes < 15 || s.RuntimeMinutes > 180 || len(s.Repositories) > 10 {
		return fmt.Errorf("invalid Spot settings: maximum concurrent runners must be between zero and %d", HardLimit)
	}
	allow := map[string]bool{}
	for _, r := range approved {
		allow[r] = true
	}
	for r, p := range s.Repositories {
		if !repoPattern.MatchString(r) || !allow[r] {
			return errors.New("Spot repository is outside the approved connection")
		}
		if _, e := LookupProfile(p); e != nil {
			return e
		}
	}
	return nil
}
func ParseSettings(raw string, backend string, approved []string) (Settings, error) {
	var s Settings
	if len(raw) > 16384 {
		return s, errors.New("Spot configuration is too large")
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&s); err != nil {
		return s, errors.New("invalid Spot configuration")
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return s, errors.New("invalid Spot configuration")
	}
	return s, s.Validate(backend, approved)
}

type Policy struct {
	Profile        string `json:"profile"`
	QueueMinutes   int    `json:"queueMinutes"`
	StartupMinutes int    `json:"startupMinutes"`
	RuntimeMinutes int    `json:"runtimeMinutes"`
}

func (s Settings) Policy(repository string) *Policy {
	p, ok := s.Repositories[repository]
	if !s.Enabled || s.MaxConcurrent == 0 || !ok {
		return nil
	}
	return &Policy{p, s.QueueMinutes, s.StartupMinutes, s.RuntimeMinutes}
}
func (p Policy) Validate() error {
	if _, e := LookupProfile(p.Profile); e != nil {
		return e
	}
	if p.QueueMinutes < 1 || p.QueueMinutes > 30 || p.StartupMinutes < 1 || p.StartupMinutes > 15 || p.RuntimeMinutes < 15 || p.RuntimeMinutes > 180 {
		return errors.New("invalid accepted Spot policy")
	}
	return nil
}
func (p Policy) QueueWindow() time.Duration { return time.Duration(p.QueueMinutes) * time.Minute }
