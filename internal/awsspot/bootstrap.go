package awsspot

import (
	_ "embed"
	"encoding/json"
	"errors"
	"sluicework/internal/spot"
	"strconv"
	"strings"
)

//go:embed bootstrap.sh
var bootstrap string

//go:embed job-start.py
var jobStartHook string

func RenderBootstrap(s spot.LaunchSpec) (string, error) {
	if e := s.Validate(); e != nil {
		return "", e
	}
	payload := struct {
		Job     spot.Job     `json:"job"`
		Label   string       `json:"label"`
		Package spot.Package `json:"package"`
		JIT     string       `json:"jit"`
		CPUs    int          `json:"cpus"`
		Expires int64        `json:"expires"`
	}{s.Job, s.Label, s.Package, s.JIT, s.Profile.CPUs, s.Expires.Unix()}
	b, e := json.Marshal(payload)
	if e != nil {
		return "", errors.New("cannot encode one-job runner configuration")
	}
	script := strings.ReplaceAll(strings.ReplaceAll(bootstrap, "__PRIVATE_CONFIG__", string(b)), "__EXPIRES_EPOCH__", strconv.FormatInt(s.Expires.Unix(), 10))
	script = strings.ReplaceAll(script, "__JOB_START_HOOK__", jobStartHook)
	if len(script) > 16384 || s.JIT == "" || s.Profile.CPUs < 1 || s.Profile.CPUs > 2 {
		return "", errors.New("one-job runner configuration exceeds bootstrap limits")
	}
	return script, nil
}
