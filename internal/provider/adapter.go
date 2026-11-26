package provider

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"
)

func newClient(local bool) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if local {
		transport.Proxy = nil
		transport.DialContext = dialLAN
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Pin the dial to a resolved private address. Redirects and HTTP proxies must never forward a local prompt.
func dialLAN(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	for _, raw := range ips {
		ip := raw.Unmap()
		if !isLAN(ip) || (strings.EqualFold(host, "localhost") && !ip.IsLoopback()) {
			continue
		}
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, errors.New("no reachable local address")
}
func isLAN(ip netip.Addr) bool { return ip.IsLoopback() || ip.IsPrivate() }

type payload map[string]any

func (s *Service) request(ctx context.Context, c Connection, key, method, path string, body payload, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return errors.New("cannot encode provider request")
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Endpoint+path, reader)
	if err != nil {
		return errors.New("invalid connection URL")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := s.cloud
	switch c.Kind {
	case "ollama":
		if req.URL.Scheme != "https" && !(req.URL.Scheme == "http" && loopbackHost(req.URL.Hostname())) {
			return errors.New("LAN inference endpoints require HTTPS")
		}
		client = s.local
	case "openai", "chatgpt":
		req.Header.Set("Authorization", "Bearer "+key)
	case "anthropic":
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	case "gemini":
		req.Header.Set("x-goog-api-key", key)
	default:
		return errors.New("unsupported provider")
	}
	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("provider unreachable; check endpoint, network and TLS")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("provider returned HTTP %d", res.StatusCode)
	}
	// Neither errors nor metadata contain prompts, response bodies, URLs or headers.
	const max = 8 << 20
	b, err := io.ReadAll(io.LimitReader(res.Body, max+1))
	if err != nil {
		return errors.New("provider response interrupted")
	}
	if len(b) > max {
		return errors.New("provider response exceeds size limit")
	}
	if out != nil && json.Unmarshal(b, out) != nil {
		return errors.New("invalid provider response")
	}
	return nil
}

func (s *Service) testModel(ctx context.Context, c Connection, key string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if c.Kind == "chatgpt" {
		models, err := s.planModels(ctx, c, key)
		if err != nil {
			return err
		}
		if len(c.Models) == 0 {
			return errors.New("select permitted ChatGPT models before using this connection")
		}
		for _, permitted := range c.Models {
			if !slices.ContainsFunc(models, func(m PlanModel) bool { return m.ID == permitted }) {
				return errors.New("permitted model is unavailable for this ChatGPT account")
			}
		}
		return nil
	}
	model := c.Models[0]
	switch c.Kind {
	case "ollama":
		var tags struct {
			Models []struct {
				Name  string `json:"name"`
				Model string `json:"model"`
			} `json:"models"`
		}
		if err := s.request(ctx, c, "", "GET", "/api/tags", nil, &tags); err != nil {
			return err
		}
		for _, m := range tags.Models {
			if m.Name == model || m.Model == model || m.Name == model+":latest" || m.Model == model+":latest" {
				return nil
			}
		}
		return errors.New("model unavailable at configured endpoint")
	case "openai", "anthropic":
		var result struct {
			ID string `json:"id"`
		}
		err := s.request(ctx, c, key, "GET", "/v1/models/"+url.PathEscape(model), nil, &result)
		if err == nil && result.ID == "" {
			return errors.New("model metadata unavailable")
		}
		return err
	case "gemini":
		var result struct {
			Name string `json:"name"`
		}
		err := s.request(ctx, c, key, "GET", "/v1beta/models/"+url.PathEscape(model), nil, &result)
		if err == nil && result.Name == "" {
			return errors.New("model metadata unavailable")
		}
		return err
	}
	return errors.New("unsupported provider")
}

type completion struct {
	citations     []Citation
	searched      bool
	text          string
	input, output *int
	truncated     bool
}

func (s *Service) generate(ctx context.Context, c Connection, key string, r Request) (completion, error) {
	var out completion
	if r.Format == "json" {
		r.Prompt += "\n\nReturn only a valid JSON object."
	}
	var err error
	switch c.Kind {
	case "ollama":
		out, err = s.ollama(ctx, c, r)
	case "openai":
		out, err = s.openai(ctx, c, key, r)
	case "chatgpt":
		out, err = s.chatgpt(ctx, c, key, r)
	case "anthropic":
		out, err = s.anthropic(ctx, c, key, r)
	case "gemini":
		out, err = s.gemini(ctx, c, key, r)
	default:
		err = errors.New("unsupported provider")
	}
	if err != nil {
		return out, err
	}
	if out.text == "" {
		return out, errors.New("provider returned no text")
	}
	if r.Format == "json" && !json.Valid([]byte(out.text)) {
		return out, errors.New("provider returned invalid JSON")
	}
	if key != "" && strings.Contains(out.text, key) {
		return out, errors.New("provider returned credential material")
	}
	return out, nil
}

func (s *Service) ollama(ctx context.Context, c Connection, r Request) (completion, error) {
	var res struct {
		Response string `json:"response"`
		Done     bool   `json:"done"`
		Reason   string `json:"done_reason"`
		Input    *int   `json:"prompt_eval_count"`
		Output   *int   `json:"eval_count"`
	}
	options := payload{"num_predict": r.MaxTokens}
	if c.ContextLength > 0 {
		options["num_ctx"] = c.ContextLength
	}
	b := payload{"model": r.Model, "prompt": r.Prompt, "stream": false, "options": options, "keep_alive": fmt.Sprintf("%ds", c.KeepAliveSeconds)}
	// Curated presets support thinking. Default to direct answers for bounded
	// summary/extraction tasks; older custom models keep their runtime default.
	if c.Thinking {
		b["think"] = true
	} else {
		for _, preset := range ModelCatalog() {
			if preset.Model == r.Model && preset.Installable {
				b["think"] = false
				break
			}
		}
	}
	if r.Format == "json" {
		b["format"] = "json"
	}
	err := s.request(ctx, c, "", "POST", "/api/generate", b, &res)
	if err == nil && !res.Done {
		err = errors.New("incomplete Ollama response")
	}
	return completion{text: res.Response, input: res.Input, output: res.Output, truncated: res.Reason == "length"}, err
}
func (s *Service) openai(ctx context.Context, c Connection, key string, r Request) (completion, error) {
	var res struct {
		Status     string `json:"status"`
		Incomplete *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			Input  *int `json:"input_tokens"`
			Output *int `json:"output_tokens"`
		} `json:"usage"`
	}
	b := payload{"model": r.Model, "input": r.Prompt, "max_output_tokens": r.MaxTokens, "store": false}
	if r.Format == "json" {
		b["text"] = payload{"format": payload{"type": "json_object"}}
	}
	err := s.request(ctx, c, key, "POST", "/v1/responses", b, &res)
	cut := res.Status == "incomplete" && res.Incomplete != nil && res.Incomplete.Reason == "max_output_tokens"
	if err == nil && res.Status != "completed" && !cut {
		err = errors.New("OpenAI did not complete the request")
	}
	var text strings.Builder
	for _, item := range res.Output {
		if item.Type == "message" {
			for _, content := range item.Content {
				if content.Type == "output_text" {
					text.WriteString(content.Text)
				}
			}
		}
	}
	return completion{text: text.String(), input: res.Usage.Input, output: res.Usage.Output, truncated: cut}, err
}
func (s *Service) anthropic(ctx context.Context, c Connection, key string, r Request) (completion, error) {
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Reason string `json:"stop_reason"`
		Usage  struct {
			Input  *int `json:"input_tokens"`
			Output *int `json:"output_tokens"`
		} `json:"usage"`
	}
	b := payload{"model": r.Model, "max_tokens": r.MaxTokens, "messages": []payload{{"role": "user", "content": r.Prompt}}}
	err := s.request(ctx, c, key, "POST", "/v1/messages", b, &res)
	var text strings.Builder
	for _, p := range res.Content {
		if p.Type == "text" {
			text.WriteString(p.Text)
		}
	}
	return completion{text: text.String(), input: res.Usage.Input, output: res.Usage.Output, truncated: res.Reason == "max_tokens"}, err
}
func (s *Service) gemini(ctx context.Context, c Connection, key string, r Request) (completion, error) {
	var res struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text    string `json:"text"`
					Thought bool   `json:"thought"`
				} `json:"parts"`
			} `json:"content"`
			Reason string `json:"finishReason"`
		} `json:"candidates"`
		Usage struct {
			Input  *int `json:"promptTokenCount"`
			Output *int `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}
	config := payload{"maxOutputTokens": r.MaxTokens}
	if r.Format == "json" {
		config["responseMimeType"] = "application/json"
	}
	b := payload{"contents": []payload{{"role": "user", "parts": []payload{{"text": r.Prompt}}}}, "generationConfig": config}
	err := s.request(ctx, c, key, "POST", "/v1beta/models/"+url.PathEscape(r.Model)+":generateContent", b, &res)
	var text strings.Builder
	cut := false
	if len(res.Candidates) > 0 {
		first := res.Candidates[0]
		cut = first.Reason == "MAX_TOKENS"
		if err == nil && !cut && first.Reason != "STOP" {
			err = errors.New("Gemini did not complete the request")
		}
		for _, p := range first.Content.Parts {
			if !p.Thought {
				text.WriteString(p.Text)
			}
		}
	}
	return completion{text: text.String(), input: res.Usage.Input, output: res.Usage.Output, truncated: cut}, err
}
