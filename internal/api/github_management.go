package api

import (
	"errors"
	"strconv"

	"sluicework/internal/githubapp"
	"sluicework/internal/spot"
)

func githubManagementError(err error) error {
	if err == nil {
		return nil
	}
	var upstream githubapp.HTTPError
	if errors.As(err, &upstream) {
		return clientError{502, "GitHub rejected the request (HTTP " + strconv.Itoa(int(upstream)) + "). Check the App permissions, installation approval and repository or runner-group access."}
	}
	return err
}
func (s *Server) githubManagementRoutes(route func(string, access, endpoint)) {
	ready := func(r *request) (*githubapp.Broker, error) {
		b := s.Broker()
		if b == nil {
			return nil, clientError{409, "Configure a GitHub App connection in Settings first"}
		}
		return b.ForRepository(r.URL.Query().Get("repository"))
	}
	route("GET /github/spot", admin, func(r *request) (any, error) {
		b := s.Broker()
		if b == nil {
			return nil, clientError{409, "Configure a GitHub App connection in Settings first"}
		}
		out, e := b.Spot(r.Context())
		return out, githubManagementError(e)
	})
	route("PUT /github/spot", admin, func(r *request) (any, error) {
		b := s.Broker()
		if b == nil {
			return nil, clientError{409, "Configure a GitHub App connection in Settings first"}
		}
		var input struct {
			Settings spot.Settings `json:"settings"`
			Revision string        `json:"revision"`
		}
		if e := decode(r, &input); e != nil {
			return nil, e
		}
		out, e := b.PublishSpot(r.Context(), r.user.Username, input.Settings, input.Revision)
		return out, githubManagementError(e)
	})
	route("GET /github/runner-groups", admin, func(r *request) (any, error) {
		b, e := ready(r)
		if e != nil {
			return nil, e
		}
		groups, e := b.WorkstationGroups(r.Context())
		return groups, githubManagementError(e)
	})
	route("GET /github/repositories", admin, func(r *request) (any, error) {
		b := s.Broker()
		if b == nil {
			return nil, clientError{409, "Configure a GitHub App connection in Settings first"}
		}
		return map[string]any{"primaryRepository": b.PrimaryRepository(), "repositories": b.Repositories()}, nil
	})
	route("GET /github/runners", admin, func(r *request) (any, error) {
		b, e := ready(r)
		if e != nil {
			return nil, e
		}
		v, e := b.Inventory(r.Context())
		return v, githubManagementError(e)
	})
	route("PUT /github/runners/{scope}/{id}/labels", admin, func(r *request) (any, error) {
		b, e := ready(r)
		if e != nil {
			return nil, e
		}
		id, e := strconv.ParseInt(oid(r), 10, 64)
		if e != nil {
			return nil, bad("invalid runner ID")
		}
		var input struct {
			Labels []string `json:"labels"`
		}
		if e = decode(r, &input); e != nil {
			return nil, e
		}
		return nil, githubManagementError(b.UpdateExternalLabels(r.Context(), r.user.Username, r.PathValue("scope"), id, input.Labels))
	})
	route("DELETE /github/runners/{scope}/{id}", admin, func(r *request) (any, error) {
		b, e := ready(r)
		if e != nil {
			return nil, e
		}
		id, e := strconv.ParseInt(oid(r), 10, 64)
		if e != nil {
			return nil, bad("invalid runner ID")
		}
		var input struct {
			ConfirmName string `json:"confirmName"`
		}
		if e = decode(r, &input); e != nil {
			return nil, e
		}
		return nil, githubManagementError(b.RemoveExternal(r.Context(), r.user.Username, r.PathValue("scope"), id, input.ConfirmName))
	})
	route("POST /github/runners/registration", admin, func(r *request) (any, error) {
		b, e := ready(r)
		if e != nil {
			return nil, e
		}
		var input struct{ Scope, OS, Arch, Name, Label string }
		if e = decode(r, &input); e != nil {
			return nil, e
		}
		out, e := b.ExternalRegistration(r.Context(), r.user.Username, input.Scope, input.OS, input.Arch, input.Name, input.Label)
		return out, githubManagementError(e)
	})
	route("GET /github/routing", admin, func(r *request) (any, error) {
		b, e := ready(r)
		if e != nil {
			return nil, e
		}
		v, e := b.Routing(r.Context())
		if errors.Is(e, githubapp.HTTPError(404)) {
			return nil, clientError{409, "Install the workflow integration (.github/sluicework.json) before managing placement here. Grant Contents: read and Variables: write to the App."}
		}
		return v, githubManagementError(e)
	})
	route("PUT /github/routing", admin, func(r *request) (any, error) {
		b, e := ready(r)
		if e != nil {
			return nil, e
		}
		var input struct {
			Policy   githubapp.RoutingPolicy `json:"policy"`
			Revision string                  `json:"revision"`
		}
		if e = decode(r, &input); e != nil {
			return nil, e
		}
		v, e := b.PublishRouting(r.Context(), r.user.Username, input.Policy, input.Revision)
		return v, githubManagementError(e)
	})
}
