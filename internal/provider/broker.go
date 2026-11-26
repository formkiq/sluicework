package provider

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Request is assembled by the server exclusively from the authenticated attempt and its snapshot.
type Request struct {
	AttemptID, ConnectionID, Model, Prompt string
	AllowCloud                             bool
	MaxTokens                              int
	Format                                 string
	WebSearch                              *WebSearch
}
type Result struct {
	Text        string     `json:"text"`
	Citations   []Citation `json:"citations,omitempty"`
	WebSearched bool       `json:"webSearched,omitempty"`
	Truncated   bool       `json:"truncated"`
	Call        Call       `json:"call"`
}
type Call struct {
	AttemptID    string `json:"attempt_id"`
	ConnectionID string `json:"connection_id"`
	Kind         string `json:"kind"`
	Model        string `json:"model"`
	Started      string `json:"started"`
	DurationMS   int64  `json:"duration_ms"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
	InputTokens  *int   `json:"input_tokens,omitempty"`
	OutputTokens *int   `json:"output_tokens,omitempty"`
}

func (s *Service) Generate(ctx context.Context, r Request) (Result, error) {
	var result Result
	if r.MaxTokens == 0 {
		r.MaxTokens = 1024
	}
	if r.Format == "" {
		r.Format = "text"
	}
	if r.AttemptID == "" || r.Prompt == "" || len(r.Prompt) > 2<<20 || r.MaxTokens < 1 || r.MaxTokens > 131072 || (r.Format != "text" && r.Format != "json") {
		return result, fmt.Errorf("%w: invalid inference request or limit", ErrInvalid)
	}
	preview, err := s.Get(ctx, r.ConnectionID)
	if err != nil {
		return result, err
	}
	if r.WebSearch != nil {
		if s.personalOwner == "" || preview.Kind != "chatgpt" {
			return result, fmt.Errorf("%w: web research requires a private ChatGPT runtime", ErrInvalid)
		}
		if err := r.WebSearch.Validate(); err != nil {
			return result, err
		}
	}
	if preview.Kind == "chatgpt" {
		if err := s.checkPlanRun(ctx, preview, r.AttemptID); err != nil {
			return result, err
		}
	}
	c, key, keyErr := s.load(ctx, r.ConnectionID)
	if c.ID == "" {
		return result, keyErr
	}
	if err := c.ValidatePolicy(r.Model, r.AllowCloud); err != nil {
		return result, err
	}
	start := time.Now()
	call := Call{AttemptID: r.AttemptID, ConnectionID: c.ID, Kind: c.Kind, Model: r.Model, Started: start.UTC().Format(time.RFC3339Nano), Status: "running"}
	// Share local model admission with authenticated coding clients.
	if err := s.reserveCall(ctx, call, "", 0); err != nil {
		return result, err
	}
	var res completion
	err = keyErr
	if err == nil {
		res, err = s.generate(ctx, c, key, r)
	}
	call.DurationMS = time.Since(start).Milliseconds()
	call.InputTokens, call.OutputTokens = res.input, res.output
	call.Status = "succeeded"
	if err != nil {
		call.Status = "failed"
		call.Error = err.Error()
		if errors.Is(err, context.Canceled) {
			call.Status = "cancelled"
		}
	}
	// Persist terminal diagnostics even if the worker disconnected during the request.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, saveErr := s.db.ExecContext(finishCtx, `UPDATE provider_calls SET status=?,error=?,duration_ms=?,input_tokens=?,output_tokens=? WHERE attempt_id=?`, call.Status, call.Error, call.DurationMS, call.InputTokens, call.OutputTokens, call.AttemptID)
	if saveErr != nil {
		return result, saveErr
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		if e := s.recordHealth(finishCtx, c, err); e != nil {
			return result, e
		}
	}
	result.Call = call
	if err != nil {
		return result, err
	}
	result.Text = res.text
	result.Citations = res.citations
	result.WebSearched = res.searched
	result.Truncated = res.truncated
	return result, nil
}

func (s *Service) Calls(ctx context.Context, attemptID string) ([]Call, error) {
	row := s.db.QueryRowContext(ctx, `SELECT attempt_id,connection_id,kind,model,started,duration_ms,status,error,input_tokens,output_tokens FROM provider_calls WHERE attempt_id=?`, attemptID)
	var call Call
	err := row.Scan(&call.AttemptID, &call.ConnectionID, &call.Kind, &call.Model, &call.Started, &call.DurationMS, &call.Status, &call.Error, &call.InputTokens, &call.OutputTokens)
	if errors.Is(err, sql.ErrNoRows) {
		return []Call{}, nil
	}
	if err != nil {
		return nil, err
	}
	return []Call{call}, nil
}
