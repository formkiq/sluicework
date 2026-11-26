package worker

import (
	"errors"
	"path/filepath"
	"slices"
	"sort"
)

// Host tools and their dependencies are readable, while only this attempt is
// writable. Fresh PID/proc namespaces hide the daemon and other attempts.
// Paths are argv entries, never interpolated into shell or policy text.
func linuxSandboxArgs(command, scratch, credentialDir, inputRoot string, argv []string) ([]string, error) {
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
	paths := []string{credentialDir, filepath.Dir(run)}
	if inputRoot != "" {
		paths = append(paths, inputRoot)
	}
	masked := []string{}
	for _, path := range paths {
		path, err = canonical(path)
		if err != nil {
			return nil, err
		}
		if path == "/" || path == run {
			return nil, errors.New("protected directories must be narrower than root and separate from the attempt")
		}
		if !slices.Contains(masked, path) {
			masked = append(masked, path)
		}
	}
	sort.Slice(masked, func(i, j int) bool { return len(masked[i]) < len(masked[j]) })
	args := []string{"--die-with-parent", "--new-session", "--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts", "--cap-drop", "ALL", "--ro-bind", "/", "/", "--proc", "/proc", "--dev", "/dev"}
	for _, path := range masked {
		args = append(args, "--tmpfs", path)
	}
	args = append(args, "--bind", run, run)
	// Create the scratch mount before making the empty masks read-only.
	// Remounting a parent does not change the scratch submount's write access.
	for _, path := range masked {
		args = append(args, "--remount-ro", path)
	}
	args = append(args, "--", executable)
	return append(args, argv...), nil
}
