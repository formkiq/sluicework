// Package cliinstall exposes an installed executable without replacing other commands.
package cliinstall

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type Options struct {
	Executable, Home, Shell, Path, ZDotDir string
	System, NoShell                        bool
}

type Result struct {
	Command  string
	Profiles []string
}

const pathBlock = `# >>> Sluicework CLI >>>
case ":${PATH-}:" in
  *":$HOME/.local/bin:"*) ;;
  *) export PATH="$HOME/.local/bin${PATH:+:$PATH}" ;;
esac
# <<< Sluicework CLI <<<
`

const fishBlock = `# >>> Sluicework CLI >>>
if not contains -- "$HOME/.local/bin" $PATH
    set -gx PATH "$HOME/.local/bin" $PATH
end
# <<< Sluicework CLI <<<
`

func Install(o Options) (Result, error) {
	var result Result
	executable, err := filepath.EvalSymlinks(o.Executable)
	if err != nil {
		return result, err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return result, err
	}
	info, err := os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return result, errors.New("choose an existing executable")
	}
	dir := filepath.Join(o.Home, ".local", "bin")
	if o.System {
		if os.Geteuid() != 0 {
			return result, errors.New("--system requires administrator access")
		}
		dir = "/usr/local/bin"
	} else if os.Geteuid() == 0 {
		return result, errors.New("run without sudo for your login, or use --system")
	} else if !filepath.IsAbs(o.Home) {
		return result, errors.New("cannot determine your home directory")
	}
	result.Command = filepath.Join(dir, "sluicework")
	if err = linkCommand(executable, dir); err != nil {
		return result, err
	}
	if o.System || o.NoShell || onPath(dir, o.Path) {
		return result, nil
	}
	profiles := []string{filepath.Join(o.Home, ".profile")}
	block := pathBlock
	switch filepath.Base(o.Shell) {
	case "zsh":
		dot := o.ZDotDir
		if dot == "" {
			dot = o.Home
		}
		if !filepath.IsAbs(dot) {
			return result, errors.New("ZDOTDIR must be absolute; CLI link installed, shell setup unchanged")
		}
		profiles = []string{filepath.Join(dot, ".zprofile"), filepath.Join(dot, ".zshrc")}
	case "bash":
		login := filepath.Join(o.Home, ".profile")
		for _, name := range []string{".bash_profile", ".bash_login", ".profile"} {
			p := filepath.Join(o.Home, name)
			if _, e := os.Lstat(p); e == nil {
				login = p
				break
			}
		}
		profiles = []string{login, filepath.Join(o.Home, ".bashrc")}
	case "fish":
		profiles = []string{filepath.Join(o.Home, ".config", "fish", "conf.d", "sluicework.fish")}
		block = fishBlock
	}
	for _, profile := range profiles {
		changed, e := addPath(profile, block)
		if e != nil {
			return result, fmt.Errorf("CLI link installed; shell setup: %w", e)
		}
		if changed {
			result.Profiles = append(result.Profiles, profile)
		}
	}
	return result, nil
}

func onPath(dir, path string) bool {
	for _, entry := range filepath.SplitList(path) {
		if filepath.Clean(entry) == dir {
			return true
		}
	}
	return false
}

func linkCommand(executable, dir string) error {
	if err := commandDirectory(dir); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("command directory must be a real directory not writable by other users: %s", dir)
	}
	command := filepath.Join(dir, "sluicework")
	if err = os.Symlink(executable, command); err == nil {
		return nil
	}
	if target, e := filepath.EvalSymlinks(command); e == nil && target == executable {
		return nil
	}
	return fmt.Errorf("existing command preserved at %s; use %s directly or remove that command yourself before retrying", command, executable)
}

func addPath(path, block string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return false, err
	}
	// O_NOFOLLOW prevents modifying a symlinked startup file (e.g. a dotfiles repo).
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return false, fmt.Errorf("startup file preserved at %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() > 1<<20 {
		return false, fmt.Errorf("startup file preserved at %s: expected a private regular file no larger than 1 MiB", path)
	}
	data := make([]byte, info.Size())
	if _, err = f.ReadAt(data, 0); err != nil && len(data) != 0 {
		return false, err
	}
	if strings.Contains(string(data), block) {
		return false, nil
	}
	if strings.Contains(string(data), "# >>> Sluicework CLI >>>") {
		return false, fmt.Errorf("existing modified Sluicework PATH block preserved at %s", path)
	}
	prefix := ""
	if len(data) != 0 && data[len(data)-1] != '\n' {
		prefix = "\n"
	}
	if _, err = f.WriteString(prefix + "\n" + block); err != nil {
		return false, err
	}
	return true, f.Sync()
}

// Explicit modes keep a system command traversable even under the CLI's 0077
// umask. Existing directories keep their permissions.
func commandDirectory(dir string) error {
	if err := os.Mkdir(dir, 0755); err == nil {
		return os.Chmod(dir, 0755)
	} else if os.IsNotExist(err) {
		parent := filepath.Dir(dir)
		if parent == dir {
			return err
		}
		if err = commandDirectory(parent); err != nil {
			return err
		}
		return commandDirectory(dir)
	} else if os.IsExist(err) {
		info, statErr := os.Lstat(dir)
		if statErr != nil {
			return statErr
		}
		if !info.IsDir() {
			return fmt.Errorf("command directory is not a real directory: %s", dir)
		}
		return nil
	} else {
		return err
	}
}
