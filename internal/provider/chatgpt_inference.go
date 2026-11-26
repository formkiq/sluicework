package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type PlanModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *Service) CheckConnectionActor(ctx context.Context, c Connection, actor string) error {
	if c.Kind != "chatgpt" {
		return nil
	}
	if actor == "" || c.Owner != actor {
		return fmt.Errorf("%w: this ChatGPT subscription belongs to another user; connect your own account or select a shared API-key connection", ErrInvalid)
	}
	return s.planOwner(ctx, actor)
}

func (s *Service) ConnectionsFor(ctx context.Context, actor string) ([]Connection, error) {
	connections, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	visible := []Connection{}
	for _, c := range connections {
		if c.Kind != "chatgpt" || (s.personalOwner != "" && c.Owner == actor) {
			visible = append(visible, c)
		}
	}
	return visible, nil
}

func (s *Service) PlanModels(ctx context.Context, actor, connectionID string) ([]PlanModel, error) {
	c, err := s.Get(ctx, connectionID)
	if err != nil {
		return nil, err
	}
	if c.Kind != "chatgpt" {
		return nil, ErrInvalid
	}
	if err := s.CheckConnectionActor(ctx, c, actor); err != nil {
		return nil, err
	}
	c, key, err := s.load(ctx, connectionID)
	if err != nil {
		return nil, err
	}
	return s.planModels(ctx, c, key)
}

func (s *Service) planModels(ctx context.Context, c Connection, key string) ([]PlanModel, error) {
	var catalog struct {
		Models []struct {
			Slug       string `json:"slug"`
			Name       string `json:"display_name"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	if err := s.request(ctx, c, key, "GET", "/v1/models", nil, &catalog); err != nil {
		return nil, err
	}
	models := []PlanModel{}
	seen := map[string]bool{}
	for _, model := range catalog.Models {
		if model.Visibility != "list" || !modelPattern.MatchString(model.Slug) || strings.ContainsAny(model.Slug, "/") || seen[model.Slug] {
			continue
		}
		seen[model.Slug] = true
		name := model.Name
		if len(name) > 256 {
			name = model.Slug
		}
		models = append(models, PlanModel{ID: model.Slug, Name: name})
	}
	if len(models) == 0 {
		return nil, errors.New("no models available for this ChatGPT account; review plan permissions")
	}
	return models, nil
}

func (s *Service) checkPlanRun(ctx context.Context, c Connection, attemptID string) error {
	if s.personalOwner == "" {
		return fmt.Errorf("%w: ChatGPT requests must execute in the owner's private runtime", ErrConflict)
	}
	// Derive the identity from the stored run, never from a worker-supplied owner.
	// Scheduled runs currently have no durable user identity, so fail closed.
	var actor, schedule string
	if err := s.db.QueryRowContext(ctx, `SELECT r.trigger,r.schedule_id FROM runs r JOIN attempts a ON a.run_id=r.id WHERE a.id=?`, attemptID).Scan(&actor, &schedule); err != nil {
		return errors.New("ChatGPT requires a user-owned authenticated run")
	}
	if schedule != "" {
		return errors.New("scheduled tasks require a shared API-key or local connection; personal subscription scheduling is not enabled")
	}
	return s.CheckConnectionActor(ctx, c, actor)
}

func (s *Service) chatgpt(ctx context.Context, c Connection, key string, r Request) (completion, error) {
	callCtx, cancel := context.WithCancel(ctx)
	s.planMu.Lock()
	if s.planClosed {
		s.planMu.Unlock()
		cancel()
		return completion{}, ErrConflict
	}
	current, err := s.Get(ctx, c.ID)
	if err != nil || current.Disabled || current.secretRef != c.secretRef {
		s.planMu.Unlock()
		cancel()
		return completion{}, errors.New("ChatGPT connection changed; retry after signing in")
	}
	if s.planCalls == nil {
		s.planCalls = map[string]map[string]context.CancelFunc{}
	}
	if s.planCalls[c.ID] == nil {
		s.planCalls[c.ID] = map[string]context.CancelFunc{}
	}
	s.planCalls[c.ID][r.AttemptID] = cancel
	s.planMu.Unlock()
	defer func() { cancel(); s.planMu.Lock(); delete(s.planCalls[c.ID], r.AttemptID); s.planMu.Unlock() }()
	// SIWC does not support max_output_tokens. Do not send unsupported API
	// parameters or silently switch to API billing to obtain a token cap.
	body, err := planBody(r)
	if err != nil {
		return completion{}, err
	}
	req, _ := http.NewRequestWithContext(callCtx, "POST", endpoints["chatgpt"]+"/v1/responses", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	res, err := s.cloud.Do(req)
	if err != nil {
		if callCtx.Err() != nil {
			return completion{}, callCtx.Err()
		}
		return completion{}, errors.New("ChatGPT is unreachable; check network and TLS")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return completion{}, fmt.Errorf("ChatGPT returned HTTP %d; review sign-in and usage limits (no API-key fallback)", res.StatusCode)
	}
	result, err := readPlanStream(res.Body)
	if err != nil {
		return completion{}, err
	}
	if r.WebSearch != nil && !result.searched {
		return completion{}, errors.New("ChatGPT did not complete the requested web search; choose a model and account with web search enabled (no fallback)")
	}
	if r.WebSearch == nil && result.searched {
		return completion{}, errors.New("ChatGPT returned an unapproved web search")
	}
	return result, nil
}

func readPlanStream(body io.Reader) (completion, error) {
	var result completion
	scanner := bufio.NewScanner(io.LimitReader(body, (8<<20)+1))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var data, deltas strings.Builder
	total := 0
	completed := false
	handle := func() error {
		if data.Len() == 0 {
			return nil
		}
		defer data.Reset()
		var event struct {
			Type     string `json:"type"`
			Delta    string `json:"delta"`
			Response struct {
				Status string `json:"status"`
				Error  struct {
					Code string `json:"code"`
				} `json:"error"`
				Output []struct {
					Type    string `json:"type"`
					Status  string `json:"status"`
					Content []struct {
						Type        string `json:"type"`
						Text        string `json:"text"`
						Annotations []struct {
							Type  string `json:"type"`
							URL   string `json:"url"`
							Title string `json:"title"`
							Start int    `json:"start_index"`
							End   int    `json:"end_index"`
						} `json:"annotations"`
					} `json:"content"`
				} `json:"output"`
				Usage struct {
					Input  *int `json:"input_tokens"`
					Output *int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(data.String()), &event) != nil {
			return errors.New("invalid ChatGPT stream event")
		}
		switch event.Type {
		case "response.output_text.delta":
			deltas.WriteString(event.Delta)
		case "response.failed", "response.incomplete", "error":
			switch event.Response.Error.Code {
			case "subscription_sharing_usage_limit_exceeded":
				return errors.New("ChatGPT plan usage limit reached; review ChatGPT Settings → Usage (no API-key fallback)")
			case "subscription_sharing_usage_unavailable":
				return errors.New("ChatGPT plan usage unavailable; review plan permissions (no API-key fallback)")
			}
			return errors.New("ChatGPT response failed or was incomplete")
		case "response.completed":
			if completed || event.Response.Status != "completed" {
				return errors.New("invalid ChatGPT completion event")
			}
			var text strings.Builder
			offset := 0
			for _, item := range event.Response.Output {
				if item.Type == "web_search_call" {
					if item.Status != "completed" {
						return errors.New("ChatGPT web search did not complete")
					}
					result.searched = true
				}
				if item.Type == "function_call" || item.Type == "custom_tool_call" {
					return errors.New("ChatGPT requested an unavailable tool")
				}
				if item.Type == "message" {
					for _, part := range item.Content {
						if part.Type == "output_text" {
							text.WriteString(part.Text)
							length := len([]rune(part.Text))
							for _, a := range part.Annotations {
								if a.Type == "url_citation" && len(result.citations) < 128 && SafeCitationURL(a.URL) && a.Start >= 0 && a.End > a.Start && a.End <= length {
									title := a.Title
									if len(title) > 512 {
										title = a.URL
									}
									result.citations = append(result.citations, Citation{URL: a.URL, Title: title, Start: offset + a.Start, End: offset + a.End})
								}
							}
							offset += length
						}
					}
				}
			}
			result.text = text.String()
			if result.text == "" {
				result.text = deltas.String()
			}
			result.input, result.output = event.Response.Usage.Input, event.Response.Usage.Output
			completed = true
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		total += len(line) + 1
		if total > 8<<20 {
			return completion{}, errors.New("ChatGPT response exceeds size limit")
		}
		if line == "" {
			if err := handle(); err != nil {
				return completion{}, err
			}
			if completed {
				return result, nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if scanner.Err() != nil {
		return completion{}, errors.New("ChatGPT stream interrupted or oversized")
	}
	if err := handle(); err != nil {
		return completion{}, err
	}
	if !completed {
		return completion{}, errors.New("ChatGPT stream ended without response.completed")
	}
	return result, nil
}
