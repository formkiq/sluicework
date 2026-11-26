package state

import "strings"

func validPlatform(os, arch string) bool {
	return (os == "" || os == "darwin" || os == "linux") && (arch == "" || arch == "arm64" || arch == "amd64")
}

// GitHub owns workflow placement. These reported labels must not advertise a
// platform that disagrees with this worker; they do not register a GitHub runner.
func validateRunnerPlatform(info WorkerInfo, runner Runner) error {
	if !runner.Workstation && info.OS == "linux" && (strings.Contains(runner.Service, "/") || !strings.HasSuffix(runner.Service, ".service")) {
		return invalid("Linux runners require an actions.runner.*.service systemd unit")
	}
	for _, label := range runner.Labels {
		switch strings.ToLower(label) {
		case "linux":
			if info.OS != "" && info.OS != "linux" {
				return invalid("runner Linux label does not match this machine")
			}
		case "macos":
			if info.OS != "" && info.OS != "darwin" {
				return invalid("runner macOS label does not match this machine")
			}
		case "arm64":
			if info.Arch != "" && info.Arch != "arm64" {
				return invalid("runner ARM64 label does not match this machine")
			}
		case "x64":
			if info.Arch != "" && info.Arch != "amd64" {
				return invalid("runner X64 label does not match this machine")
			}
		case "windows", "arm":
			return invalid("unsupported runner platform label")
		}
	}
	return nil
}
