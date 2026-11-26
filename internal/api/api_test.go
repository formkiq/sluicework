package api_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"sluicework/internal/api"
	"sluicework/internal/auth"
	"sluicework/internal/provider"
	"sluicework/internal/schedule"
	"sluicework/internal/state"
)

// Exercise the wire contract with real opaque sessions, worker tokens and SQLite.
type cluster struct {
	t               *testing.T
	api             *api.Server
	http            *httptest.Server
	admin, operator identity
}
type identity struct {
	cookie *http.Cookie
	bearer string
}

func setup(t *testing.T) *cluster {
	t.Helper()
	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := &state.Store{DB: db}
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	users, err := auth.New(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"admin", "operator"} {
		if err := users.SetUser(role, role, "integration-test-password", false); err != nil {
			t.Fatal(err)
		}
	}
	providers, err := provider.Open(db, filepath.Join(dir, "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(providers.Close)
	schedules := schedule.New(db, store)
	if err := schedules.Init(); err != nil {
		t.Fatal(err)
	}
	server := &api.Server{State: store, Auth: users, Provider: providers, Schedule: schedules}
	h := httptest.NewTLSServer(server.Handler(http.NotFoundHandler()))
	t.Cleanup(h.Close)
	c := &cluster{t: t, http: h, api: server}
	c.admin = c.login("admin")
	c.operator = c.login("operator")
	return c
}
func (c *cluster) request(method, path string, data any, id identity, headers map[string]string) (*http.Response, error) {
	var body io.Reader
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.http.URL+"/api/v1"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if id.cookie != nil {
		req.AddCookie(id.cookie)
	}
	if id.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+id.bearer)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return c.http.Client().Do(req)
}
func (c *cluster) do(method, path string, data any, id identity, code int, out any) []byte {
	c.t.Helper()
	res, err := c.request(method, path, data, id, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	if res.StatusCode != code {
		c.t.Fatalf("%s %s: status %d, expected %d, body %s", method, path, res.StatusCode, code, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			c.t.Fatal(err)
		}
	}
	return b
}
func (c *cluster) login(name string) identity {
	c.t.Helper()
	res, err := c.request("POST", "/login", map[string]string{"username": name, "password": "integration-test-password"}, identity{}, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		c.t.Fatalf("login returned %d", res.StatusCode)
	}
	cookies := res.Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteStrictMode {
		c.t.Fatal("session cookie attributes")
	}
	return identity{cookie: cookies[0]}
}
func (c *cluster) enroll(name string) identity {
	c.t.Helper()
	var token struct{ Token string }
	c.do("POST", "/enrollment-tokens", nil, c.admin, 200, &token)
	info := map[string]any{"token": token.Token, "info": state.WorkerInfo{Name: name, Roles: []string{"script"}, Version: "test"}}
	var worker state.Enrollment
	c.do("POST", "/worker/enroll", info, identity{}, 200, &worker)
	c.do("POST", "/worker/enroll", info, identity{}, 401, nil)
	return identity{bearer: worker.Credential}
}
func (c *cluster) createRun() state.Run {
	c.t.Helper()
	j := state.Job{Name: "Print", Type: "script", Command: "/bin/echo", Args: []string{"ok"}}
	c.do("POST", "/jobs", j, c.admin, 200, &j)
	var r state.Run
	c.do("POST", "/jobs/"+j.ID+"/runs", map[string]any{"inputs": map[string]string{}}, c.operator, 200, &r)
	return r
}
func TestPermissionAndCredentialBoundaries(t *testing.T) {
	c := setup(t)
	worker := c.enroll("ci")
	for _, r := range []struct{ method, path string }{
		{"POST", "/enrollment-tokens"}, {"POST", "/jobs"}, {"POST", "/connections"}, {"POST", "/machines/any/revoke"},
	} {
		c.do(r.method, r.path, nil, c.operator, 403, nil)
	}
	var machines []state.Machine
	c.do("GET", "/machines", nil, c.operator, 200, &machines)
	path := "/machines/" + machines[0].ID
	var drained state.Machine
	c.do("PATCH", path, map[string]bool{"draining": true}, c.operator, 200, &drained)
	if !drained.Draining {
		t.Fatal("operator drain was not applied")
	}
	c.do("PATCH", path, map[string]any{"draining": false, "labels": []string{"ci"}}, c.operator, 403, nil)
	c.do("GET", "/machines", nil, c.operator, 200, &machines)
	if !machines[0].Draining || machines[0].Slots != 1 {
		t.Fatal("forbidden mixed patch changed machine")
	}
	c.do("PATCH", path, map[string]any{"slots": 2, "roleLimits": map[string]int{"script": 1}}, c.operator, 200, &drained)
	if drained.Slots != 2 || drained.RoleLimits["script"] != 1 {
		t.Fatal("operator capacity change was not applied")
	}
	c.do("GET", "/machines", nil, worker, 401, nil)
	c.do("POST", "/worker/claim", nil, c.admin, 401, nil)
	c.do("GET", "/connections", nil, identity{}, 401, nil)
	key := "example-private-provider-key"
	b := c.do("POST", "/connections", map[string]any{"name": "Project", "kind": "openai", "models": []string{"model"}, "api_key": key}, c.admin, 200, nil)
	if bytes.Contains(b, []byte(key)) || bytes.Contains(c.do("GET", "/connections", nil, c.admin, 200, nil), []byte(key)) {
		t.Fatal("public connection payload exposed a credential")
	}
	res, err := c.request("POST", "/enrollment-tokens", nil, c.admin, map[string]string{"Origin": "https://different.example"})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatalf("cross-origin write returned %d", res.StatusCode)
	}
	c.do("POST", "/logout", nil, c.admin, 204, nil)
	c.do("GET", "/session", nil, c.admin, 401, nil)
}

func TestWorkstationRunnerEndpointRequiresWorkerAuthentication(t *testing.T) {
	c := setup(t)
	body := map[string]string{"operation": "acquire"}
	c.do("POST", "/worker/runner", body, identity{}, 401, nil)
	c.do("POST", "/worker/runner", body, c.admin, 401, nil)
	worker := c.enroll("test-ci")
	c.do("POST", "/worker/runner", body, worker, 503, nil)
}
func TestStreamResumeAndCancellation(t *testing.T) {
	c := setup(t)
	owner := c.enroll("one")
	other := c.enroll("two")
	r := c.createRun()
	path := "/runs/" + r.ID
	var claim state.Claim
	c.do("POST", "/worker/claim", nil, owner, 200, &claim)
	if claim.RunID != r.ID {
		t.Fatal("worker claimed a different run")
	}
	attempt := "/worker/attempts/" + claim.AttemptID
	c.do("POST", "/worker/claim", nil, other, 204, nil)
	c.do("POST", attempt+"/renew", nil, other, 409, nil)
	c.do("POST", attempt+"/logs", map[string]string{"text": "foreign"}, other, 409, nil)
	c.do("POST", attempt+"/complete", state.Completion{State: "succeeded"}, other, 409, nil)
	c.do("POST", attempt+"/infer", map[string]any{}, owner, 403, nil)
	c.do("POST", attempt+"/logs", map[string]any{"text": "first line", "sequence": 1}, owner, 204, nil)
	res, err := c.request("GET", path+"/events", nil, c.operator, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Header.Get("Content-Type") != "text/event-stream" {
		res.Body.Close()
		t.Fatal("missing stream response")
	}
	// Disconnect after the first delivered event, before the second log is recorded.
	reader := bufio.NewReader(res.Body)
	var id int64
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			res.Body.Close()
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "id: ") {
			id, err = strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "id: ")), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
		}
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "first line") {
			break
		}
	}
	res.Body.Close()
	c.do("POST", attempt+"/logs", map[string]any{"text": "first line", "sequence": 1}, owner, 204, nil)
	if id <= 0 {
		t.Fatal("first event missing cursor")
	}
	c.do("POST", attempt+"/logs", map[string]any{"text": "second line", "sequence": 2}, owner, 204, nil)
	c.do("POST", path+"/cancel", nil, c.operator, 204, nil)
	var pending state.Run
	c.do("GET", path, nil, c.operator, 200, &pending)
	if pending.State != "running" || !pending.Attempts[0].CancelRequested {
		t.Fatal("cancellation not pending")
	}
	var renew state.Renewal
	c.do("POST", attempt+"/renew", nil, owner, 200, &renew)
	if !renew.CancelRequested {
		t.Fatal("worker missed cancellation")
	}
	c.do("POST", attempt+"/complete", state.Completion{State: "cancelled"}, owner, 204, nil)
	res, err = c.request("GET", path+"/events", nil, c.operator, map[string]string{"Last-Event-ID": fmt.Sprint(id)})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stream, []byte("second line")) || bytes.Contains(stream, []byte("first line")) || !bytes.Contains(stream, []byte(`"state":"cancelled"`)) {
		t.Fatalf("unexpected resumed stream: %s", stream)
	}
	var stored []state.Log
	c.do("GET", path+"/logs?after="+fmt.Sprint(id), nil, c.operator, 200, &stored)
	if len(stored) != 1 || stored[0].Text != "second line" {
		t.Fatal("stored cursor and stream diverged")
	}
}
func TestInterruptedLeaseAndRetryOverHTTP(t *testing.T) {
	c := setup(t)
	c.api.State.LeaseTTL = 100 * time.Millisecond
	first, second := c.enroll("primary"), c.enroll("replacement")
	r := c.createRun()
	var claim state.Claim
	c.do("POST", "/worker/claim", nil, first, 200, &claim)
	c.do("POST", "/runs/"+r.ID+"/cancel", nil, c.operator, 204, nil)
	time.Sleep(time.Until(claim.LeaseUntil) + time.Millisecond)
	c.do("POST", "/worker/claim", nil, second, 204, nil)
	c.do("POST", "/worker/attempts/"+claim.AttemptID+"/complete", state.Completion{State: "cancelled"}, first, 409, nil)
	var retried state.Run
	c.do("POST", "/runs/"+r.ID+"/retry", nil, c.operator, 200, &retried)
	if len(retried.Attempts) != 2 || retried.Attempts[0].State != "interrupted" || !retried.Attempts[0].OutcomeUnknown || retried.Attempts[1].State != "queued" {
		t.Fatalf("retry lost history: %+v", retried.Attempts)
	}
	var replacement state.Claim
	c.do("POST", "/worker/claim", nil, second, 200, &replacement)
	if replacement.RunID != r.ID || claim.AttemptID == replacement.AttemptID {
		t.Fatal("unexpected replacement identity")
	}
}
