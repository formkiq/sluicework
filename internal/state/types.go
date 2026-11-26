// Package state owns the durable worker registry, job revisions and attempt lifecycle.
package state

import (
	"database/sql"
	"errors"
	"time"
)

var (
	ErrNotFound     = errors.New("resource not found")
	ErrConflict     = errors.New("operation conflicts with current state")
	ErrInvalid      = errors.New("invalid request")
	ErrUnauthorized = errors.New("credential is invalid or expired")
	ErrLease        = errors.New("attempt lease is no longer active")
)

type Store struct {
	LogLimitPolicy         func() int
	DB                     *sql.DB
	LeaseTTL, OfflineAfter time.Duration
	LogLimit               int
}

type WorkerInfo struct {
	Workstation  *WorkstationStatus `json:"workstation,omitempty"`
	Name         string             `json:"name"`
	Version      string             `json:"version"`
	OS           string             `json:"os,omitempty"`
	Arch         string             `json:"arch,omitempty"`
	Roles        []string           `json:"roles"`
	RunnerStatus string             `json:"runnerStatus,omitempty"`
	RunnerReason string             `json:"runnerReason,omitempty"`
}

type Runner struct {
	Control     *WorkstationControlRequest `json:"control,omitempty"`
	Settings    *WorkstationSettings       `json:"settings,omitempty"`
	Workstation bool                       `json:"workstation,omitempty"`
	Name        string                     `json:"name"`
	Scope       string                     `json:"scope"` // GitHub owner or owner/repository.
	Labels      []string                   `json:"labels"`
	Service     string                     `json:"service"` // Exact launchd service label.
}

type Machine struct {
	WorkerInfo
	ID            string         `json:"id"`
	Labels        []string       `json:"labels"`
	Slots         int            `json:"slots"`
	RoleLimits    map[string]int `json:"roleLimits"`
	Runner        *Runner        `json:"runner,omitempty"`
	LastHeartbeat time.Time      `json:"lastHeartbeat"`
	Online        bool           `json:"online"`
	Draining      bool           `json:"draining"`
	Revoked       bool           `json:"revoked"`
	Running       int            `json:"running"`
}
type MachinePatch struct {
	PreserveRunnerSettings bool            `json:"-"`
	Labels                 *[]string       `json:"labels,omitempty"`
	Slots                  *int            `json:"slots,omitempty"`
	RoleLimits             *map[string]int `json:"roleLimits,omitempty"`
	Draining               *bool           `json:"draining,omitempty"`
	Runner                 *Runner         `json:"runner,omitempty"` // Empty name removes a registration.
}
type Enrollment struct {
	ID         string `json:"id"`
	Credential string `json:"credential"`
}

type Input struct {
	Name     string   `json:"name"`
	Required bool     `json:"required"`
	Paths    []string `json:"paths,omitempty"` // An explicit list of readable worker-side files; empty means literal text.
}
type Job struct {
	ID             string   `json:"id"`
	Revision       int      `json:"revision"`
	Name           string   `json:"name"`
	Type           string   `json:"type"` // script or ai
	Role           string   `json:"role"`
	Labels         []string `json:"labels"`
	OS             string   `json:"os,omitempty"`   // Empty permits either supported operating system.
	Arch           string   `json:"arch,omitempty"` // Empty permits either supported CPU architecture.
	Inputs         []Input  `json:"inputs"`
	Command        string   `json:"command,omitempty"`
	Args           []string `json:"args,omitempty"`
	WorkDir        string   `json:"workDir,omitempty"` // Relative directory inside fresh attempt directory.
	TimeoutSeconds int      `json:"timeoutSeconds"`
	Prompt         string   `json:"prompt,omitempty"`
	Connection     string   `json:"connection,omitempty"`
	Model          string   `json:"model,omitempty"`
	MaxTokens      int      `json:"maxTokens"`
	AllowCloud     bool     `json:"allowCloud"`
	OutputFormat   string   `json:"outputFormat,omitempty"` // text or json
	OutputLimit    int      `json:"outputLimit"`            // bytes
	DiscardOutput  bool     `json:"discardOutput"`
	MaxAutoRetries int      `json:"maxAutoRetries"`
	SafeToRepeat   bool     `json:"safeToRepeat"`
}
type Attempt struct {
	ID              string     `json:"id"`
	Number          int        `json:"number"`
	State           string     `json:"state"`
	MachineID       string     `json:"machineId,omitempty"`
	QueuedAt        time.Time  `json:"queuedAt"`
	StartedAt       *time.Time `json:"startedAt,omitempty"`
	EndedAt         *time.Time `json:"endedAt,omitempty"`
	LeaseUntil      *time.Time `json:"leaseUntil,omitempty"`
	Automatic       bool       `json:"automatic"`
	CancelRequested bool       `json:"cancelRequested"`
	OutcomeUnknown  bool       `json:"outcomeUnknown"`
	Output          string     `json:"output,omitempty"`
	OutputTruncated bool       `json:"outputTruncated"`
	LogsTruncated   bool       `json:"logsTruncated"`
	Error           string     `json:"error,omitempty"`
}
type Run struct {
	ID         string            `json:"id"`
	JobID      string            `json:"jobId"`
	Job        Job               `json:"job"`
	Inputs     map[string]string `json:"inputs"`
	Trigger    string            `json:"trigger"`
	ScheduleID string            `json:"scheduleId,omitempty"`
	CreatedAt  time.Time         `json:"createdAt"`
	State      string            `json:"state"`
	Attempts   []Attempt         `json:"attempts"` // ascending attempt number
}
type Claim struct {
	RunID      string            `json:"runId"`
	AttemptID  string            `json:"attemptId"`
	Job        Job               `json:"job"`
	Inputs     map[string]string `json:"inputs"`
	LeaseUntil time.Time         `json:"leaseUntil"`
}
type Renewal struct {
	LeaseUntil      time.Time `json:"leaseUntil"`
	CancelRequested bool      `json:"cancelRequested"`
}
type Completion struct {
	State           string `json:"state"`
	Output          string `json:"output"`
	OutputTruncated bool   `json:"outputTruncated"`
	Error           string `json:"error"`
}
type Log struct {
	ID        int64     `json:"id"`
	AttemptID string    `json:"attemptId"`
	At        time.Time `json:"at"`
	Text      string    `json:"text"`
	Truncated bool      `json:"truncated,omitempty"`
}
