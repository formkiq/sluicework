package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestPlanWebSearchBodyAndDomainRestrictions(t *testing.T) {
	raw, err := planBody(Request{Model: "fixture", Prompt: "Research a public topic"})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err = json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body["tools"] != nil || body["tool_choice"] != nil {
		t.Fatal("text-only task acquired a browsing tool")
	}
	raw, err = planBody(Request{Model: "fixture", Prompt: "Research", WebSearch: &WebSearch{AllowedDomains: []string{"openai.com", "github.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(raw, &body)
	if body["tool_choice"] != "required" || body["store"] != false || body["stream"] != true {
		t.Fatal("wrong private search contract")
	}
	tools := body["tools"].([]any)
	tool := tools[0].(map[string]any)
	if len(tools) != 1 || tool["type"] != "web_search" || tool["filters"].(map[string]any)["allowed_domains"].([]any)[0] != "openai.com" {
		t.Fatal("approved domain restriction lost")
	}
	for _, domain := range []string{"https://openai.com", "openai.com/path", "*.openai.com", "localhost", "127.0.0.1", "cluster.local", "example.com:443", "UPPER.com", "a..com", "-a.com"} {
		if (WebSearch{AllowedDomains: []string{domain}}).Validate() == nil {
			t.Errorf("unsafe search domain accepted: %s", domain)
		}
	}
}
func TestSearchCitationsAndUnsupportedTools(t *testing.T) {
	stream := `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"web_search_call","status":"completed"},{"type":"message","content":[{"type":"output_text","text":"☀ News [1]","annotations":[{"type":"url_citation","url":"https://example.com/news","title":"News","start_index":7,"end_index":10},{"type":"url_citation","url":"javascript:alert(1)","start_index":0,"end_index":1},{"type":"url_citation","url":"https://example.com","start_index":0,"end_index":999}]},{"type":"output_text","text":" More [2]","annotations":[{"type":"url_citation","url":"https://example.org","title":"More","start_index":6,"end_index":9}]}]}]}}` + "\n\n"
	result, err := readPlanStream(strings.NewReader(stream))
	if err != nil || !result.searched || result.text != "☀ News [1] More [2]" || len(result.citations) != 2 || result.citations[1].Start != 16 {
		t.Fatalf("citation ranges/content lost: %+v %v", result, err)
	}
	for _, raw := range []string{"javascript:alert(1)", "file:///etc/passwd", "https://user:password@example.com", "http://127.0.0.1", "http://10.0.0.1", "http://localhost", "https://[::1]"} {
		if SafeCitationURL(raw) {
			t.Errorf("unsafe link accepted: %s", raw)
		}
	}
	for _, output := range []string{`{"type":"web_search_call","status":"failed"}`, `{"type":"function_call","name":"shell"}`, `{"type":"custom_tool_call"}`} {
		_, err := readPlanStream(strings.NewReader(`data: {"type":"response.completed","response":{"status":"completed","output":[` + output + `]}}` + "\n\n"))
		if err == nil {
			t.Fatal("failed search or unavailable tool reported success")
		}
	}
}
func TestPrivateSearchDoesNotSilentlyReturnUnresearchedAnswer(t *testing.T) {
	f := setupPlan(t)
	c := f.connect(t, "alice")
	r := planRequest(t, f.s, c, "alice")
	r.WebSearch = &WebSearch{}
	if _, err := f.s.Generate(context.Background(), r); err == nil || !strings.Contains(err.Error(), "did not complete") {
		t.Fatalf("missing search was treated as researched: %v", err)
	}
	f.stream = `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"web_search_call","status":"completed"},{"type":"message","content":[{"type":"output_text","text":"Researched report"}]}]}}` + "\n\n"
	r = planRequest(t, f.s, c, "alice")
	r.WebSearch = &WebSearch{}
	if result, err := f.s.Generate(context.Background(), r); err != nil || !result.WebSearched {
		t.Fatalf("approved web research failed: %+v %v", result, err)
	}
	r = planRequest(t, f.s, c, "alice")
	if _, err := f.s.Generate(context.Background(), r); err == nil {
		t.Fatal("unapproved search accepted")
	}
	f.s.personalOwner = ""
	r = planRequest(t, f.s, c, "alice")
	r.WebSearch = &WebSearch{}
	before := f.generations.Load()
	if _, err := f.s.Generate(context.Background(), r); err == nil || f.generations.Load() != before {
		t.Fatal("shared runtime allowed personal web research")
	}
}
