package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

const runnerVersion = "2.338.2"

var runnerHashes = map[string]string{
	"darwin/arm64": "25a065af409ffa317ab4d5a3b614dd15fc9c40f2172bc1b35a1b1258e919a388",
	"darwin/amd64": "109fff505a34014a7e8d1acb850c33d404811102f572b8fb9788e2fff013cc49",
	"linux/arm64":  "670adb2f268fe21dcbb58b3db0e0aa9d5b3bddfe2c651481336e561ff86de945",
	"linux/amd64":  "baebf204307bac6193480852732f0aba2ae97b6c5597c244f4d5558390147c40",
}

func installRunner(ctx context.Context, dir string) error {
	hash := runnerHashes[runtime.GOOS+"/"+runtime.GOARCH]
	if hash == "" {
		return errors.New("unsupported runner platform")
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		return errors.New("runnerDir already exists or is inaccessible; choose a new directory")
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(dir), ".runner-install-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	osname, arch := runtime.GOOS, runtime.GOARCH
	if osname == "darwin" {
		osname = "osx"
	}
	if arch == "amd64" {
		arch = "x64"
	}
	name := fmt.Sprintf("actions-runner-%s-%s-%s.tar.gz", osname, arch, runnerVersion)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://github.com/actions/runner/releases/download/v"+runnerVersion+"/"+name, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if r.URL.Scheme != "https" || len(via) > 5 {
			return errors.New("invalid download redirect")
		}
		return nil
	}}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("runner download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("runner download HTTP %d", resp.StatusCode)
	}
	archive, err := os.CreateTemp(stage, "archive-*.tar.gz")
	if err != nil {
		return err
	}
	defer archive.Close()
	digest := sha256.New()
	n, err := io.Copy(io.MultiWriter(archive, digest), io.LimitReader(resp.Body, 500<<20))
	if err != nil {
		return err
	}
	if n >= 500<<20 || hex.EncodeToString(digest.Sum(nil)) != hash {
		return errors.New("runner archive SHA256 mismatch")
	}
	if err = archive.Close(); err != nil {
		return err
	}
	// Only the pinned, checksum-verified official archive reaches tar.
	if err = exec.CommandContext(ctx, "/usr/bin/tar", "-xzf", archive.Name(), "-C", stage).Run(); err != nil {
		return errors.New("runner archive extraction failed")
	}
	if err = os.Remove(archive.Name()); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(stage, ".sluicework-runner"), []byte(runnerVersion+"\n"), 0600); err != nil {
		return err
	}
	return os.Rename(stage, dir)
}
