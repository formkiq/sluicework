package provider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

type codingRequest struct {
	Model       string
	Stream      bool
	OutputLimit int
	Messages    []payload
	Tools       []payload
	Mappings    map[string]codingTool
}
type codingTool struct{ Type, Name, Namespace string }

var codingToolName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func codingToolAlias(name, namespace string) string {
	alias := name
	if namespace != "" {
		alias = namespace + "__" + name
	}
	if !codingToolName.MatchString(alias) {
		d := sha256.Sum256([]byte(namespace + "\x00" + name))
		alias = "swtool_" + hex.EncodeToString(d[:12])
	}
	return alias
}

func parseCodingRequest(raw []byte, c HarnessClient) (codingRequest, error) {
	var out codingRequest
	var body map[string]json.RawMessage
	invalid := func(msg string) (codingRequest, error) { return out, fmt.Errorf("%w: %s", ErrInvalid, msg) }
	if e := json.Unmarshal(raw, &body); e != nil || body == nil {
		return invalid("expected a Responses JSON object")
	}
	if json.Unmarshal(body["model"], &out.Model) != nil || out.Model == "" {
		return invalid("model is required")
	}
	if b, ok := body["stream"]; ok && json.Unmarshal(b, &out.Stream) != nil {
		return invalid("stream must be a boolean")
	}
	for _, key := range []string{"previous_response_id", "conversation", "background"} {
		if b := body[key]; len(b) > 0 && string(b) != "null" && string(b) != "false" && string(b) != "\"\"" {
			return invalid("send complete stateless input; " + key + " is unsupported")
		}
	}
	// Reject routing/resource overrides rather than silently accepting them.
	for _, key := range []string{"options", "keep_alive", "base_url", "endpoint", "extra_body"} {
		if _, ok := body[key]; ok {
			return invalid("runtime settings are controlled by Sluicework")
		}
	}
	out.OutputLimit = c.MaxOutputTokens
	if b, ok := body["max_output_tokens"]; ok && string(b) != "null" {
		var n int
		if json.Unmarshal(b, &n) != nil || n < 1 {
			return invalid("invalid max_output_tokens")
		}
		if n < out.OutputLimit {
			out.OutputLimit = n
		}
	}
	out.Mappings = map[string]codingTool{}
	out.Tools = []payload{}
	var tools []json.RawMessage
	if b := body["tools"]; len(b) > 0 && json.Unmarshal(b, &tools) != nil {
		return invalid("invalid tool definitions")
	}
	var addTools func([]json.RawMessage, string) error
	addTools = func(list []json.RawMessage, ns string) error {
		for _, b := range list {
			var t struct {
				Type, Name, Description string
				Parameters              json.RawMessage
				Tools                   []json.RawMessage
			}
			if json.Unmarshal(b, &t) != nil {
				return ErrInvalid
			}
			if t.Type == "namespace" {
				if ns != "" || t.Name == "" {
					return ErrInvalid
				}
				if e := addTools(t.Tools, t.Name); e != nil {
					return e
				}
				continue
			}
			if t.Type != "function" && t.Type != "custom" {
				return fmt.Errorf("%w: only client-executed function/custom tools are supported", ErrInvalid)
			}
			if t.Name == "" || len(t.Name) > 128 {
				return ErrInvalid
			}
			name := codingToolAlias(t.Name, ns)
			if _, ok := out.Mappings[name]; ok {
				return fmt.Errorf("%w: duplicate tool name", ErrInvalid)
			}
			if len(out.Mappings) >= 128 {
				return fmt.Errorf("%w: too many tool definitions", ErrInvalid)
			}
			out.Mappings[name] = codingTool{t.Type, t.Name, ns}
			var parameters any
			if t.Type == "custom" {
				parameters = payload{"type": "object", "properties": payload{"input": payload{"type": "string", "description": "The complete tool input, verbatim."}}, "required": []string{"input"}, "additionalProperties": false}
			} else if json.Unmarshal(t.Parameters, &parameters) != nil {
				return ErrInvalid
			}
			out.Tools = append(out.Tools, payload{"type": "function", "function": payload{"name": name, "description": t.Name + ": " + t.Description, "parameters": parameters}})
		}
		return nil
	}
	if e := addTools(tools, ""); e != nil {
		return out, e
	}
	alias := func(name, ns string) (string, error) {
		if name == "" || len(name) > 128 {
			return "", fmt.Errorf("%w: invalid historical tool name", ErrInvalid)
		}
		for n, t := range out.Mappings {
			if t.Name == name && t.Namespace == ns {
				return n, nil
			}
		}
		// Completed calls are history, not permission to execute a new tool.
		// Codex's local compaction intentionally omits tools while replaying
		// those calls/results. Keep their identities without adding definitions
		// or mappings: runCodingResponse still rejects new unoffered calls.
		return codingToolAlias(name, ns), nil
	}
	if b := body["instructions"]; len(b) > 0 && string(b) != "null" {
		var text string
		if json.Unmarshal(b, &text) != nil {
			return invalid("invalid instructions")
		}
		out.Messages = append(out.Messages, payload{"role": "system", "content": text})
	}
	input := body["input"]
	var text string
	if json.Unmarshal(input, &text) == nil {
		out.Messages = append(out.Messages, payload{"role": "user", "content": text})
		return out, nil
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(input, &items) != nil || len(items) == 0 {
		return invalid("input must contain messages")
	}
	calls := map[string]string{}
	for _, item := range items {
		str := func(key string) string { var s string; _ = json.Unmarshal(item[key], &s); return s }
		kind := str("type")
		switch kind {
		case "message", "":
			role := str("role")
			if role == "developer" {
				role = "system"
			}
			if role != "system" && role != "user" && role != "assistant" {
				return invalid("unsupported message role")
			}
			content, e := codingText(item["content"])
			if e != nil {
				return out, e
			}
			out.Messages = append(out.Messages, payload{"role": role, "content": content})
		case "function_call", "custom_tool_call":
			name, e := alias(str("name"), str("namespace"))
			if e != nil {
				return out, e
			}
			var args any
			if kind == "custom_tool_call" {
				args = payload{"input": str("input")}
			} else if json.Unmarshal([]byte(str("arguments")), &args) != nil {
				return invalid("invalid tool arguments")
			}
			id := str("call_id")
			if id == "" {
				return invalid("missing tool call identity")
			}
			calls[id] = name
			out.Messages = append(out.Messages, payload{"role": "assistant", "content": "", "tool_calls": []payload{{"id": id, "type": "function", "function": payload{"name": name, "arguments": args}}}})
		case "function_call_output", "custom_tool_call_output":
			content, e := codingText(item["output"])
			if e != nil {
				return out, e
			}
			id := str("call_id")
			name, ok := calls[id]
			if !ok {
				return invalid("tool output has no preceding call")
			}
			out.Messages = append(out.Messages, payload{"role": "tool", "content": content, "tool_call_id": id, "tool_name": name})
		case "reasoning": // No private chain-of-thought replay is required by local models.
		default:
			return invalid("unsupported input item " + kind)
		}
	}
	if len(out.Messages) == 0 {
		return invalid("input contains no messages")
	}
	return out, nil
}
func codingText(raw json.RawMessage) (string, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var parts []struct{ Type, Text string }
	if json.Unmarshal(raw, &parts) != nil {
		return "", fmt.Errorf("%w: only text input is supported", ErrInvalid)
	}
	var out strings.Builder
	for _, p := range parts {
		if p.Type != "input_text" && p.Type != "output_text" && p.Type != "text" {
			return "", fmt.Errorf("%w: only text input is supported", ErrInvalid)
		}
		out.WriteString(p.Text)
	}
	return out.String(), nil
}

type codingEvents struct {
	w               http.ResponseWriter
	stream, started bool
	sequence        int
	response        payload
	output          []payload
	message         payload
	text            strings.Builder
	failed          bool
}

func newCodingEvents(w http.ResponseWriter, stream bool, model string) *codingEvents {
	return &codingEvents{w: w, stream: stream, response: payload{"id": "resp_" + uuid.NewString(), "object": "response", "created_at": time.Now().Unix(), "status": "in_progress", "model": model, "output": []payload{}, "store": false, "error": nil, "incomplete_details": nil}, output: []payload{}}
}
func (e *codingEvents) event(kind string, fields payload) error {
	if !e.stream {
		return nil
	}
	if !e.started {
		e.w.Header().Set("Content-Type", "text/event-stream")
		e.w.Header().Set("Cache-Control", "no-store")
		e.w.Header().Set("X-Accel-Buffering", "no")
		e.started = true
	}
	e.sequence++
	fields["type"] = kind
	fields["sequence_number"] = e.sequence
	b, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", kind, b); err != nil {
		return err
	}
	if f, ok := e.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}
func (e *codingEvents) start() error {
	if err := e.event("response.created", payload{"response": e.response}); err != nil {
		return err
	}
	return e.event("response.in_progress", payload{"response": e.response})
}
func (e *codingEvents) messageIndex() int {
	for i, item := range e.output {
		if item["id"] == e.message["id"] {
			return i
		}
	}
	return 0
}
func (e *codingEvents) delta(text string) error {
	if text == "" {
		return nil
	}
	if e.message == nil {
		e.message = payload{"id": "msg_" + uuid.NewString(), "type": "message", "role": "assistant", "status": "in_progress", "content": []payload{}}
		e.output = append(e.output, e.message)
		if err := e.event("response.output_item.added", payload{"output_index": len(e.output) - 1, "item": e.message}); err != nil {
			return err
		}
		if err := e.event("response.content_part.added", payload{"item_id": e.message["id"], "output_index": len(e.output) - 1, "content_index": 0, "part": payload{"type": "output_text", "text": "", "annotations": []any{}}}); err != nil {
			return err
		}
	}
	e.text.WriteString(text)
	return e.event("response.output_text.delta", payload{"item_id": e.message["id"], "output_index": e.messageIndex(), "content_index": 0, "delta": text})
}
func (e *codingEvents) tool(t codingTool, args json.RawMessage) error {
	id, call := "fc_"+uuid.NewString(), "call_"+uuid.NewString()
	item := payload{"id": id, "type": "function_call", "call_id": call, "name": t.Name, "arguments": string(args), "status": "completed"}
	if t.Namespace != "" {
		item["namespace"] = t.Namespace
	}
	if t.Type == "custom" {
		var a struct {
			Input string `json:"input"`
		}
		if json.Unmarshal(args, &a) != nil || a.Input == "" {
			return errors.New("local model returned invalid custom tool input")
		}
		item["type"] = "custom_tool_call"
		item["input"] = a.Input
		delete(item, "arguments")
	}
	index := len(e.output)
	e.output = append(e.output, item)
	if err := e.event("response.output_item.added", payload{"output_index": index, "item": item}); err != nil {
		return err
	}
	if t.Type == "function" {
		if err := e.event("response.function_call_arguments.done", payload{"item_id": id, "output_index": index, "arguments": string(args)}); err != nil {
			return err
		}
	}
	return e.event("response.output_item.done", payload{"output_index": index, "item": item})
}
func (e *codingEvents) finish(in, out int, reason string) error {
	if e.message != nil {
		e.message["status"] = "completed"
		e.message["content"] = []payload{{"type": "output_text", "text": e.text.String(), "annotations": []any{}}}
		if err := e.event("response.output_text.done", payload{"item_id": e.message["id"], "output_index": e.messageIndex(), "content_index": 0, "text": e.text.String()}); err != nil {
			return err
		}
		if err := e.event("response.content_part.done", payload{"item_id": e.message["id"], "output_index": e.messageIndex(), "content_index": 0, "part": e.message["content"].([]payload)[0]}); err != nil {
			return err
		}
		if err := e.event("response.output_item.done", payload{"output_index": e.messageIndex(), "item": e.message}); err != nil {
			return err
		}
	}
	e.response["output"] = e.output
	e.response["status"] = "completed"
	event := "response.completed"
	if reason == "length" {
		e.response["status"] = "incomplete"
		e.response["incomplete_details"] = payload{"reason": "max_output_tokens"}
		event = "response.incomplete"
	}
	e.response["usage"] = payload{"input_tokens": in, "output_tokens": out, "total_tokens": in + out, "input_tokens_details": payload{"cached_tokens": 0}, "output_tokens_details": payload{"reasoning_tokens": 0}}
	if !e.stream {
		e.w.Header().Set("Content-Type", "application/json")
		return json.NewEncoder(e.w).Encode(e.response)
	}
	return e.event(event, payload{"response": e.response})
}
func (e *codingEvents) fail(err error) {
	if !e.started {
		return
	}
	e.failed = true
	e.response["status"] = "failed"
	code, message := "model_error", "Local model request failed"
	if errors.Is(err, context.DeadlineExceeded) {
		code, message = "model_timeout", HarnessTimeoutMessage
	} else if errors.Is(err, context.Canceled) {
		code, message = "request_cancelled", "Local model request was cancelled. Check gateway availability and client access."
	}
	e.response["error"] = payload{"code": code, "message": message}
	_ = e.event("response.failed", payload{"response": e.response})
}

func (s *Service) runCodingResponse(ctx context.Context, w http.ResponseWriter, r codingRequest, conn Connection, c HarnessClient, input, output **int) (err error) {
	b := payload{"model": r.Model, "messages": r.Messages, "stream": true, "tools": r.Tools, "think": conn.Thinking, "keep_alive": fmt.Sprintf("%ds", conn.KeepAliveSeconds), "options": payload{"num_ctx": c.ContextLength, "num_predict": r.OutputLimit}}
	// Use Ollama's constrained JSON output for tool turns. This avoids model-
	// specific XML tool parsers corrupting shell commands containing <, > or &.
	// Codex still receives standard Responses function/custom call items.
	structured := len(r.Tools) > 0
	if structured {
		definitions, _ := json.Marshal(r.Tools)
		instruction := "Return one JSON object with content (a string) and tool_calls (an array). Act on the user's task with the available tools. For file or shell operations, make a tool call rather than merely promising to act. Use arguments as a JSON object matching the selected tool's parameters, not a string. While working, use an empty or brief content string and the required tool call. Return an empty tool_calls array only when the task is actually complete or the user only needs a final answer. Use exec_command for file edits when no dedicated edit tool exists. Do not emit XML tool markup. Example: {\"content\":\"\",\"tool_calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"cat README.md\"}}]}. Tool definitions: " + string(definitions)
		messages := append([]payload{}, r.Messages...)
		messages = append(messages, payload{"role": "system", "content": instruction})
		b["messages"] = messages
		b["tools"] = []payload{}
		alternatives := []payload{}
		for _, tool := range r.Tools {
			fn := tool["function"].(payload)
			alternatives = append(alternatives, payload{"type": "object", "properties": payload{"name": payload{"type": "string", "const": fn["name"]}, "arguments": fn["parameters"]}, "required": []string{"name", "arguments"}, "additionalProperties": false})
		}
		b["format"] = payload{"type": "object", "properties": payload{"content": payload{"type": "string"}, "tool_calls": payload{"type": "array", "maxItems": 8, "items": payload{"anyOf": alternatives}}}, "required": []string{"content", "tool_calls"}, "additionalProperties": false}

	}
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", conn.Endpoint+"/api/chat", bytes.NewReader(raw))
	if err != nil {
		return ErrInvalid
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	// The scoped request context governs coding deadlines; scheduled AI keeps
	// its existing HTTP timeout. Reuse the verified transport and redirect policy.
	codingHTTP := *s.local
	codingHTTP.Timeout = 0
	res, err := codingHTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("local model endpoint unreachable")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return errors.New("local model rejected the request")
	}
	events := newCodingEvents(w, r.Stream, r.Model)
	defer func() {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		if err != nil {
			events.fail(err)
		}
	}()
	if err = events.start(); err != nil {
		return err
	}
	scan := bufio.NewScanner(io.LimitReader(res.Body, 16<<20))
	scan.Buffer(make([]byte, 4096), 2<<20)
	done := false
	count := 0
	var envelope strings.Builder
	for scan.Scan() {
		var chunk struct {
			Error   string `json:"error"`
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			Done   bool   `json:"done"`
			Reason string `json:"done_reason"`
			Input  int    `json:"prompt_eval_count"`
			Output int    `json:"eval_count"`
		}
		if json.Unmarshal(scan.Bytes(), &chunk) != nil || chunk.Error != "" {
			return errors.New("invalid local model response")
		}
		count += len(chunk.Message.Content)
		if count > 2<<20 {
			return errors.New("local model output exceeds limit")
		}
		if structured {
			envelope.WriteString(chunk.Message.Content)
		} else if err = events.delta(chunk.Message.Content); err != nil {
			return err
		}
		for _, call := range chunk.Message.ToolCalls {
			mapping, ok := r.Mappings[call.Function.Name]
			if !ok {
				return errors.New("local model selected an unknown tool")
			}
			if !json.Valid(call.Function.Arguments) {
				return errors.New("local model returned invalid tool arguments")
			}
			if err = events.tool(mapping, call.Function.Arguments); err != nil {
				return err
			}
		}
		if chunk.Done {
			*input = &chunk.Input
			*output = &chunk.Output
			if structured {
				var result struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"tool_calls"`
				}
				if json.Unmarshal([]byte(envelope.String()), &result) != nil || len(result.ToolCalls) > 8 {
					return errors.New("invalid structured tool response")
				}
				if err = events.delta(result.Content); err != nil {
					return err
				}
				for _, call := range result.ToolCalls {
					mapping, ok := r.Mappings[call.Name]
					if !ok || !json.Valid(call.Arguments) {
						return errors.New("invalid structured tool call")
					}
					if err = events.tool(mapping, call.Arguments); err != nil {
						return err
					}
				}
			}
			done = true
			*input = &chunk.Input
			*output = &chunk.Output
			err = events.finish(chunk.Input, chunk.Output, chunk.Reason)
			break
		}
	}
	if err != nil {
		return err
	}
	if !done {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("local model stream ended without completion")
	}
	return nil
}
