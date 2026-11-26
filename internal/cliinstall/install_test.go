package cliinstall

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func fixture(t *testing.T) (string, string) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home with spaces")
	binary := filepath.Join(home, "Sluicework-pilot", "sluicework")
	if e := os.MkdirAll(filepath.Dir(binary), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(binary, []byte("#!/bin/sh\necho fixture\n"), 0700); e != nil {
		t.Fatal(e)
	}
	return home, binary
}
func TestUserShellSetupIsIdempotentAndWorksWithSpaces(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("user installation runs without sudo")
	}
	home, binary := fixture(t)
	profile := filepath.Join(home, ".zprofile")
	original := "# Existing shell configuration\nexport PRESERVED=yes"
	if e := os.WriteFile(profile, []byte(original), 0644); e != nil {
		t.Fatal(e)
	}
	options := Options{Executable: binary, Home: home, Shell: "/bin/zsh", Path: "/usr/bin:/bin"}
	first, e := Install(options)
	if e != nil {
		t.Fatal(e)
	}
	second, e := Install(options)
	if e != nil {
		t.Fatal(e)
	}
	if len(first.Profiles) != 2 || len(second.Profiles) != 0 {
		t.Fatal("shell setup was not idempotent", first, second)
	}
	data, _ := os.ReadFile(profile)
	if !strings.HasPrefix(string(data), original) || strings.Count(string(data), "# >>> Sluicework CLI >>>") != 1 {
		t.Fatal("existing profile changed or duplicate block added")
	}
	cmd := exec.Command("/bin/sh", "-c", `. "$HOME/.zprofile"; . "$HOME/.zshrc"; test "$PRESERVED" = yes; sluicework`)
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	output, e := cmd.CombinedOutput()
	if e != nil || strings.TrimSpace(string(output)) != "fixture" {
		t.Fatalf("new shell: %s %v", output, e)
	}
}
func TestExistingCommandsAndOtherInstallationsArePreserved(t *testing.T) {
	home, binary := fixture(t)
	dir := filepath.Join(home, ".local", "bin")
	if e := os.MkdirAll(dir, 0755); e != nil {
		t.Fatal(e)
	}
	command := filepath.Join(dir, "sluicework")
	if e := os.WriteFile(command, []byte("existing program"), 0755); e != nil {
		t.Fatal(e)
	}
	if e := linkCommand(binary, dir); e == nil {
		t.Fatal("existing executable replaced")
	}
	data, _ := os.ReadFile(command)
	if string(data) != "existing program" {
		t.Fatal("existing command changed")
	}
	if e := os.Remove(command); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink("/another/sluicework", command); e != nil {
		t.Fatal(e)
	}
	if e := linkCommand(binary, dir); e == nil {
		t.Fatal("another installation replaced")
	}
	target, _ := os.Readlink(command)
	if target != "/another/sluicework" {
		t.Fatal("other installation changed")
	}
}
func TestSymlinkedStartupFilesArePreserved(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "dotfiles")
	profile := filepath.Join(root, ".zprofile")
	if e := os.WriteFile(target, []byte("existing"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(target, profile); e != nil {
		t.Fatal(e)
	}
	if _, e := addPath(profile, pathBlock); e == nil {
		t.Fatal("symlinked startup file modified")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "existing" {
		t.Fatal("dotfiles target changed")
	}
}
func TestExistingPathNeedsNoStartupChanges(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("user installation runs without sudo")
	}
	home, binary := fixture(t)
	r, e := Install(Options{Executable: binary, Home: home, Shell: "/bin/bash", Path: filepath.Join(home, ".local", "bin") + ":/usr/bin"})
	if e != nil {
		t.Fatal(e)
	}
	if len(r.Profiles) != 0 {
		t.Fatal("unnecessary startup changes")
	}
	if _, e = os.Stat(filepath.Join(home, ".bashrc")); !os.IsNotExist(e) {
		t.Fatal("created unnecessary shell file")
	}
}
func TestZshUsesConfiguredDotDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("user installation runs without sudo")
	}
	home, binary := fixture(t)
	dot := filepath.Join(home, "zsh settings")
	r, e := Install(Options{Executable: binary, Home: home, Shell: "/bin/zsh", ZDotDir: dot, Path: "/usr/bin"})
	if e != nil {
		t.Fatal(e)
	}
	if len(r.Profiles) != 2 || filepath.Dir(r.Profiles[0]) != dot {
		t.Fatal("ZDOTDIR ignored")
	}
}

func TestNewCommandDirectoriesRemainTraversableUnderPrivateUmask(t *testing.T) {
	home, binary := fixture(t)
	dir := filepath.Join(home, "new", "bin")
	previous := syscall.Umask(0077)
	defer syscall.Umask(previous)
	if e := linkCommand(binary, dir); e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{filepath.Dir(dir), dir} {
		info, e := os.Stat(path)
		if e != nil {
			t.Fatal(e)
		}
		if info.Mode().Perm() != 0755 {
			t.Fatal("new command directory is inaccessible to other users", path, info.Mode())
		}
	}
}

func TestWritableCommandDirectoryIsPreservedAndRefused(t *testing.T) {
	home, binary := fixture(t)
	dir := filepath.Join(home, "unsafe-bin")
	if e := os.Mkdir(dir, 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(dir, 0777); e != nil {
		t.Fatal(e)
	}
	if e := linkCommand(binary, dir); e == nil {
		t.Fatal("installed a command into an unsafe directory")
	}
	info, e := os.Stat(dir)
	if e != nil || info.Mode().Perm() != 0777 {
		t.Fatal("existing directory permissions changed", e)
	}
	if _, e = os.Lstat(filepath.Join(dir, "sluicework")); !os.IsNotExist(e) {
		t.Fatal("created a shortcut despite unsafe permissions")
	}
}
