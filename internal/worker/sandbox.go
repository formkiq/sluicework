package worker

import (
	"context"
	_ "embed"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

//go:embed script.sb
var scriptPolicy string

const sandboxExec = "/usr/bin/sandbox-exec"

// SBPL paths are bound parameters. A path containing quotes, parentheses or a
// newline is exactly one argument, and never becomes executable profile text.
func sandboxArgs(command, scratch, credentialDir, inputRoot string, argv []string) ([]string, error) {
	canonical := func(path string) (string, error) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(abs)
	}
	run, err := canonical(scratch)
	if err != nil {
		return nil, err
	}
	executable, err := canonical(command)
	if err != nil {
		return nil, err
	}
	private, err := canonical(credentialDir)
	if err != nil {
		return nil, err
	}
	profile := scriptPolicy
	args := []string{"-D", "RUN=" + run, "-D", "STATE=" + private, "-D", "JOBS=" + filepath.Dir(run)}
	if inputRoot != "" {
		input, err := canonical(inputRoot)
		if err != nil {
			return nil, err
		}
		args = append(args, "-D", "INPUT="+input)
		profile += `
(deny file-read-data file-map-executable file-write*
    (require-all (subpath (param "INPUT")) (require-not (subpath (param "RUN")))))
`
	}
	args = append(args, "-p", profile, executable)
	return append(args, argv...), nil
}

func (c *client) scriptCommand(ctx context.Context, approved, scratch string, args []string) (*exec.Cmd, error) {
	if os.Geteuid() == 0 {
		return nil, errors.New("run the worker under its dedicated unprivileged account")
	}
	if runtime.GOOS == "linux" {
		argv, err := linuxSandboxArgs(approved, scratch, filepath.Dir(c.config.CredentialFile), c.config.InputRoot, args)
		if err != nil {
			return nil, err
		}
		// Never fall back to unconfined execution if bubblewrap or namespaces
		// are unavailable. The attempt records the startup failure.
		helper := "/usr/bin/bwrap"
		if _, err := os.Stat("/opt/sluicework/bin/sluicework-bwrap"); err == nil {
			helper = "/opt/sluicework/bin/sluicework-bwrap"
		}
		return exec.CommandContext(ctx, helper, argv...), nil
	}
	if runtime.GOOS != "darwin" {
		return nil, errors.New("script workers support only macOS and Linux")
	}
	sandboxed, err := sandboxArgs(approved, scratch, filepath.Dir(c.config.CredentialFile), c.config.InputRoot, args)
	if err != nil {
		return nil, err
	}
	// A missing sandbox-exec or a rejected policy fails the attempt on macOS.
	return exec.CommandContext(ctx, sandboxExec, sandboxed...), nil
}
