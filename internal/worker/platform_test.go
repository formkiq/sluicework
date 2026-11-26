package worker

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sluicework/internal/state"
	"testing"
)

func TestWorkerReportsActualPlatform(t *testing.T) {
	info := (&client{config: Config{Name: "worker"}}).info()
	if info.OS != runtime.GOOS || info.Arch != runtime.GOARCH {
		t.Fatalf("platform %+v", info)
	}
}

func TestWorkerRejectsIncompatibleClaimBeforeReadingFiles(t *testing.T) {
	other := "linux"
	if runtime.GOOS == "linux" {
		other = "darwin"
	}
	result := (&client{}).execute(context.Background(), state.Claim{Job: state.Job{OS: other}})
	if result.State != "failed" || result.Error != "job requires a different operating system or CPU architecture" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestLinuxSandboxKeepsPathsAndArgumentsSeparate(t *testing.T) {
	base := t.TempDir()
	run := filepath.Join(base, "jobs", "quoted \"(run)")
	if err := os.MkdirAll(run, 0700); err != nil {
		t.Fatal(err)
	}
	args, err := linuxSandboxArgs("/bin/echo", run, base, "", []string{"--bind", "/", "/", "text"})
	if err != nil {
		t.Fatal(err)
	}
	separator := slices.Index(args, "--")
	if separator < 0 || !slices.Equal(args[separator+2:], []string{"--bind", "/", "/", "text"}) {
		t.Fatalf("command arguments escaped: %q", args)
	}
	for _, flag := range []string{"--unshare-user", "--unshare-pid", "--proc", "--ro-bind", "--remount-ro", "--new-session", "--die-with-parent"} {
		if !slices.Contains(args[:separator], flag) {
			t.Fatal("missing boundary", flag)
		}
	}
	if _, err = linuxSandboxArgs("/bin/echo", run, "/", "", nil); err == nil {
		t.Fatal("root masking accepted")
	}
}

func TestSystemdStatusDoesNotTreatTransitionsAsRunning(t *testing.T) {
	for output, want := range map[string]string{
		"LoadState=loaded\nActiveState=active\n":      "running",
		"ActiveState=failed\nLoadState=loaded\n":      "stopped",
		"LoadState=not-found\nActiveState=inactive\n": "unloaded",
		"LoadState=loaded\nActiveState=activating\n":  "unknown",
		"ActiveState=active\n":                        "unknown",
	} {
		if got := parseSystemdStatus(output); got != want {
			t.Fatalf("%q: %s, want %s", output, got, want)
		}
	}
	if runnerUnit.MatchString("actions.runner.foo.service/../../other") || runnerUnit.MatchString("other.service") {
		t.Fatal("unrelated unit accepted")
	}
}
