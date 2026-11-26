package api

import "sluicework/internal/personal"

func (s *Server) personalRoutes(route func(string, access, endpoint)) {
	registry, openErr := personal.OpenRegistry(s.State.DB)
	ready := func() error {
		if openErr != nil {
			return clientError{503, "Personal agent registry is unavailable"}
		}
		return nil
	}
	route("POST /personal-agents/tickets", member, func(r *request) (any, error) {
		if err := ready(); err != nil {
			return nil, err
		}
		return registry.Ticket(r.Context(), r.user.Username)
	})
	route("POST /personal-agents/enroll", public, func(r *request) (any, error) {
		if err := ready(); err != nil {
			return nil, err
		}
		var body personal.Enrollment
		if err := decode(r, &body); err != nil {
			return nil, err
		}
		return registry.Enroll(r.Context(), body)
	})
	route("POST /personal-agents/sync", public, func(r *request) (any, error) {
		if err := ready(); err != nil {
			return nil, err
		}
		var body personal.Envelope
		if err := decode(r, &body); err != nil {
			return nil, err
		}
		return registry.Sync(r.Context(), body)
	})
	route("GET /personal-agents", member, func(r *request) (any, error) {
		if err := ready(); err != nil {
			return nil, err
		}
		return registry.Runtimes(r.Context(), r.user.Username)
	})
	route("GET /personal-tasks", member, func(r *request) (any, error) {
		if err := ready(); err != nil {
			return nil, err
		}
		return registry.Proposals(r.Context(), r.user.Username)
	})
	save := func(r *request) (any, error) {
		if err := ready(); err != nil {
			return nil, err
		}
		var body struct {
			RuntimeID string            `json:"runtimeId"`
			Manifest  personal.Manifest `json:"manifest"`
		}
		if err := decode(r, &body); err != nil {
			return nil, err
		}
		return registry.Save(r.Context(), r.user.Username, oid(r), body.RuntimeID, body.Manifest)
	}
	route("POST /personal-tasks", member, save)
	route("PUT /personal-tasks/{id}", member, save)
	// No remote pause/resume/approve/sign-in/stop/execute routes exist. The
	// controller can propose definitions and receive signed reports, never control
	// the owner-only local socket or invoke subscription credentials.
}
