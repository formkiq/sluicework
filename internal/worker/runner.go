package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
)

var runnerTarget = regexp.MustCompile(`^((system|gui/[0-9]+|user/[0-9]+)/)?actions\.runner\.[a-zA-Z0-9_.-]+$`)

func runnerStatus(ctx context.Context, service, file string) string {
	status := launchdStatus(ctx, service)
	if runtime.GOOS == "linux" {
		status = systemdStatus(ctx, service)
	}
	if file == "" || (status != "unknown" && status != "unloaded") {
		return status
	}
	// A differently owned LaunchAgent can publish only its service name and
	// status. Freshness prevents an old successful report masking an outage.
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return status
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > 2048 || st.Mode().Perm()&0002 != 0 {
		return status
	}
	age := time.Since(st.ModTime())
	if age < 0 || age > 2*time.Minute {
		return status
	}
	var report struct {
		Service string `json:"service"`
		Status  string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(f, 2048)).Decode(&report); err != nil || report.Service != service {
		return status
	}
	switch report.Status {
	case "running", "stopped", "unloaded", "unknown":
		return report.Status
	default:
		return status
	}
}

var runnerUnit = regexp.MustCompile(`^actions\.runner\.[a-zA-Z0-9_.-]+\.service$`)

func systemdStatus(ctx context.Context, service string) string {
	if service == "" {
		return ""
	}
	if !runnerUnit.MatchString(service) {
		return "unknown"
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/systemctl", "show", "--no-pager", "--property=LoadState,ActiveState", "--", service)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
	output, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return parseSystemdStatus(string(output))
}

func parseSystemdStatus(output string) string {
	values := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			values[k] = v
		}
	}
	if values["LoadState"] == "not-found" {
		return "unloaded"
	}
	if values["LoadState"] != "loaded" {
		return "unknown"
	}
	switch values["ActiveState"] {
	case "active":
		return "running"
	case "inactive", "failed":
		return "stopped"
	default:
		return "unknown"
	}
}

func launchdStatus(ctx context.Context, service string) string {
	if service == "" {
		return ""
	}
	if runtime.GOOS != "darwin" || !runnerTarget.MatchString(service) {
		return "unknown"
	}
	targets := []string{service}
	if !strings.Contains(service, "/") {
		targets = []string{"system/" + service, fmt.Sprintf("gui/%d/%s", os.Getuid(), service), fmt.Sprintf("user/%d/%s", os.Getuid(), service)}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	missing := true
	for _, target := range targets {
		cmd := exec.CommandContext(ctx, "/bin/launchctl", "print", target)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
		output, err := cmd.CombinedOutput()
		text := string(output)
		if err == nil {
			for _, line := range strings.Split(text, "\n") {
				if strings.TrimSpace(line) == "state = running" {
					return "running"
				}
			}
			return "stopped"
		}
		if !strings.Contains(text, "Could not find") && !strings.Contains(text, "Could not print domain") {
			missing = false
		}
	}
	if missing {
		return "unloaded"
	}
	return "unknown"
}
