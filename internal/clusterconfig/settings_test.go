package clusterconfig

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"sluicework/internal/githubapp"
	"sluicework/internal/state"
)

func fixture(t *testing.T) (*Manager, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = (&state.Store{DB: db}).Init(); err != nil {
		t.Fatal(err)
	}
	m, err := Open(db, Seed{DataDir: dir, Listen: "127.0.0.1:7443"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m, db
}
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}
func TestNetworkSetupPreservesPrimaryAccessAndTrustAcrossRestart(t *testing.T) {
	m, db := fixture(t)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("cluster")) })
	primary := httptest.NewServer(handler)
	defer primary.Close()
	if err := m.StartNetwork(handler); err != nil {
		t.Fatal(err)
	}
	input := NetworkInput{Enabled: true, Hostname: "127.0.0.1", Port: freePort(t)}
	if err := m.SetNetwork("admin", input); err != nil {
		t.Fatal(err)
	}
	view := m.Snapshot()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(view.Network.CAPEM)) {
		t.Fatal("public CA missing")
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
	check := func(origin string) {
		t.Helper()
		r, err := client.Get(origin)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		if string(b) != "cluster" {
			t.Fatal("wrong cluster response")
		}
	}
	check(view.Network.URL)
	check(primary.URL)
	previous := view.Network.Fingerprint
	input.Hostname = "localhost"
	if err := m.SetNetwork("admin", input); err != nil {
		t.Fatal(err)
	}
	if got := m.Snapshot().Network.Fingerprint; got != previous {
		t.Fatal("hostname update silently rotated the CA")
	}
	check(m.Snapshot().Network.URL)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	failed := input
	failed.Port = occupied.Addr().(*net.TCPAddr).Port
	if err = m.SetNetwork("admin", failed); !errors.Is(err, state.ErrConflict) {
		t.Fatal("port conflict did not fail safely", err)
	}
	if m.Snapshot().Network.Port != input.Port {
		t.Fatal("failed update changed active configuration")
	}
	check(m.Snapshot().Network.URL)
	m.Close()
	restarted, err := Open(db, Seed{DataDir: m.dir, Listen: m.primary})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err = restarted.StartNetwork(handler); err != nil {
		t.Fatal(err)
	}
	if restarted.Snapshot().Network.Fingerprint != previous {
		t.Fatal("restart lost certificate trust")
	}
	check(restarted.Snapshot().Network.URL)
	if err = restarted.SetNetwork("admin", NetworkInput{}); err != nil {
		t.Fatal(err)
	}
	check(primary.URL)
	cfg := restarted.current()
	for _, name := range []string{cfg.Network.KeyFile, cfg.Network.CAKeyFile} {
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("network private key permissions", name, err)
		}
	}
}
func TestConfigurationPersistsWithoutExposingPrivateKeys(t *testing.T) {
	m, db := fixture(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	private := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	var broker *githubapp.Broker
	transition := func(_ bool, next *githubapp.Broker, commit func() error) error {
		if err := commit(); err != nil {
			return err
		}
		broker = next
		return nil
	}
	in := GitHubInput{Enabled: true, AppID: 123, InstallationID: 456, Repository: "team/repo", PrivateKeyPEM: private}
	if err = m.SetGitHub("admin", in, transition); err != nil || broker == nil {
		t.Fatal(err)
	}
	org := true
	in.OrganizationRunners = &org
	if err = m.SetGitHub("admin", in, transition); err != nil {
		t.Fatal(err)
	}
	in.OrganizationRunners = nil // An older client must preserve the explicit opt-in.
	if err = m.SetGitHub("admin", in, transition); err != nil || !m.Snapshot().GitHub.OrganizationRunners {
		t.Fatal("organization management opt-in erased", err)
	}
	groupID := int64(7)
	repositories := []string{"team/repo", "team/secondary"}
	in.WorkstationGroupID, in.WorkstationRepositories = &groupID, &repositories
	requiredDrain := false
	if err = m.SetGitHub("admin", in, func(idle bool, next *githubapp.Broker, commit func() error) error {
		requiredDrain = idle
		return transition(idle, next, commit)
	}); err != nil || !requiredDrain {
		t.Fatal("shared scope did not require drained registration", err)
	}
	in.WorkstationGroupID, in.WorkstationRepositories, in.PrivateKeyPEM = nil, nil, ""
	if err = m.SetGitHub("admin", in, transition); err != nil || m.Snapshot().GitHub.WorkstationGroupID != 7 || len(m.Snapshot().GitHub.WorkstationRepositories) != 2 {
		t.Fatal("older client erased shared scope", err)
	}
	raw, _ := json.Marshal(m.Snapshot())
	if bytes.Contains(raw, []byte("PRIVATE KEY")) || bytes.Contains(raw, []byte("privateKeyFile")) || bytes.Contains(raw, []byte("configuration-secrets")) {
		t.Fatal("secret information exposed")
	}
	if err = m.SetLogLimit("admin", 4<<20); err != nil {
		t.Fatal(err)
	}
	again, err := Open(db, Seed{DataDir: m.dir, Listen: m.primary})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.LogLimit() != 4<<20 || again.Snapshot().GitHub.Repository != "team/repo" {
		t.Fatal("settings lost after restart")
	}
	in.PrivateKeyPEM = "not a private key"
	if err = m.SetGitHub("admin", in, transition); !errors.Is(err, state.ErrInvalid) {
		t.Fatal("invalid key accepted", err)
	}
	if m.Snapshot().GitHub.Repository != "team/repo" {
		t.Fatal("invalid key changed configuration")
	}
	for _, limit := range []int{0, 65 << 20} {
		if err = m.SetLogLimit("admin", limit); !errors.Is(err, state.ErrInvalid) {
			t.Fatal("invalid output limit accepted", err)
		}
	}
}
func TestWorkerBundleContainsBootstrapAndPublicTrustOnly(t *testing.T) {
	m, _ := fixture(t)
	if err := m.StartNetwork(http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}
	if err := m.SetNetwork("admin", NetworkInput{Enabled: true, Hostname: "127.0.0.1", Port: freePort(t)}); err != nil {
		t.Fatal(err)
	}
	other := "amd64"
	if runtime.GOARCH == other {
		other = "arm64"
	}
	data, err := m.WorkerBundle("Second Mac", m.Snapshot().Network.URL, "darwin", other, "ci")
	if err != nil {
		t.Fatal(err)
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, entry := range archive.File {
		r, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name], err = io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(files) != 5 || files["sluicework"] != nil || files["cluster-ca.pem"] == nil || files["manage"] == nil {
		t.Fatal("wrong bundle contents")
	}
	for name, b := range files {
		if strings.HasSuffix(name, ".pem") && bytes.Contains(b, []byte("PRIVATE KEY")) {
			t.Fatal("private key in worker bundle")
		}
	}
	var config map[string]any
	if err = json.Unmarshal(files["bootstrap.json"], &config); err != nil {
		t.Fatal(err)
	}
	if config["caFingerprint"] != m.Snapshot().Network.Fingerprint || config["kind"] != "ci" || config["name"] != "Second Mac" {
		t.Fatal("wrong public bootstrap")
	}
	for _, line := range strings.Split(strings.TrimSpace(string(files["SHA256SUMS"])), "\n") {
		parts := strings.SplitN(line, "  ", 2)
		sum := sha256.Sum256(files[parts[1]])
		if parts[0] != hex.EncodeToString(sum[:]) {
			t.Fatal("incorrect setup checksum")
		}
	}
	for _, origin := range []string{"http://192.168.1.5:7443", "https://user:pass@host:7443", "https://host:7443/path", "https://host:99999"} {
		if _, err = m.WorkerBundle("Mac", origin, "darwin", other, "ci"); !errors.Is(err, state.ErrInvalid) {
			t.Fatal("unsafe origin accepted", origin, err)
		}
	}
	if _, err = m.WorkerBundle("Mac", "http://127.0.0.1:"+strconv.Itoa(17443), "darwin", other, "worker"); err != nil {
		t.Fatal("encrypted-tunnel bootstrap rejected", err)
	}
}

func TestCertificateRenewalPreservesTrustAndNeverEnablesDisabledNetwork(t *testing.T) {
	m, _ := fixture(t)
	if err := m.StartNetwork(http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}
	if err := m.SetNetwork("admin", NetworkInput{Enabled: true, Hostname: "127.0.0.1", Port: freePort(t)}); err != nil {
		t.Fatal(err)
	}
	cfg := m.current()
	previous := m.Snapshot().Network.Fingerprint
	parseKey := func(path string) *rsa.PrivateKey {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(b)
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	b, err := os.ReadFile(cfg.Network.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(b)
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := serial()
	leaf := &x509.Certificate{SerialNumber: id, Subject: pkix.Name{CommonName: "127.0.0.1"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &parseKey(cfg.Network.KeyFile).PublicKey, parseKey(cfg.Network.CAKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Network.CertFile, err = m.privateFile("expiring-test-", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	if err != nil {
		t.Fatal(err)
	}
	if err = m.store("fixture", cfg); err != nil {
		t.Fatal(err)
	}
	certificate, err := loadCertificate(cfg.Network)
	if err != nil {
		t.Fatal(err)
	}
	m.certificate.Store(&certificate)
	m.renewCertificateIfNeeded()
	after := m.Snapshot()
	expires, err := time.Parse(time.RFC3339, after.Network.CertificateExpires)
	if err != nil || time.Until(expires) < 300*24*time.Hour || after.Network.Fingerprint != previous {
		t.Fatal("renewal changed trust or failed to extend validity", after, err)
	}
	if err = m.SetNetwork("admin", NetworkInput{}); err != nil {
		t.Fatal(err)
	}
	m.renewCertificateIfNeeded()
	if m.Snapshot().Network.Enabled {
		t.Fatal("renewal re-enabled disabled capacity")
	}
}
