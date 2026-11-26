package clusterconfig

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"

	"sluicework/internal/state"
	"sluicework/packaging"
)

func (m *Manager) WorkerBundle(name, origin, operatingSystem, arch, kind string) ([]byte, error) {
	if kind == "" {
		kind = "ci"
	}
	if kind != "ci" && kind != "worker" {
		return nil, fmt.Errorf("%w: choose a workstation or script/AI worker", state.ErrInvalid)
	}
	if strings.TrimSpace(name) == "" || len(name) > 128 || strings.ContainsAny(name, "\r\n\x00") {
		return nil, fmt.Errorf("%w: provide a worker name", state.ErrInvalid)
	}
	if (operatingSystem != "darwin" && operatingSystem != "linux") || (arch != "arm64" && arch != "amd64") {
		return nil, fmt.Errorf("%w: choose macOS/Linux and ARM64/x86-64", state.ErrInvalid)
	}
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("%w: provide the cluster origin", state.ErrInvalid)
	}
	loopback := u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback()
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return nil, fmt.Errorf("%w: remote workers require HTTPS", state.ErrInvalid)
	}
	port, portErr := strconv.Atoi(u.Port())
	if portErr != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("%w: use an explicit cluster port", state.ErrInvalid)
	}
	files := map[string][]byte{"manage": packaging.WorkstationHelper}
	bootstrap := map[string]any{"format": 1, "url": strings.TrimSuffix(origin, "/"), "name": name, "os": operatingSystem, "arch": arch, "kind": kind}
	cfg := m.current()
	if cfg.Network.Enabled && origin == networkURL(cfg.Network) && cfg.Network.CAFile != "" {
		b, err := os.ReadFile(cfg.Network.CAFile)
		if err != nil {
			return nil, err
		}
		files["cluster-ca.pem"] = b
		bootstrap["caFingerprint"] = fingerprint(b)
	}
	includedBinary := operatingSystem == runtime.GOOS && arch == runtime.GOARCH
	if includedBinary {
		executable, err := os.Executable()
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(executable)
		if err != nil || info.Size() > 128<<20 {
			return nil, fmt.Errorf("%w: server binary cannot be bundled", state.ErrInvalid)
		}
		b, err := os.ReadFile(executable)
		if err != nil {
			return nil, err
		}
		files["sluicework"] = b
	}
	files["bootstrap.json"], err = json.MarshalIndent(bootstrap, "", "  ")
	if err != nil {
		return nil, err
	}
	binaryInstructions := "This bundle includes the matching Sluicework binary.\n"
	if !includedBinary {
		binaryInstructions = "Build the matching Sluicework installer following https://github.com/formkiq/sluicework#build-and-test and copy its sluicework binary into this directory before continuing. The server cannot supply a binary for another OS/CPU.\n"
	}
	prerequisites := "Install rustup and Python 3.12; macOS also needs Xcode Command Line Tools."
	after := "In the main admin, approve the GitHub runner and resource profile, then choose Offer capacity. Local manage enable/pause controls remain available."
	if kind == "worker" {
		prerequisites = "Python 3.9+ is required for this helper. Build-tool prerequisites and a GitHub runner are unnecessary for script/AI workers."
		after = "Configure labels, slots and approved jobs in the main admin."
	}
	files["README.txt"] = []byte(fmt.Sprintf("Sluicework worker setup: %s/%s\n\n%s\n1. Extract into a new directory owned by your normal login, without sudo.\n2. Verify SHA256SUMS (on macOS: shasum -a 256 -c SHA256SUMS).\n3. %s\n4. Run: chmod 700 ./sluicework ./manage, then python3 ./manage join\n5. Compare the CA fingerprint with the main admin, then paste its one-use enrollment token at the hidden prompt.\n6. %s\n\nNo server, private GitHub App key or separate worker admin is installed.\nUse manage start/status/pause/stop for this worker only; stopping it does not stop the main server.\n", operatingSystem, arch, binaryInstructions, prerequisites, after))
	var sums strings.Builder
	for _, name := range []string{"sluicework", "manage", "bootstrap.json", "cluster-ca.pem", "README.txt"} {
		if b, ok := files[name]; ok {
			sum := sha256.Sum256(b)
			fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), name)
		}
	}
	files["SHA256SUMS"] = []byte(sums.String())
	var out bytes.Buffer
	archive := zip.NewWriter(&out)
	for _, name := range []string{"sluicework", "manage", "bootstrap.json", "cluster-ca.pem", "README.txt", "SHA256SUMS"} {
		b, ok := files[name]
		if !ok {
			continue
		}
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0600)
		if name == "sluicework" || name == "manage" {
			header.SetMode(0700)
		}
		entry, err := archive.CreateHeader(header)
		if err != nil {
			return nil, err
		}
		if _, err = entry.Write(b); err != nil {
			return nil, err
		}
	}
	if err = archive.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
