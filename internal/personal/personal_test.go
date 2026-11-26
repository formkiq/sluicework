package personal

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sluicework/internal/provider"
	"sluicework/internal/state"
)

func testManifest() Manifest {
	return Manifest{Name: "Morning report", Prompt: "Summarize the approved fixture", Model: "fixture-model", Cron: "* * * * *", Timezone: "UTC", Expires: time.Now().Add(24 * time.Hour).UTC(), MaxRunsPerDay: 2, TimeoutSeconds: 30, OutputLimit: 1024}
}
func testRegistry(t *testing.T) *Registry {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = (&state.Store{DB: db}).Init(); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRegistry(db)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func enrollTest(t *testing.T, r *Registry, owner string) (Runtime, ed25519.PrivateKey) {
	t.Helper()
	ticket, err := r.Ticket(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	input := Enrollment{ticket.ID, ticket.Code, "Fixture agent", base64.RawURLEncoding.EncodeToString(public), base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, EnrollmentMessage(ticket.ID, public)))}
	v, err := r.Enroll(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Enroll(context.Background(), input); err == nil {
		t.Fatal("replayed enrollment ticket accepted")
	}
	return v, key
}
func TestRegistryIsolationAndSignedReports(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	alice, key := enrollTest(t, r, "alice")
	bob, _ := enrollTest(t, r, "bob")
	if _, err := r.Save(ctx, "bob", "", alice.ID, testManifest()); err == nil {
		t.Fatal("other user attached a personal task")
	}
	p, err := r.Save(ctx, "alice", "", alice.ID, testManifest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Save(ctx, "bob", p.ID, bob.ID, testManifest()); err == nil {
		t.Fatal("other user changed a task")
	}
	list, _ := r.Proposals(ctx, "bob")
	if len(list) != 0 {
		t.Fatal("personal tasks visible to another user")
	}
	runtimeList, _ := r.Runtimes(ctx, "bob")
	if len(runtimeList) != 1 || runtimeList[0].ID != bob.ID {
		t.Fatal("cross-user runtime visibility")
	}
	payload, _ := json.Marshal(Report{Paused: true, Models: []string{"fixture-model"}, Approvals: []ApprovalStatus{}, Runs: []Run{}})
	nonce, _ := randomValue()
	e := Envelope{RuntimeID: alice.ID, Timestamp: time.Now().Unix(), Nonce: nonce, Payload: payload}
	e.Sign(key)
	got, err := r.Sync(ctx, e)
	if err != nil || len(got) != 1 || got[0].ID != p.ID {
		t.Fatalf("signed sync: %v %v", got, err)
	}
	if _, err = r.Sync(ctx, e); err == nil {
		t.Fatal("replayed signed report accepted")
	}
	e.Nonce, _ = randomValue()
	e.Payload = []byte(`{"paused":false}`)
	if _, err = r.Sync(ctx, e); err == nil {
		t.Fatal("tampered signed report accepted")
	}
	e.Sign(key)
	e.Timestamp = time.Now().Add(-time.Hour).Unix()
	e.Sign(key)
	if _, err = r.Sync(ctx, e); err == nil {
		t.Fatal("expired signed report accepted")
	}
	e.Timestamp = time.Now().Unix()
	e.Payload = []byte(`{"access_token":"must-never-reach-controller"}`)
	e.Nonce, _ = randomValue()
	e.Sign(key)
	if _, err = r.Sync(ctx, e); err == nil {
		t.Fatal("subscription credential upload accepted")
	}
}
func testAgent(t *testing.T) *Agent {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("personal runtime deliberately refuses root")
	}
	dir, err := os.MkdirTemp("", "swpa-")
	if err != nil {
		t.Fatal(err)
	}
	v, err := OpenVault(dir)
	if err != nil {
		t.Fatal(err)
	}
	key, err := v.Key()
	if err != nil {
		t.Fatal(err)
	}
	if err = v.SaveConfig(Config{Server: "https://cluster.example.test", Owner: "alice", RuntimeID: RuntimeID(key.Public().(ed25519.PublicKey)), UID: v.UID, Name: "Fixture"}); err != nil {
		t.Fatal(err)
	}
	a, err := OpenAgent(v)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); v.Close() })
	_, err = a.db.Exec(`INSERT INTO provider_connections(id,name,kind,endpoint,models,disabled,secret_ref) VALUES('fixture-account','Fixture account','chatgpt','https://api.openai.com','["fixture-model"]',0,''); INSERT INTO chatgpt_accounts(connection_id,owner,client_id,subject,email) VALUES('fixture-account','alice','fixture-client','fixture-subject','alice@example.test')`)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.set("connection", "fixture-account"); err != nil {
		t.Fatal(err)
	}
	return a
}
func propose(a *Agent) Proposal {
	return Proposal{ID: "fixture-proposal", Revision: 1, RuntimeID: a.Config.RuntimeID, Owner: a.Config.Owner, Manifest: testManifest()}
}
func TestOnlyLocalConsentActivatesImmutableSchedules(t *testing.T) {
	a := testAgent(t)
	ctx := context.Background()
	p := propose(a)
	a.inbox[p.ID] = p
	var calls atomic.Int32
	a.generate = func(ctx context.Context, r provider.Request) (provider.Result, error) {
		calls.Add(1)
		if r.Prompt != p.Manifest.Prompt {
			t.Error("remote edit changed approved prompt")
		}
		return provider.Result{Text: "private report"}, nil
	}
	now := time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
	a.boot = now.Add(-time.Minute)
	if err := a.Pause(false); err != nil {
		t.Fatal(err)
	}
	if err := a.Tick(ctx, now); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("unapproved proposal executed")
	}
	preview, err := a.Preview(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.Approve(p.ID, preview.Digest); err != nil {
		t.Fatal(err)
	}
	if err = a.Tick(ctx, now); err != nil {
		t.Fatal(err)
	}
	a.wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("approved schedule did not run")
	}
	if err = a.Tick(ctx, now); err != nil {
		t.Fatal(err)
	}
	a.wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("duplicate schedule slot ran")
	}
	changed := p
	changed.Revision++
	changed.Manifest.Prompt = "Unapproved remote instructions"
	changed.Manifest.ShareResults = true
	a.mu.Lock()
	a.inbox[p.ID] = changed
	a.mu.Unlock()
	if err = a.Tick(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	a.wg.Wait()
	if calls.Load() != 2 {
		t.Fatal("previously approved immutable task did not run")
	}
	if err = a.Tick(ctx, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	a.wg.Wait()
	if calls.Load() != 2 {
		t.Fatal("daily run limit bypassed")
	}
	report, err := a.Report()
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range report.Runs {
		if run.Result != "" {
			t.Fatal("unapproved sharing exposed local output")
		}
	}
	local, err := a.Results(10, false)
	if err != nil || len(local) != 2 || local[0].Result != "private report" {
		t.Fatal("private result history missing")
	}
	if err = a.Pause(true); err != nil {
		t.Fatal(err)
	}
	if err = a.Tick(ctx, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	a.wg.Wait()
	if calls.Load() != 2 {
		t.Fatal("paused agent ran")
	}
}
func TestApprovalRejectsChangedProposalOrAccount(t *testing.T) {
	a := testAgent(t)
	p := propose(a)
	a.inbox[p.ID] = p
	preview, err := a.Preview(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	changed := p
	changed.Manifest.Prompt = "Changed"
	a.inbox[p.ID] = changed
	if _, err = a.Approve(p.ID, preview.Digest); err == nil {
		t.Fatal("changed proposal approved without reviewing")
	}
	a.inbox[p.ID] = p
	_, err = a.db.Exec(`INSERT INTO provider_connections(id,name,kind,endpoint,models,disabled,secret_ref) VALUES('other-account','Other account','chatgpt','https://api.openai.com','["fixture-model"]',0,''); INSERT INTO chatgpt_accounts(connection_id,owner,client_id,subject) VALUES('other-account','alice','different-client','different-person')`)
	if err != nil {
		t.Fatal(err)
	}
	_ = a.set("connection", "other-account")
	if _, err = a.Approve(p.ID, preview.Digest); err == nil {
		t.Fatal("personal account changed after preview")
	}
}
func TestPrivateVaultAndKernelPeerIdentity(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("normal OS login required")
	}
	dir, err := os.MkdirTemp("", "swpa-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	v, err := OpenVault(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if _, err = OpenVault(dir); err == nil {
		t.Fatal("second agent obtained the private runtime lock")
	}
	if _, err = v.Key(); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Join(dir, "agent.key"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("private agent key has unsafe permissions")
	}
	path := filepath.Join(dir, "peer.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		c, err := listener.AcceptUnix()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		if verifyPeer(c, v.UID+1) == nil {
			done <- ErrInvalid
			return
		}
		done <- verifyPeer(c, v.UID)
	}()
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = <-done; err != nil {
		t.Fatal("kernel peer identity was not enforced:", err)
	}
	unsafe := filepath.Join(t.TempDir(), "unsafe")
	if err = os.Mkdir(unsafe, 0700); err != nil {
		t.Fatal(err)
	}
	// Mkdir applies the process umask; the workstation runner uses 0077.
	// Explicitly create the unsafe fixture mode regardless of that policy.
	if err = os.Chmod(unsafe, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenVault(unsafe); err == nil {
		t.Fatal("shared-readable runtime directory accepted")
	}
}
func TestManifestBoundsAndDSTSlots(t *testing.T) {
	m := testManifest()
	for _, mutate := range []func(*Manifest){func(v *Manifest) { v.Cron = "* * * * * *" }, func(v *Manifest) { v.Timezone = "Local" }, func(v *Manifest) { v.MaxRunsPerDay = 25 }, func(v *Manifest) { v.TimeoutSeconds = 86400 }, func(v *Manifest) { v.Expires = time.Now().Add(-time.Minute) }} {
		copy := m
		mutate(&copy)
		if copy.Validate(time.Now()) == nil {
			t.Fatal("unsafe background approval accepted")
		}
	}
	a := testAgent(t)
	p := propose(a)
	p.Manifest.Timezone = "America/Winnipeg"
	p.Manifest.Cron = "30 1 * * *"
	p.Manifest.Expires = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if p.Manifest.Expires.After(time.Now().Add(366 * 24 * time.Hour)) {
		t.Skip("fixture timezone date outside approval year")
	}
	a.inbox[p.ID] = p
	// Slot keys deliberately omit the UTC offset, preventing duplicate fall-back execution.
	loc, err := time.LoadLocation(p.Manifest.Timezone)
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2024, 11, 3, 6, 30, 0, 0, time.UTC)
	second := first.Add(time.Hour)
	if first.In(loc).Format("2006-01-02T15:04") != second.In(loc).Format("2006-01-02T15:04") {
		t.Fatal("fixture does not represent repeated DST slot")
	}
	if strings.Contains(p.Digest(a.Config.Server), p.Manifest.Prompt) {
		t.Fatal("approval digest contains source text")
	}
}

func TestPauseCancellationAndQuotaDoNotEscalateToOtherCredentials(t *testing.T) {
	a := testAgent(t)
	p := propose(a)
	a.inbox[p.ID] = p
	preview, err := a.Preview(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.Approve(p.ID, preview.Digest); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	var calls atomic.Int32
	a.generate = func(ctx context.Context, r provider.Request) (provider.Result, error) {
		calls.Add(1)
		if r.ConnectionID != "fixture-account" {
			t.Error("changed credential route")
		}
		close(started)
		<-ctx.Done()
		return provider.Result{}, ctx.Err()
	}
	now := time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
	a.boot = now.Add(-time.Minute)
	_ = a.Pause(false)
	if err = a.Tick(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("fixture inference did not start")
	}
	if err = a.Pause(true); err != nil {
		t.Fatal(err)
	}
	a.wg.Wait()
	if err = a.Tick(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("paused agent used credentials")
	}
	a.generate = func(ctx context.Context, r provider.Request) (provider.Result, error) {
		calls.Add(1)
		return provider.Result{}, fmt.Errorf("ChatGPT plan usage limit reached; no API-key fallback")
	}
	_ = a.Pause(false)
	if err = a.Tick(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	a.wg.Wait()
	if a.setting("paused") != "true" || calls.Load() != 2 {
		t.Fatal("quota failure did not pause without fallback")
	}
	// Pauses and exact local approvals survive process restart.
	a.Close()
	reopened, err := OpenAgent(a.vault)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	approvals, err := reopened.Approvals()
	if err != nil || len(approvals) != 1 || reopened.setting("paused") != "true" {
		t.Fatal("local consent/pause did not survive restart")
	}
}

func TestLoginServicesRemainUserScoped(t *testing.T) {
	id := strings.Repeat("a", 64)
	path, xml, label, err := serviceDefinition("darwin", "/Users/fixture", "/Users/fixture/Apps/Sluicework & Tools/sluicework", "/Users/fixture/private", id, 501)
	if err != nil || !strings.Contains(path, "/Library/LaunchAgents/") || strings.Contains(path, "LaunchDaemons") || !strings.Contains(xml, "&amp;") || !strings.Contains(label, id[:16]) {
		t.Fatal("unsafe macOS login service")
	}
	path, unit, _, err := serviceDefinition("linux", "/home/fixture", "/home/fixture/bin/sluicework", "/home/fixture/private%agent", id, 1001)
	if err != nil || !strings.Contains(path, "/.config/systemd/user/") || !strings.Contains(unit, "private%%agent") || strings.Contains(unit, "User=root") || !strings.Contains(unit, "NoNewPrivileges=true") {
		t.Fatal("unsafe Linux user service")
	}
	if _, _, _, err = serviceDefinition("darwin", "/Users/fixture", "/bin/test", "/tmp/private", "../../untrusted/name", 501); err == nil {
		t.Fatal("untrusted runtime label accepted")
	}
}

type fixtureTransport func(*http.Request) (*http.Response, error)

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestOwnerOnlyLocalControlsAndGracefulStop(t *testing.T) {
	a := testAgent(t)
	a.network.HTTP = &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("[]")), Header: http.Header{}}, nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(a.vault.Dir, "control.sock")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("local control socket was not created")
		}
		time.Sleep(time.Millisecond * 10)
	}
	info, _ := os.Stat(filepath.Join(a.vault.Dir, "control.sock"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("control socket is not private")
	}
	if err := LocalCall(context.Background(), a.vault.Dir, Control{Operation: "resume"}, nil); err != nil {
		t.Fatal(err)
	}
	if a.setting("paused") != "false" {
		t.Fatal("owner resume failed")
	}
	if err := LocalCall(context.Background(), a.vault.Dir, Control{Operation: "stop"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("owner stop did not stop all agent loops")
	}
	if a.setting("paused") != "true" {
		t.Fatal("owner stop did not preserve a pause")
	}
}

func TestNetworkUsesVerifiedTLSAndNeverFollowsEnrollmentRedirect(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var delivered, forwarded atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1); w.WriteHeader(200) }))
	defer trap.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delivered.Add(1)
		w.Header().Set("Location", trap.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	untrusted, err := NewNetwork(server.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = untrusted.Enroll(context.Background(), key, "fixture-ticket", "fixture-code", "Fixture"); err == nil {
		t.Fatal("untrusted TLS accepted")
	}
	if delivered.Load() != 0 {
		t.Fatal("enrollment code sent before certificate validation")
	}
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	trusted, err := NewNetwork(server.URL, ca)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = trusted.Enroll(context.Background(), key, "fixture-ticket", "fixture-code", "Fixture"); err == nil {
		t.Fatal("redirected enrollment accepted")
	}
	if delivered.Load() != 1 || forwarded.Load() != 0 {
		t.Fatal("enrollment capability forwarded through a redirect")
	}
	if RuntimeID(public) == "" {
		t.Fatal("missing public identity")
	}
	for _, origin := range []string{"http://192.168.1.2:7443", "https://user:secret@example.test", "https://example.test/?code=secret", "file:///private/config"} {
		if _, err := NewNetwork(origin, ""); err == nil {
			t.Fatal("unsafe cluster origin accepted")
		}
	}
}
