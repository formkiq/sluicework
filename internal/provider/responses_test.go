package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompactionReplaysHistoricalCallsWithoutOfferingTools(t *testing.T) {
	for _, tc := range []struct {
		name, kind, namespace, arguments string
	}{
		{"exec_command", "function_call", "", `{"cmd":"ls"}`},
		{"apply_patch", "custom_tool_call", "", "fixture patch"},
		{"read", "function_call", "files", `{"path":"README.md"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, client, _ := codingFixture(t, func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Tools    []payload `json:"tools"`
					Messages []payload `json:"messages"`
				}
				if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
					t.Error(e)
				}
				if len(body.Tools) != 0 {
					t.Error("compaction must not re-enable historical tools")
				}
				var history, output bool
				for _, message := range body.Messages {
					history = history || message["role"] == "assistant" && message["tool_calls"] != nil
					output = output || message["role"] == "tool" && message["content"] == "fixture result"
				}
				if !history || !output {
					t.Error("compaction lost the completed tool turn")
				}
				fmt.Fprintln(w, `{"message":{"content":"Compacted fixture context."},"done":true}`)
			})
			call := payload{"type": tc.kind, "name": tc.name, "namespace": tc.namespace, "call_id": "old-call"}
			outputType := "function_call_output"
			if tc.kind == "custom_tool_call" {
				call["input"] = tc.arguments
				outputType = "custom_tool_call_output"
			} else {
				call["arguments"] = tc.arguments
			}
			raw, _ := json.Marshal(payload{"model": "qwen", "tools": []payload{}, "input": []payload{
				{"role": "user", "content": "Inspect fixture"}, call,
				{"type": outputType, "call_id": "old-call", "output": "fixture result"},
				{"role": "user", "content": "Summarize the completed work for compaction"},
			}})
			parsed, e := parseCodingRequest(raw, client)
			if e != nil {
				t.Fatal(e)
			}
			if len(parsed.Tools) != 0 || len(parsed.Mappings) != 0 {
				t.Fatal("historical calls expanded the current tool inventory")
			}
			w := httptest.NewRecorder()
			if e := s.ServeHarnessResponse(w, httptest.NewRequest("POST", "/", nil), client, raw); e != nil {
				t.Fatal(e)
			}
			if !strings.Contains(w.Body.String(), "Compacted fixture context.") {
				t.Fatal("compaction failed to return its summary", w.Body.String())
			}
		})
	}
}

func TestHistoricalReplayDoesNotAuthorizeNewCallsToRemovedTools(t *testing.T) {
	for _, structured := range []bool{false, true} {
		t.Run(fmt.Sprint("structured=", structured), func(t *testing.T) {
			s, _, client, _ := codingFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if structured {
					content, _ := json.Marshal(payload{"content": "", "tool_calls": []payload{{"name": "exec_command", "arguments": payload{"cmd": "unapproved command"}}}})
					response, _ := json.Marshal(payload{"message": payload{"content": string(content)}, "done": true})
					fmt.Fprintln(w, string(response))
				} else {
					fmt.Fprintln(w, `{"message":{"tool_calls":[{"function":{"name":"exec_command","arguments":{"cmd":"unapproved command"}}}]},"done":true}`)
				}
			})
			tools := []payload{}
			if structured {
				tools = append(tools, payload{"type": "function", "name": "read_file", "parameters": payload{"type": "object"}})
			}
			raw, _ := json.Marshal(payload{"model": "qwen", "tools": tools, "input": []payload{
				{"type": "function_call", "name": "exec_command", "call_id": "old-call", "arguments": `{"cmd":"ls"}`},
				{"type": "function_call_output", "call_id": "old-call", "output": "fixture result"},
			}})
			parsed, e := parseCodingRequest(raw, client)
			if e != nil {
				t.Fatal(e)
			}
			if _, ok := parsed.Mappings["exec_command"]; ok || len(parsed.Tools) != len(tools) {
				t.Fatal("removed shell tool was re-enabled")
			}
			w := httptest.NewRecorder()
			e = s.ServeHarnessResponse(w, httptest.NewRequest("POST", "/", nil), client, raw)
			if e == nil || strings.Contains(w.Body.String(), `"type":"function_call"`) {
				t.Fatal("model emitted a removed tool call", e, w.Body.String())
			}
		})
	}
}

func TestMalformedHistoricalCallsRemainInvalid(t *testing.T) {
	for _, input := range []string{
		`[{"type":"function_call","call_id":"old","arguments":"{}"}]`,
		`[{"type":"function_call","name":"exec_command","call_id":"old","arguments":"not-json"}]`,
		`[{"type":"function_call","name":"exec_command","arguments":"{}"}]`,
		`[{"type":"function_call_output","call_id":"missing","output":"orphan"}]`,
	} {
		_, e := parseCodingRequest([]byte(`{"model":"qwen","input":`+input+`}`), HarnessClient{MaxOutputTokens: 512})
		if !errors.Is(e, ErrInvalid) {
			t.Fatal("malformed history accepted", input, e)
		}
	}
}
