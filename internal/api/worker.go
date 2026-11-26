package api

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"sluicework/internal/provider"
	"sluicework/internal/state"
)

func (s *Server) workerRoutes(route func(string, access, endpoint)) {
	route("GET /worker/status", machine, func(r *request) (any, error) { return s.State.Machine(r.worker) })
	route("POST /worker/drain", machine, func(r *request) (any, error) {
		draining := true
		if err := s.State.UpdateMachine("machine:"+r.worker, r.worker, state.MachinePatch{Draining: &draining}); err != nil {
			return nil, err
		}
		return s.State.Machine(r.worker)
	})
	route("POST /worker/runner", machine, func(r *request) (any, error) {
		broker := s.Broker()
		if broker == nil {
			return nil, clientError{503, "GitHub App is not configured on this server"}
		}
		var b struct {
			Operation string `json:"operation"`
		}
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		m, err := s.State.Machine(r.worker)
		if err != nil {
			return nil, err
		}
		return broker.Act(r.Context(), m, b.Operation)
	})
	route("POST /worker/enroll", public, func(r *request) (any, error) {
		var b struct {
			Token string
			Info  state.WorkerInfo
		}
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		return s.State.Enroll(b.Token, b.Info)
	})
	route("POST /worker/heartbeat", machine, func(r *request) (any, error) {
		var b state.WorkerInfo
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		if err := s.State.Heartbeat(r.worker, b); err != nil {
			return nil, err
		}
		return s.State.Machine(r.worker)
	})
	route("POST /worker/claim", machine, func(r *request) (any, error) {
		c, err := s.State.Claim(r.worker)
		if c == nil {
			return nil, err
		}
		return c, err
	})
	route("POST /worker/attempts/{id}/renew", machine, func(r *request) (any, error) { return s.State.Renew(r.worker, oid(r)) })
	route("POST /worker/attempts/{id}/logs", machine, func(r *request) (any, error) {
		var b struct {
			Text     string
			Sequence int64
		}
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		return nil, s.State.AppendLogChunk(r.worker, oid(r), b.Text, b.Sequence)
	})
	route("POST /worker/attempts/{id}/complete", machine, func(r *request) (any, error) {
		var b state.Completion
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		return nil, s.State.Complete(r.worker, oid(r), b)
	})
	route("POST /worker/attempts/{id}/infer", machine, s.infer)
}

var field = regexp.MustCompile(`\{\{([A-Za-z_][A-Za-z0-9_]*)\}\}`)

func prompt(claim *state.Claim, files map[string]string) (string, error) {
	values := map[string]string{}
	total, count := 0, 0
	for _, input := range claim.Job.Inputs {
		value := claim.Inputs[input.Name]
		if len(input.Paths) > 0 && value != "" {
			var found bool
			if value, found = files[input.Name]; !found {
				return "", bad("missing selected file input")
			}
			total += len(value)
			count++
		}
		values[input.Name] = value
	}
	if count != len(files) || total > 256<<10 {
		return "", bad("unexpected or oversized file inputs")
	}
	template := claim.Job.Prompt
	var out strings.Builder
	cursor := 0
	for _, m := range field.FindAllStringSubmatchIndex(template, -1) {
		value := values[template[m[2]:m[3]]]
		if out.Len()+m[0]-cursor+len(value) > 2<<20 {
			return "", bad("rendered prompt exceeds size limit")
		}
		out.WriteString(template[cursor:m[0]])
		out.WriteString(value)
		cursor = m[1]
	}
	out.WriteString(template[cursor:])
	return out.String(), nil
}
func (s *Server) infer(r *request) (any, error) {
	var b struct{ Inputs map[string]string }
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	claim, err := s.State.ActiveClaim(r.worker, oid(r))
	if err != nil {
		return nil, err
	}
	j := claim.Job
	if j.Type != "ai" {
		return nil, clientError{403, "inference is available to AI attempts only"}
	}
	input, err := prompt(claim, b.Inputs)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(j.TimeoutSeconds)*time.Second)
	defer cancel()
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, e := s.State.ActiveClaim(r.worker, claim.AttemptID); e != nil {
					cancel()
					return
				}
			}
		}
	}()
	result, err := s.Provider.Generate(ctx, provider.Request{AttemptID: claim.AttemptID, ConnectionID: j.Connection, Model: j.Model, Prompt: input, AllowCloud: j.AllowCloud, MaxTokens: j.MaxTokens, Format: j.OutputFormat})
	if err != nil && result.Call.Error != "" {
		return nil, clientError{502, result.Call.Error}
	}
	if errors.Is(err, context.Canceled) {
		return nil, state.ErrLease
	}
	if err != nil {
		return nil, err
	}
	if _, err = s.State.ActiveClaim(r.worker, claim.AttemptID); err != nil {
		return nil, err
	}
	if j.DiscardOutput {
		return map[string]any{"text": "", "truncated": false}, nil
	}
	if len(result.Text) > j.OutputLimit {
		if j.OutputFormat == "json" {
			return nil, clientError{422, "JSON result exceeds the template output limit"}
		}
		cut := j.OutputLimit
		for cut > 0 && !utf8.RuneStart(result.Text[cut]) {
			cut--
		}
		result.Text = result.Text[:cut]
		result.Truncated = true
	}
	return map[string]any{"text": result.Text, "truncated": result.Truncated}, nil
}
