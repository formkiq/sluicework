package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func codingFixture(t *testing.T, handler http.HandlerFunc) (*Service, Connection, HarnessClient, string) {
	t.Helper()
	s := setup(t)
	up := httptest.NewTLSServer(handler)
	t.Cleanup(up.Close)
	s.local = up.Client()
	conn, e := s.Create(context.Background(), Config{Name: "Remote model host", Kind: "ollama", Endpoint: up.URL, Models: []string{"qwen", "other"}, LocalOptions: LocalOptions{ContextLength: 4096}})
	if e != nil {
		t.Fatal(e)
	}
	c, token, e := s.CreateHarnessClient(context.Background(), "admin", HarnessClientInput{Name: "Separate developer machine", ConnectionID: conn.ID, Models: []string{"qwen"}, ContextLength: 16384, MaxOutputTokens: 2048, MaxConcurrent: 1, TimeoutSeconds: 15, LifetimeDays: 7})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SetHarnessPolicy(context.Background(), "admin", HarnessPolicy{true, 1}); e != nil {
		t.Fatal(e)
	}
	return s, conn, c, token
}
func codingRequestBody(model string, stream bool, extra string) []byte {
	return []byte(fmt.Sprintf(`{"model":%q,"stream":%t,"input":"private prompt"%s}`, model, stream, extra))
}
func TestHarnessScopesPrivateTokenAndRuntimePolicy(t *testing.T) {
	var calls atomic.Int32
	s, _, client, token := codingFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/chat" || r.Header.Get("Authorization") != "" {
			t.Error("client credential or endpoint escaped gateway")
		}
		var b struct {
			Options struct {
				Context, Output int `json:"-"`
			}
			KeepAlive string `json:"keep_alive"`
		}
		var raw map[string]any
		_ = json.NewDecoder(r.Body).Decode(&raw)
		_ = b
		o := raw["options"].(map[string]any)
		if o["num_ctx"] != float64(16384) || o["num_predict"] != float64(2048) || raw["keep_alive"] != "0s" {
			t.Errorf("server policy not enforced: %v", raw["options"])
		}
		fmt.Fprintln(w, `{"message":{"content":"answer"},"done":true,"prompt_eval_count":10,"eval_count":2}`)
	})
	if _, e := s.AuthenticateHarness(context.Background(), token); e != nil {
		t.Fatal(e)
	}
	for _, raw := range [][]byte{codingRequestBody("other", false, ""), codingRequestBody("qwen", false, `,"options":{"num_ctx":100000}`), codingRequestBody("qwen", false, `,"previous_response_id":"old"`)} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(string(raw)))
		if e := s.ServeHarnessResponse(httptest.NewRecorder(), r, client, raw); !errors.Is(e, ErrInvalid) {
			t.Fatalf("bad request accepted: %v", e)
		}
	}
	raw := codingRequestBody("qwen", false, `,"max_output_tokens":999999`)
	r := httptest.NewRequest("POST", "/", strings.NewReader(string(raw)))
	w := httptest.NewRecorder()
	if e := s.ServeHarnessResponse(w, r, client, raw); e != nil {
		t.Fatal(e)
	}
	if calls.Load() != 1 || !strings.Contains(w.Body.String(), `"status":"completed"`) {
		t.Fatal(w.Body.String())
	}
	audit, e := s.HarnessCalls(context.Background())
	if e != nil || len(audit) != 1 || audit[0].Status != "succeeded" {
		t.Fatalf("calls: %+v %v", audit, e)
	}
	data, _ := json.Marshal(audit)
	clients, _ := s.HarnessClients(context.Background())
	public, _ := json.Marshal(clients)
	if strings.Contains(string(data), "private prompt") || strings.Contains(string(public), token) {
		t.Fatal("private data leaked into metadata")
	}
	var digest []byte
	if e = s.db.QueryRow(`SELECT digest FROM harness_clients WHERE id=?`, client.ID).Scan(&digest); e != nil || len(digest) != 32 {
		t.Fatal("credential not stored as hash")
	}
	if e = s.RevokeHarnessClient(context.Background(), "admin", client.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.AuthenticateHarness(context.Background(), token); !errors.Is(e, ErrHarnessAuth) {
		t.Fatal("revoked credential accepted")
	}
}
func TestHarnessStreamsToolsAndReplaysClientResults(t *testing.T) {
	var calls atomic.Int32
	s, _, client, _ := codingFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if calls.Add(1) == 1 {
			content, _ := json.Marshal(map[string]any{"content": "", "tool_calls": []map[string]any{{"name": "exec_command", "arguments": map[string]string{"cmd": "cat README.md"}}}})
			response, _ := json.Marshal(map[string]any{"message": map[string]any{"content": string(content)}, "done": true, "prompt_eval_count": 50, "eval_count": 10})
			fmt.Fprintln(w, string(response))
		} else {
			messages := b["messages"].([]any)
			last := map[string]any{}
			for _, item := range messages {
				m := item.(map[string]any)
				if m["role"] == "tool" {
					last = m
				}
			}
			if last["role"] != "tool" || last["content"] != "fixture contents" || last["tool_name"] != "exec_command" {
				t.Errorf("bad replay: %+v", last)
			}
			content, _ := json.Marshal(map[string]any{"content": "It contains a fixture.", "tool_calls": []any{}})
			response, _ := json.Marshal(map[string]any{"message": map[string]any{"content": string(content)}, "done": false})
			fmt.Fprintln(w, string(response))
			fmt.Fprintln(w, `{"message":{},"done":true,"prompt_eval_count":70,"eval_count":6}`)
		}
	})
	tool := `{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}`
	raw := []byte(`{"model":"qwen","input":"inspect repository","tools":[` + tool + `]}`)
	w := httptest.NewRecorder()
	if e := s.ServeHarnessResponse(w, httptest.NewRequest("POST", "/", nil), client, raw); e != nil {
		t.Fatal(e)
	}
	var result struct{ Output []map[string]any }
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if len(result.Output) != 1 || result.Output[0]["type"] != "function_call" {
		t.Fatal(w.Body.String())
	}
	call := result.Output[0]
	body := map[string]any{"model": "qwen", "stream": true, "input": []any{map[string]any{"role": "user", "content": "inspect repository"}, call, map[string]any{"type": "function_call_output", "call_id": call["call_id"], "output": "fixture contents"}}, "tools": []json.RawMessage{json.RawMessage(tool)}}
	raw, _ = json.Marshal(body)
	w = httptest.NewRecorder()
	if e := s.ServeHarnessResponse(w, httptest.NewRequest("POST", "/", nil), client, raw); e != nil {
		t.Fatal(e)
	}
	for _, event := range []string{"response.created", "response.output_text.delta", "response.completed"} {
		if !strings.Contains(w.Body.String(), "event: "+event) {
			t.Fatal("missing " + event)
		}
	}
}
func TestHarnessCustomAndNamespaceTools(t *testing.T) {
	c := HarnessClient{MaxOutputTokens: 512}
	raw := []byte(`{"model":"qwen","input":[{"role":"developer","content":[{"type":"input_text","text":"follow instructions"}]},{"role":"user","content":"edit"}],"tools":[{"type":"custom","name":"apply_patch","description":"Apply a patch"},{"type":"namespace","name":"files","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]}]}`)
	request, e := parseCodingRequest(raw, c)
	if e != nil {
		t.Fatal(e)
	}
	if request.Messages[0]["role"] != "system" || request.Mappings["files__read"].Namespace != "files" {
		t.Fatal("tool namespace/developer role lost")
	}
	w := httptest.NewRecorder()
	events := newCodingEvents(w, false, "qwen")
	if e = events.tool(request.Mappings["apply_patch"], json.RawMessage(`{"input":"*** Begin Patch\n*** End Patch"}`)); e != nil {
		t.Fatal(e)
	}
	if e = events.finish(10, 5, ""); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(w.Body.String(), "custom_tool_call") {
		t.Fatal(w.Body.String())
	}
}
func TestHarnessRevocationCancelsActiveModelAndReleasesCapacity(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	s, _, client, _ := codingFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
			close(stopped)
		case <-release:
		}
	})
	done := make(chan error, 1)
	go func() {
		raw := codingRequestBody("qwen", true, "")
		done <- s.ServeHarnessResponse(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil), client, raw)
	}()
	<-started
	if e := s.RevokeHarnessClient(context.Background(), "admin", client.ID); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("revocation did not cancel")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request continued after revocation")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("upstream not cancelled")
	}
	calls, _ := s.HarnessCalls(context.Background())
	if len(calls) != 1 || calls[0].Status != "cancelled" {
		t.Fatalf("unexpected audit: %+v", calls)
	}
}
func TestScheduledAndCodingCallsShareAtomicAdmission(t *testing.T) {
	s := setup(t)
	ctx := context.Background()
	first := Call{AttemptID: "scheduled", Kind: "ollama", ConnectionID: "conn", Model: "qwen", Started: time.Now().UTC().Format(time.RFC3339Nano)}
	if e := s.reserveCall(ctx, first, "", 0); e != nil {
		t.Fatal(e)
	}
	wait, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	second := first
	second.AttemptID = "other-client"
	if e := s.reserveCall(wait, second, "", 0); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("slot cap bypassed: %v", e)
	}
	if e := s.finishHarness(first, time.Now(), nil, nil, nil); e != nil {
		t.Fatal(e)
	}
	if e := s.reserveCall(ctx, second, "", 0); e != nil {
		t.Fatal(e)
	}
}
func TestCodingBrokenStreamNeverReportsSuccess(t *testing.T) {
	s, _, client, _ := codingFixture(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"message":{"content":"partial"},"done":false}`)
	})
	w := httptest.NewRecorder()
	raw := codingRequestBody("qwen", true, "")
	if e := s.ServeHarnessResponse(w, httptest.NewRequest("POST", "/", nil), client, raw); e == nil {
		t.Fatal("truncated stream accepted")
	}
	if !strings.Contains(w.Body.String(), "response.failed") || strings.Contains(w.Body.String(), "response.completed") {
		t.Fatal(w.Body.String())
	}
	calls, _ := s.HarnessCalls(context.Background())
	if calls[0].Status != "failed" {
		t.Fatal(calls)
	}
}

func TestCodingExpiryAndCurrentModelPolicyAreEnforced(t *testing.T) {
	s, conn, client, token := codingFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("policy check reached model host") })
	ctx := context.Background()
	if _, e := s.db.Exec(`UPDATE harness_clients SET expires=? WHERE id=?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), client.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.AuthenticateHarness(ctx, token); !errors.Is(e, ErrHarnessAuth) {
		t.Fatal("expired credential accepted", e)
	}
	if _, e := s.db.Exec(`UPDATE provider_connections SET models='["other"]' WHERE id=?`, conn.ID); e != nil {
		t.Fatal(e)
	}
	models, e := s.HarnessModels(ctx, client)
	if e != nil || len(models) != 0 {
		t.Fatal("removed model still discoverable", models, e)
	}
	raw := codingRequestBody("qwen", false, "")
	if e = s.ServeHarnessResponse(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil), client, raw); e == nil {
		t.Fatal("removed model accepted")
	}
	if e = s.SetHarnessPolicy(ctx, "admin", HarnessPolicy{Enabled: false, MaxConcurrent: 1}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.HarnessModels(ctx, client); !errors.Is(e, ErrHarnessDisabled) {
		t.Fatal("paused gateway allowed discovery", e)
	}
}

func TestCodingPerClientLimitAppliesBelowSharedLimit(t *testing.T) {
	s, _, client, _ := codingFixture(t, func(http.ResponseWriter, *http.Request) {})
	ctx := context.Background()
	if e := s.SetHarnessPolicy(ctx, "admin", HarnessPolicy{Enabled: true, MaxConcurrent: 3}); e != nil {
		t.Fatal(e)
	}
	call := Call{AttemptID: "coding-first", Kind: "ollama", ConnectionID: client.ConnectionID, Model: "qwen", Started: time.Now().UTC().Format(time.RFC3339Nano)}
	if e := s.reserveCall(ctx, call, client.ID, 1); e != nil {
		t.Fatal(e)
	}
	wait, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	call.AttemptID = "coding-second"
	if e := s.reserveCall(wait, call, client.ID, 1); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("per-client limit bypassed", e)
	}
}

func TestCodingStreamTimeoutReportsActionableFailure(t *testing.T) {
	w := httptest.NewRecorder()
	events := newCodingEvents(w, true, "qwen")
	if e := events.start(); e != nil {
		t.Fatal(e)
	}
	events.fail(context.DeadlineExceeded)
	body := w.Body.String()
	if !strings.Contains(body, "model_timeout") || !strings.Contains(body, "request timeout") || !strings.Contains(body, "response.failed") || strings.Contains(body, "response.completed") {
		t.Fatal(body)
	}
}

func TestModelTimeoutAfterStreamingBeginsPreservesDeadlineReason(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	s, _, client, _ := codingFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		fmt.Fprintln(w, `{"message":{"content":"partial"},"done":false}`)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest("POST", "/", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	e := s.ServeHarnessResponse(w, r, client, codingRequestBody("qwen", true, ""))
	if !errors.Is(e, context.DeadlineExceeded) || !strings.Contains(w.Body.String(), "model_timeout") {
		t.Fatal("stream timeout lost its reason", e, w.Body.String())
	}
	calls, e := s.HarnessCalls(context.Background())
	if e != nil || len(calls) != 1 || calls[0].Error != "Request timed out" {
		t.Fatal("request history lost timeout reason", calls, e)
	}
}

func TestThirtyMinuteClientTimeoutCanBeEditedWithoutRotatingAccess(t *testing.T) {
	s, _, client, token := codingFixture(t, func(http.ResponseWriter, *http.Request) {})
	ctx := context.Background()
	if e := s.SetHarnessClientTimeout(ctx, "admin", client.ID, DefaultHarnessTimeoutSeconds); e != nil {
		t.Fatal(e)
	}
	current, e := s.AuthenticateHarness(ctx, token)
	if e != nil {
		t.Fatal(e)
	}
	if current.TimeoutSeconds != 1800 || current.Expires != client.Expires || current.ConnectionID != client.ConnectionID || current.ID != client.ID {
		t.Fatal("timeout update changed access", current)
	}
	if e = s.SetHarnessClientTimeout(ctx, "admin", client.ID, 1801); !errors.Is(e, ErrInvalid) {
		t.Fatal("timeout bound bypassed", e)
	}
	if e = s.RevokeHarnessClient(ctx, "admin", client.ID); e != nil {
		t.Fatal(e)
	}
	if e = s.SetHarnessClientTimeout(ctx, "admin", client.ID, 1800); !errors.Is(e, ErrNotFound) {
		t.Fatal("revoked access changed", e)
	}
}

func TestCodingUsesScopedDeadlineInsteadOfScheduledHTTPTimeout(t *testing.T) {
	s, _, client, _ := codingFixture(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(60 * time.Millisecond)
		fmt.Fprintln(w, `{"message":{"content":"finished"},"done":true}`)
	})
	s.local.Timeout = 10 * time.Millisecond
	raw := codingRequestBody("qwen", false, "")
	w := httptest.NewRecorder()
	if e := s.ServeHarnessResponse(w, httptest.NewRequest("POST", "/", nil), client, raw); e != nil {
		t.Fatal("scheduled HTTP timeout cut off coding request", e)
	}
	if !strings.Contains(w.Body.String(), "finished") {
		t.Fatal(w.Body.String())
	}
	if s.local.Timeout != 10*time.Millisecond {
		t.Fatal("scheduled AI HTTP policy was changed")
	}
}
