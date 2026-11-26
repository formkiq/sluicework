package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"sluicework/internal/state"
)

func TestSandboxParametersKeepPathsOutOfProfile(t *testing.T) {
	base := t.TempDir()
	run := filepath.Join(base, "quoted \"(run)")
	if err := os.Mkdir(run, 0700); err != nil {
		t.Fatal(err)
	}
	argv, err := sandboxArgs("/bin/echo", run, base, base, []string{"-D", "INPUT=/", "text"})
	if err != nil {
		t.Fatal(err)
	}
	at := slices.Index(argv, "-p")
	if at < 1 || len(argv) != at+6 {
		t.Fatalf("unexpected invocation shape %q", argv)
	}
	profile := argv[at+1]
	canonical, err := filepath.EvalSymlinks(run)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv[:at], "RUN="+canonical) || strings.Contains(profile, base) || !strings.Contains(profile, "(allow default)") || !strings.Contains(profile, `(subpath (param "STATE"))`) {
		t.Fatal("sandbox paths must be bound as arguments to a fixed policy")
	}
	if !slices.Equal(argv[at+3:], []string{"-D", "INPUT=/", "text"}) {
		t.Fatal("approved argv was altered")
	}
}

func TestNativeScriptFilesystemBoundary(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("requires macOS Seatbelt or Linux bubblewrap")
	}
	if os.Geteuid() == 0 {
		t.Skip("worker refuses root on macOS")
	}
	base := t.TempDir()
	private := filepath.Join(base, "worker")
	source := filepath.Join(base, "inputs")
	jobs := filepath.Join(private, "jobs")
	other := filepath.Join(jobs, "attempt-other")
	for _, dir := range []string{source, other} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cred := filepath.Join(private, "worker.json")
	adjacent := filepath.Join(private, "other-settings")
	otherOutput := filepath.Join(other, "output")
	report := filepath.Join(source, "approved.txt")
	toolDependency := filepath.Join(base, "custom-runtime-data")
	if err := os.WriteFile(toolDependency, []byte("runtime"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{cred, adjacent, otherOutput, report} {
		if err := os.WriteFile(file, []byte("contents"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer server.Close()
	c, err := newClient(Config{URL: server.URL, WorkDir: jobs, CredentialFile: cred, InputRoot: source})
	if err != nil {
		t.Fatal(err)
	}
	claim := state.Claim{AttemptID: "native", Inputs: map[string]string{"report": report},
		Job: state.Job{Type: "script", Command: "/bin/sh", Inputs: []state.Input{{Name: "report", Paths: []string{report}}},
			Args: []string{"-c", `set -e
test "$(cat "$1")" = contents
/usr/bin/curl --fail --silent --show-error --noproxy '*' "$2" >/dev/null
test "$(cat "$3")" = runtime
shift 3
for private in "$@"; do
  if cat "$private" >/dev/null 2>&1; then exit 20; fi
  if (printf changed >"$private") 2>/dev/null; then exit 21; fi
done
/usr/bin/uname -m > result
test -s result
printf isolated`, "approved", "{{report}}", server.URL, toolDependency, cred, adjacent, otherOutput, report}, OutputLimit: 100}}
	got := c.execute(context.Background(), claim)
	if got.State != "succeeded" || got.Output != "isolated" {
		t.Fatalf("native script isolation: %+v", got)
	}
}
