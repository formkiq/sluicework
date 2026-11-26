package spot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

var ErrConflict = errors.New("Spot fleet changed")
var ErrCapacity = errors.New("all Spot runner slots are reserved")
var ErrDisabled = errors.New("Spot runner admission is paused")
var ErrUncertain = errors.New("Spot launch outcome is not confirmed; reservation retained")

type Job struct {
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	Repository  string `json:"repository"`
	WorkflowRef string `json:"workflowRef"`
	WorkflowSHA string `json:"workflowSha"`
	SourceSHA   string `json:"sourceSha"`
	RequestID   string `json:"requestId"`
	Policy      Policy `json:"policy"`
}
type Package struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// LaunchSpec is persisted before RunInstances. Retries must reuse every field,
// especially ClientToken and SubnetID: EC2 idempotency is scoped to its AZ.
// JIT is a one-job secret and must never be included in receipts or admin output.
type LaunchSpec struct {
	RootDeviceName  string    `json:"rootDeviceName"`
	ClientToken     string    `json:"clientToken"`
	Name            string    `json:"name"`
	Label           string    `json:"label"`
	BackendID       string    `json:"backendId"`
	Job             Job       `json:"job"`
	Profile         Profile   `json:"profile"`
	ImageID         string    `json:"imageId"`
	SubnetID        string    `json:"subnetId"`
	SecurityGroupID string    `json:"securityGroupId"`
	VolumeGiB       int       `json:"volumeGiB"`
	Expires         time.Time `json:"expires"`
	Package         Package   `json:"package"`
	JIT             string    `json:"jit"`
}
type Allocation struct {
	Spec            LaunchSpec `json:"spec"`
	State           string     `json:"state"`
	Created         time.Time  `json:"created"`
	RunnerID        int64      `json:"runnerId,omitempty"`
	InstanceID      string     `json:"instanceId,omitempty"`
	LaunchUncertain bool       `json:"launchUncertain,omitempty"`
	EndReason       string     `json:"endReason,omitempty"`
}
type State struct {
	Version     int64                 `json:"version"`
	Allocations map[string]Allocation `json:"allocations"`
}

// Store atomically saves the whole bounded fleet, so concurrent admissions
// cannot exceed the global cap. Occupied records have no TTL.
type Store interface {
	Load(context.Context) (State, error)
	Save(context.Context, *State) error
}
type PublicAllocation struct {
	ID           string    `json:"id"`
	State        string    `json:"state"`
	Profile      string    `json:"profile"`
	InstanceType string    `json:"instanceType"`
	InstanceID   string    `json:"instanceId,omitempty"`
	RunnerID     int64     `json:"runnerId,omitempty"`
	Label        string    `json:"label"`
	Created      time.Time `json:"created"`
	Expires      time.Time `json:"expires"`
	EndReason    string    `json:"endReason,omitempty"`
}

func (a Allocation) Public() PublicAllocation {
	return PublicAllocation{a.Spec.Job.ID, a.State, a.Spec.Profile.ID, a.Spec.Profile.InstanceType, a.InstanceID, a.RunnerID, a.Spec.Label, a.Created, a.Spec.Expires, a.EndReason}
}
func cloneState(s State) State {
	b, _ := json.Marshal(s)
	var n State
	_ = json.Unmarshal(b, &n)
	if n.Allocations == nil {
		n.Allocations = map[string]Allocation{}
	}
	return n
}

// Validate rejects mutated persisted bootstrap inputs before an EC2 call.
func (s LaunchSpec) Validate() error {
	p, e := LookupProfile(s.Profile.ID)
	if e != nil || p != s.Profile || !validJob(s.Job) || !approvedPackage(s.Package) || !namePattern.MatchString(s.BackendID) || !namePattern.MatchString(s.Name) || s.Name != s.Label || !strings.HasPrefix(s.Name, "sluicework-spot-") || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(s.ClientToken) || !regexp.MustCompile(`^ami-[a-f0-9]{8,17}$`).MatchString(s.ImageID) || !regexp.MustCompile(`^subnet-[a-f0-9]{8,17}$`).MatchString(s.SubnetID) || !regexp.MustCompile(`^sg-[a-f0-9]{8,17}$`).MatchString(s.SecurityGroupID) || s.RootDeviceName != "/dev/sda1" || s.VolumeGiB < 16 || s.VolumeGiB > 128 || s.Expires.IsZero() || len(s.JIT) == 0 || len(s.JIT) > 8192 {
		return errors.New("invalid persisted Spot launch specification")
	}
	if _, e := base64.StdEncoding.DecodeString(s.JIT); e != nil {
		return errors.New("invalid one-job runner grant")
	}
	return nil
}
