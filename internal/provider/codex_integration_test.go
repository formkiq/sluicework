package provider

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sluicework/internal/codexclient"
)

// Opt-in: use the installed CLI, a fake model and disposable state. No real
// inference, user credentials, home configuration or project files are used.
func TestInstalledCodexCompactsAfterExecutingSandboxedTool(t *testing.T) {
	binary := os.Getenv("SLUICEWORK_TEST_CODEX_BINARY")
	if binary == "" {
		t.Skip("set SLUICEWORK_TEST_CODEX_BINARY to run the actual Codex CLI smoke test")
	}
	t.Setenv("CODEX_HOME", t.TempDir())
	var turns atomic.Int32
	var compacted, retainedToolResult atomic.Bool
	s, _, scope, token := codingFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			t.Error(e)
			http.Error(w, "invalid fixture request", 400)
			return
		}
		format, structured := body["format"].(map[string]any)
		if !structured {
			compacted.Store(true)
			for _, item := range body["messages"].([]any) {
				m := item.(map[string]any)
				if m["role"] == "tool" && strings.Contains(fmt.Sprint(m["content"]), "sluicework_fixture") {
					retainedToolResult.Store(true)
				}
			}
			fmt.Fprintln(w, `{"message":{"content":"The fixture command printed sluicework_fixture successfully. Reply Completed fixture. No more commands are needed."},"done":true,"prompt_eval_count":50,"eval_count":20}`)
			return
		}
		if turns.Add(1) == 1 {
			properties := format["properties"].(map[string]any)
			calls := properties["tool_calls"].(map[string]any)
			items := calls["items"].(map[string]any)
			name := ""
			for _, alternative := range items["anyOf"].([]any) {
				p := alternative.(map[string]any)["properties"].(map[string]any)
				n := p["name"].(map[string]any)["const"].(string)
				if strings.HasSuffix(n, "exec_command") {
					name = n
					break
				}
			}
			if name == "" {
				t.Error("installed Codex did not offer its shell tool")
				http.Error(w, "missing fixture tool", 500)
				return
			}
			content, _ := json.Marshal(payload{"content": "", "tool_calls": []payload{{"name": name, "arguments": payload{"cmd": "printf sluicework_fixture", "max_output_tokens": 128}}}})
			// Exceed the launcher's 12,288-token auto-compaction threshold.
			response, _ := json.Marshal(payload{"message": payload{"content": string(content)}, "done": true, "prompt_eval_count": 13000, "eval_count": 20})
			fmt.Fprintln(w, string(response))
			return
		}
		content, _ := json.Marshal(payload{"content": "Completed fixture.", "tool_calls": []payload{}})
		response, _ := json.Marshal(payload{"message": payload{"content": string(content)}, "done": true, "prompt_eval_count": 50, "eval_count": 10})
		fmt.Fprintln(w, string(response))
	})
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized fixture", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" && r.URL.Path == "/api/v1/harness/v1/models" {
			fmt.Fprintln(w, `{"data":[{"id":"qwen"}]}`)
			return
		}
		var raw json.RawMessage
		if e := json.NewDecoder(r.Body).Decode(&raw); e != nil {
			http.Error(w, "invalid fixture", 400)
			return
		}
		if e := s.ServeHarnessResponse(w, r, scope, raw); e != nil {
			t.Error("gateway rejected actual Codex request:", e)
			http.Error(w, e.Error(), 400)
		}
	}))
	defer gateway.Close()
	bundle := codexclient.Bundle{Version: 1, ServerURL: gateway.URL, Token: token, Models: []string{"qwen"}, ContextLength: 16384, MaxOutputTokens: 2048,
		CAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: gateway.Certificate().Raw}))}
	fingerprint, _ := codexclient.Fingerprint(bundle.CAPEM)
	client, e := codexclient.New(bundle, fingerprint)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var output, diagnostic strings.Builder
	e = client.Run(ctx, codexclient.RunOptions{Binary: binary, Directory: t.TempDir(), Exec: true, Sandbox: "workspace-write",
		Prompt: "Run the fixture command, then report completion.", In: strings.NewReader(""), Out: &output, Err: &diagnostic})
	if e != nil || !compacted.Load() || !retainedToolResult.Load() || !strings.Contains(output.String(), "Completed fixture.") {
		t.Fatalf("actual CLI compaction: %v; compacted=%v retainedResult=%v\n%s\n%s", e, compacted.Load(), retainedToolResult.Load(), diagnostic.String(), output.String())
	}
}
