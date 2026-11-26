// Package api exposes the versioned, same-origin console and worker protocol.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"sluicework/docs"
	"sluicework/internal/auth"
	"sluicework/internal/clusterconfig"
	"sluicework/internal/githubapp"
	"sluicework/internal/personal"
	"sluicework/internal/provider"
	"sluicework/internal/schedule"
	"sluicework/internal/state"
)

const base = "/api/v1"
const cookieName = "sluicework_session"
const maxBody = 10 << 20

type Server struct {
	Configuration *clusterconfig.Manager
	githubMu      sync.RWMutex
	GitHub        *githubapp.Broker
	State         *state.Store
	Auth          *auth.Service
	Provider      *provider.Service
	Schedule      *schedule.Service
}

func (s *Server) Broker() *githubapp.Broker {
	s.githubMu.RLock()
	defer s.githubMu.RUnlock()
	return s.GitHub
}
func (s *Server) setBroker(b *githubapp.Broker) { s.githubMu.Lock(); s.GitHub = b; s.githubMu.Unlock() }

type request struct {
	*http.Request
	user   auth.User
	worker string
}
type endpoint func(*request) (any, error)
type access int

const (
	public access = iota
	member
	admin
	machine
)

type clientError struct {
	code int
	msg  string
}

func (e clientError) Error() string { return e.msg }
func bad(msg string) error          { return clientError{400, msg} }
func decode(r *request, v any) error {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return bad("invalid JSON request or unknown field")
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return bad("expected a single JSON object")
	}
	return nil
}
func response(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}
func report(w http.ResponseWriter, err error) {
	code := 500
	msg := "internal server error"
	var e clientError
	switch {
	case errors.As(err, &e):
		code, msg = e.code, e.msg
	case errors.Is(err, personal.ErrDenied):
		code, msg = 403, "personal runtime belongs to a different signed-in user or the request proof is invalid"
	case errors.Is(err, personal.ErrInvalid):
		code, msg = 400, err.Error()
	case errors.Is(err, auth.ErrLogin), errors.Is(err, auth.ErrSession), errors.Is(err, state.ErrUnauthorized):
		code, msg = 401, err.Error()
	case errors.Is(err, state.ErrLease), errors.Is(err, state.ErrConflict), errors.Is(err, provider.ErrConflict):
		code, msg = 409, err.Error()
	case errors.Is(err, state.ErrNotFound), errors.Is(err, provider.ErrNotFound):
		code, msg = 404, err.Error()
	case errors.Is(err, state.ErrInvalid), errors.Is(err, provider.ErrInvalid):
		code, msg = 400, err.Error()
	default:
		log.Printf("request failed (%T)", err)
	}
	response(w, code, map[string]string{"error": msg})
}
func sessionToken(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	return c.Value
}
func writeCookie(w http.ResponseWriter, r *http.Request, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: value, Path: base, Secure: r.TLS != nil, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: age})
}
func (s *Server) Handler(console http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+base+"/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(docs.Contract)
	})
	route := func(pattern string, a access, f endpoint) {
		method, path, _ := strings.Cut(pattern, " ")
		mux.HandleFunc(method+" "+base+path, func(w http.ResponseWriter, r *http.Request) {
			req := &request{Request: r}
			switch a {
			case member, admin:
				user, err := s.Auth.Lookup(sessionToken(r))
				if err != nil {
					report(w, err)
					return
				}
				if a == admin && user.Role != "admin" {
					report(w, clientError{403, "administrator permission required"})
					return
				}
				req.user = user
			case machine:
				token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
				if !ok || token == "" {
					report(w, state.ErrUnauthorized)
					return
				}
				id, err := s.State.AuthenticateWorker(token)
				if err != nil {
					report(w, err)
					return
				}
				req.worker = id
			}
			value, err := f(req)
			if err != nil {
				report(w, err)
				return
			}
			if file, ok := value.(attachment); ok {
				w.Header().Set("Content-Type", file.ContentType)
				w.Header().Set("Content-Disposition", `attachment; filename="`+file.Name+`"`)
				_, _ = w.Write(file.Data)
				return
			}
			if value == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			response(w, 200, value)
		})
	}
	mux.HandleFunc("POST "+base+"/login", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Username, Password string }
		if err := decode(&request{Request: r}, &b); err != nil {
			report(w, err)
			return
		}
		user, token, err := s.Auth.Login(b.Username, b.Password)
		if err != nil {
			report(w, err)
			return
		}
		writeCookie(w, r, token, int(auth.Lifetime.Seconds()))
		response(w, 200, user)
	})
	mux.HandleFunc("POST "+base+"/logout", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Auth.Logout(sessionToken(r)); err != nil {
			report(w, err)
			return
		}
		writeCookie(w, r, "", -1)
		w.WriteHeader(204)
	})
	route("GET /session", member, func(r *request) (any, error) { return r.user, nil })
	s.consoleRoutes(route)
	s.workerRoutes(route)
	s.harnessRoutes(mux)
	mux.HandleFunc("GET "+base+"/runs/{id}/events", s.events)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { report(w, clientError{404, "unknown API endpoint"}) })
	mux.Handle("/", console)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'; object-src 'none'; base-uri 'none'; form-action 'self'")
		if r.TLS == nil {
			host, _, err := net.SplitHostPort(r.Host)
			if err != nil {
				host = strings.Trim(r.Host, "[]")
			}
			ip := net.ParseIP(host)
			if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
				report(w, clientError{403, "HTTPS is required for LAN access"})
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
			if r.Method != "GET" && r.Method != "HEAD" {
				origin := r.Header.Get("Origin")
				scheme := "http"
				if r.TLS != nil {
					scheme = "https"
				}
				if origin != "" {
					u, err := url.Parse(origin)
					if err != nil || u.Scheme != scheme || !strings.EqualFold(u.Host, r.Host) {
						report(w, clientError{403, "cross-origin request rejected"})
						return
					}
				}
				if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
					report(w, clientError{403, "cross-site request rejected"})
					return
				}
				typ, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
				if typ != "application/json" && !(strings.HasPrefix(r.URL.Path, base+"/worker/") && r.ContentLength == 0) {
					report(w, clientError{415, "use application/json"})
					return
				}
				limit := int64(maxBody)
				if strings.HasPrefix(r.URL.Path, base+"/worker/attempts/") && strings.HasSuffix(r.URL.Path, "/complete") {
					limit = (25 << 20)
				}
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
		}
		mux.ServeHTTP(w, r)
	})
}
func after(r *http.Request) (int64, error) {
	cursor := r.Header.Get("Last-Event-ID")
	if cursor == "" {
		cursor = r.URL.Query().Get("after")
	}
	if cursor == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(cursor, 10, 64)
	if err != nil || n < 0 {
		return 0, bad("invalid log cursor")
	}
	return n, nil
}
func oid(r *request) string { return r.PathValue("id") }
func validateJobPolicy(s *Server, r *request, j state.Job, approving ...bool) error {
	if j.Type != "ai" {
		return nil
	}
	c, err := s.Provider.Get(r.Context(), j.Connection)
	if err != nil {
		return err
	}
	// Administrators may approve a template for another user's connection.
	// Approval does not grant permission to invoke that user's credentials.
	if len(approving) == 0 || !approving[0] {
		if err := s.Provider.CheckConnectionActor(r.Context(), c, r.user.Username); err != nil {
			return err
		}
	}
	return c.ValidatePolicy(j.Model, j.AllowCloud)
}
func (s *Server) consoleRoutes(route func(string, access, endpoint)) {
	s.chatGPTRoutes(route)
	s.personalRoutes(route)
	s.configurationRoutes(route)
	s.harnessManagementRoutes(route)
	s.githubManagementRoutes(route)
	route("GET /overview", member, s.overview)
	route("GET /github/activity", member, func(r *request) (any, error) {
		broker := s.Broker()
		if broker == nil {
			return githubapp.Activity{Builds: []githubapp.Build{}}, nil
		}
		return broker.Activity(r.Context())
	})
	route("GET /machines", member, func(r *request) (any, error) { return s.State.Machines() })
	route("POST /machines/{id}/workstation/availability", admin, func(r *request) (any, error) {
		var input struct {
			Operation string `json:"operation"`
			Minutes   int    `json:"minutes"`
		}
		if err := decode(r, &input); err != nil {
			return nil, err
		}
		if err := s.State.SetWorkstationAvailability(r.user.Username, oid(r), input.Operation, input.Minutes); err != nil {
			return nil, err
		}
		return s.State.Machine(oid(r))
	})
	route("POST /enrollment-tokens", admin, func(r *request) (any, error) {
		t, err := s.State.EnrollToken(r.user.Username)
		return map[string]any{"token": t, "expiresIn": 600}, err
	})
	route("PATCH /machines/{id}", member, func(r *request) (any, error) {
		var b state.MachinePatch
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		if r.user.Role != "admin" && (b.Labels != nil || b.Runner != nil) {
			return nil, clientError{403, "administrator permission is required for machine labels and CI runner registration"}
		}
		if err := s.State.UpdateMachine(r.user.Username, oid(r), b); err != nil {
			return nil, err
		}
		return s.State.Machine(oid(r))
	})
	route("POST /machines/{id}/revoke", admin, func(r *request) (any, error) { return nil, s.State.Revoke(r.user.Username, oid(r)) })
	route("GET /jobs", member, func(r *request) (any, error) { return s.State.Jobs() })
	route("GET /jobs/{id}", member, func(r *request) (any, error) { return s.State.Job(oid(r)) })
	saveJob := func(r *request) (any, error) {
		var j state.Job
		if err := decode(r, &j); err != nil {
			return nil, err
		}
		if err := validateJobPolicy(s, r, j, true); err != nil {
			return nil, err
		}
		return s.State.SaveJob(r.user.Username, oid(r), j)
	}
	route("POST /jobs", admin, saveJob)
	route("PUT /jobs/{id}", admin, saveJob)
	route("POST /jobs/{id}/runs", member, func(r *request) (any, error) {
		var b struct {
			Inputs map[string]string `json:"inputs"`
		}
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		j, err := s.State.Job(oid(r))
		if err != nil {
			return nil, err
		}
		if err := validateJobPolicy(s, r, j); err != nil {
			return nil, err
		}
		return s.State.Queue(r.user.Username, oid(r), b.Inputs)
	})
	route("GET /runs", member, func(r *request) (any, error) {
		q := r.URL.Query()
		return s.State.RunsBefore(q.Get("job"), q.Get("machine"), q.Get("status"), q.Get("before"))
	})
	route("GET /runs/{id}", member, func(r *request) (any, error) { return s.State.Run(oid(r)) })
	route("POST /runs/{id}/cancel", member, func(r *request) (any, error) { return nil, s.State.Cancel(r.user.Username, oid(r)) })
	route("POST /runs/{id}/retry", member, func(r *request) (any, error) {
		run, err := s.State.Run(oid(r))
		if err != nil {
			return nil, err
		}
		if err := validateJobPolicy(s, r, run.Job); err != nil {
			return nil, err
		}
		return s.State.Retry(r.user.Username, oid(r))
	})
	route("GET /runs/{id}/logs", member, func(r *request) (any, error) {
		n, e := after(r.Request)
		if e != nil {
			return nil, e
		}
		return s.State.Logs(oid(r), n)
	})
	route("GET /runs/{id}/calls", member, func(r *request) (any, error) {
		run, err := s.State.Run(oid(r))
		if err != nil {
			return nil, err
		}
		calls := []provider.Call{}
		for _, a := range run.Attempts {
			row, e := s.Provider.Calls(r.Context(), a.ID)
			if e != nil {
				return nil, e
			}
			calls = append(calls, row...)
		}
		return calls, nil
	})
	route("GET /schedules/{id}", member, func(r *request) (any, error) { return s.Schedule.Get(oid(r)) })
	route("GET /schedules", member, func(r *request) (any, error) { return s.Schedule.List() })
	saveSchedule := func(r *request) (any, error) {
		var b schedule.Spec
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		job, err := s.State.Job(b.JobID)
		if err != nil {
			return nil, err
		}
		if job.Type == "ai" {
			c, err := s.Provider.Get(r.Context(), job.Connection)
			if err != nil {
				return nil, err
			}
			if c.Kind == "chatgpt" {
				return nil, bad("scheduled tasks require a shared API-key or local connection; personal subscription scheduling is not enabled")
			}
		}
		return s.Schedule.Save(r.user.Username, oid(r), b)
	}
	route("POST /schedules", member, saveSchedule)
	route("PUT /schedules/{id}", member, saveSchedule)
	route("DELETE /schedules/{id}", member, func(r *request) (any, error) { return nil, s.Schedule.Delete(r.user.Username, oid(r)) })
	route("GET /schedules/{id}/occurrences", member, func(r *request) (any, error) { return s.Schedule.Occurrences(oid(r)) })
	route("GET /connections", member, func(r *request) (any, error) {
		if r.URL.Query().Get("purpose") == "job-approval" {
			if r.user.Role != "admin" {
				return nil, clientError{403, "administrator permission required for job approval metadata"}
			}
			return s.Provider.List(r.Context())
		}
		return s.Provider.ConnectionsFor(r.Context(), r.user.Username)
	})
	route("GET /model-catalog", admin, func(r *request) (any, error) { return provider.ModelCatalog(), nil })
	route("GET /connections/{id}/models", admin, func(r *request) (any, error) { return s.Provider.Models(r.Context(), oid(r)) })
	route("POST /connections/{id}/models", admin, func(r *request) (any, error) {
		var b struct {
			Model string `json:"model"`
		}
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		return s.Provider.InstallModel(provider.WithActor(r.Context(), r.user.Username), oid(r), b.Model)
	})
	route("POST /connections/{id}/models/{install}/cancel", admin, func(r *request) (any, error) {
		return nil, s.Provider.CancelModelInstall(provider.WithActor(r.Context(), r.user.Username), oid(r), r.PathValue("install"))
	})
	saveConnection := func(r *request) (any, error) {
		var b provider.Config
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		ctx := provider.WithActor(r.Context(), r.user.Username)
		if oid(r) != "" {
			c, err := s.Provider.Get(ctx, oid(r))
			if err != nil {
				return nil, err
			}
			if err := s.Provider.CheckConnectionActor(ctx, c, r.user.Username); err != nil {
				return nil, err
			}
			if c.Kind != "chatgpt" && r.user.Role != "admin" {
				return nil, clientError{403, "administrator permission required for shared connections"}
			}
		}
		if oid(r) == "" {
			return s.Provider.Create(ctx, b)
		}
		return s.Provider.Update(ctx, oid(r), b)
	}
	route("POST /connections", admin, saveConnection)
	route("PUT /connections/{id}", member, saveConnection)
	route("POST /connections/{id}/test", member, func(r *request) (any, error) {
		c, err := s.Provider.Get(r.Context(), oid(r))
		if err != nil {
			return nil, err
		}
		if err := s.Provider.CheckConnectionActor(r.Context(), c, r.user.Username); err != nil {
			return nil, err
		}
		if c.Kind != "chatgpt" && r.user.Role != "admin" {
			return nil, clientError{403, "administrator permission required for shared connections"}
		}
		return s.Provider.Test(r.Context(), oid(r))
	})
}
