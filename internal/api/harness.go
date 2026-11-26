package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	"sluicework/internal/codexclient"
	"sluicework/internal/provider"
)

func (s *Server) harnessManagementRoutes(route func(string, access, endpoint)) {
	route("GET /harness", admin, func(r *request) (any, error) {
		p, e := s.Provider.HarnessPolicy(r.Context())
		if e != nil {
			return nil, e
		}
		clients, e := s.Provider.HarnessClients(r.Context())
		if e != nil {
			return nil, e
		}
		calls, e := s.Provider.HarnessCalls(r.Context())
		return map[string]any{"policy": p, "clients": clients, "calls": calls}, e
	})
	route("PUT /harness/policy", admin, func(r *request) (any, error) {
		var p provider.HarnessPolicy
		if e := decode(r, &p); e != nil {
			return nil, e
		}
		return nil, s.Provider.SetHarnessPolicy(r.Context(), r.user.Username, p)
	})
	route("POST /harness/clients", admin, func(r *request) (any, error) {
		var input struct {
			provider.HarnessClientInput
			ServerURL string `json:"serverUrl"`
		}
		if e := decode(r, &input); e != nil {
			return nil, e
		}
		origin, e := codexclient.Origin(input.ServerURL)
		if e != nil {
			return nil, bad(e.Error())
		}
		ca := ""
		if s.Configuration != nil {
			n := s.Configuration.Snapshot().Network
			if n.Enabled && strings.EqualFold(n.URL, origin) {
				ca = n.CAPEM
			}
		}
		c, token, e := s.Provider.CreateHarnessClient(r.Context(), r.user.Username, input.HarnessClientInput)
		if e != nil {
			return nil, e
		}
		bundle := codexclient.Bundle{Version: 1, ServerURL: origin, Token: token, ClientID: c.ID, Name: c.Name, Models: c.Models, ContextLength: c.ContextLength, MaxOutputTokens: c.MaxOutputTokens, Expires: c.Expires, CAPEM: ca}
		fingerprint := ""
		if ca != "" {
			fingerprint, _ = codexclient.Fingerprint(ca)
		}
		return map[string]any{"client": bundle, "caFingerprint": fingerprint}, nil
	})
	route("PUT /harness/clients/{id}/timeout", admin, func(r *request) (any, error) {
		var input struct {
			Seconds int `json:"timeoutSeconds"`
		}
		if e := decode(r, &input); e != nil {
			return nil, e
		}
		return nil, s.Provider.SetHarnessClientTimeout(r.Context(), r.user.Username, oid(r), input.Seconds)
	})
	route("POST /harness/clients/{id}/revoke", admin, func(r *request) (any, error) {
		return nil, s.Provider.RevokeHarnessClient(r.Context(), r.user.Username, oid(r))
	})
}
func harnessError(w http.ResponseWriter, e error) {
	code, kind, msg := 502, "server_error", "Local model request failed"
	switch {
	case errors.Is(e, context.DeadlineExceeded):
		code, kind, msg = http.StatusGatewayTimeout, "timeout_error", provider.HarnessTimeoutMessage
	case errors.Is(e, provider.ErrHarnessAuth):
		code, kind, msg = 401, "authentication_error", e.Error()
	case errors.Is(e, provider.ErrHarnessDisabled):
		code, kind, msg = 503, "service_unavailable", e.Error()
	case errors.Is(e, provider.ErrHarnessBusy):
		code, kind, msg = 429, "rate_limit_error", e.Error()
		w.Header().Set("Retry-After", "2")
	case errors.Is(e, provider.ErrInvalid):
		code, kind, msg = 400, "invalid_request_error", e.Error()
	case errors.Is(e, provider.ErrConflict):
		code, kind, msg = 409, "request_conflict", e.Error()
	}
	response(w, code, map[string]any{"error": map[string]string{"message": msg, "type": kind}})
}

type codingWriter struct {
	http.ResponseWriter
	started bool
}

func (w *codingWriter) WriteHeader(status int) {
	w.started = true
	w.ResponseWriter.WriteHeader(status)
}
func (w *codingWriter) Write(b []byte) (int, error) {
	w.started = true
	return w.ResponseWriter.Write(b)
}
func (w *codingWriter) Flush() {
	w.started = true
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (s *Server) harnessRoutes(mux *http.ServeMux) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			host, _, e := net.SplitHostPort(r.RemoteAddr)
			ip := net.ParseIP(host)
			if e != nil || ip == nil || !ip.IsLoopback() {
				harnessError(w, provider.ErrHarnessAuth)
				return
			}
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			harnessError(w, provider.ErrHarnessAuth)
			return
		}
		c, e := s.Provider.AuthenticateHarness(r.Context(), token)
		if e != nil {
			harnessError(w, e)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == "GET" {
			models, e := s.Provider.HarnessModels(r.Context(), c)
			if e != nil {
				harnessError(w, e)
				return
			}
			data := []any{}
			for _, m := range models {
				data = append(data, map[string]any{"id": m, "object": "model", "owned_by": "sluicework"})
			}
			response(w, 200, map[string]any{"object": "list", "data": data})
			return
		}
		if r.Header.Get("Content-Encoding") != "" && r.Header.Get("Content-Encoding") != "identity" {
			harnessError(w, provider.ErrInvalid)
			return
		}
		raw, e := io.ReadAll(io.LimitReader(r.Body, (2<<20)+1))
		if e != nil || len(raw) > 2<<20 || !json.Valid(raw) {
			harnessError(w, provider.ErrInvalid)
			return
		}
		tracked := &codingWriter{ResponseWriter: w}
		e = s.Provider.ServeHarnessResponse(tracked, r, c, raw)
		if e != nil && !tracked.started {
			harnessError(w, e)
		}
	}
	mux.HandleFunc("GET "+base+"/harness/v1/models", handler)
	mux.HandleFunc("POST "+base+"/harness/v1/responses", handler)
}
