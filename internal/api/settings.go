package api

import (
	"fmt"
	"net/http"

	"sluicework/internal/clusterconfig"
	"sluicework/internal/githubapp"
)

type attachment struct {
	Name, ContentType string
	Data              []byte
}

func (s *Server) configurationRoutes(route func(string, access, endpoint)) {
	ready := func() error {
		if s.Configuration == nil {
			return clientError{503, "Server setup is unavailable on this version"}
		}
		return nil
	}
	route("GET /settings", admin, func(r *request) (any, error) {
		if err := ready(); err != nil {
			return nil, err
		}
		return s.Configuration.Snapshot(), nil
	})
	route("PUT /settings/log-limit", admin, func(r *request) (any, error) {
		if err := ready(); err != nil {
			return nil, err
		}
		var input struct {
			Bytes int `json:"bytes"`
		}
		if err := decode(r, &input); err != nil {
			return nil, err
		}
		if err := s.Configuration.SetLogLimit(r.user.Username, input.Bytes); err != nil {
			return nil, err
		}
		return s.Configuration.Snapshot(), nil
	})
	route("PUT /settings/network", admin, func(r *request) (any, error) {
		if err := ready(); err != nil {
			return nil, err
		}
		var input clusterconfig.NetworkInput
		if err := decode(r, &input); err != nil {
			return nil, err
		}
		if err := s.Configuration.SetNetwork(r.user.Username, input); err != nil {
			return nil, err
		}
		return s.Configuration.Snapshot(), nil
	})
	route("PUT /settings/github", admin, func(r *request) (any, error) {
		if err := ready(); err != nil {
			return nil, err
		}
		var input clusterconfig.GitHubInput
		if err := decode(r, &input); err != nil {
			return nil, err
		}
		err := s.Configuration.SetGitHub(r.user.Username, input, func(requireIdle bool, next *githubapp.Broker, commit func() error) error {
			apply := func() error {
				if err := commit(); err != nil {
					return err
				}
				s.setBroker(next)
				return nil
			}
			if old := s.Broker(); old != nil {
				return old.Replace(requireIdle, apply)
			}
			if requireIdle {
				var exists, count int
				if err := s.State.DB.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='github_runner_leases'").Scan(&exists); err != nil {
					return err
				}
				if exists > 0 {
					if err := s.State.DB.QueryRow("SELECT count(*) FROM github_runner_leases").Scan(&count); err != nil {
						return err
					}
					if count > 0 {
						return clientError{409, "Restore the existing App connection and drain its runners before changing repository or App IDs"}
					}
				}
			}
			return apply()
		})
		if err != nil {
			return nil, err
		}
		return s.Configuration.Snapshot(), nil
	})
	route("POST /settings/github/test", admin, func(r *request) (any, error) {
		if err := ready(); err != nil {
			return nil, err
		}
		if err := s.Configuration.CheckGitHub(r.Context()); err != nil {
			return nil, clientError{http.StatusBadRequest, fmt.Sprintf("GitHub check failed: %s", err)}
		}
		return map[string]bool{"ok": true}, nil
	})
	route("GET /open-webui", member, func(r *request) (any, error) {
		if s.Configuration == nil {
			return clusterconfig.OpenWebUIStatus{}, nil
		}
		return s.Configuration.OpenWebUIStatus(r.Context()), nil
	})
	route("PUT /settings/open-webui", admin, func(r *request) (any, error) {
		if err := ready(); err != nil {
			return nil, err
		}
		var input clusterconfig.OpenWebUIInput
		if err := decode(r, &input); err != nil {
			return nil, err
		}
		if err := s.Configuration.SetOpenWebUI(r.user.Username, input); err != nil {
			return nil, err
		}
		return s.Configuration.Snapshot(), nil
	})
	route("POST /settings/open-webui/test", admin, func(r *request) (any, error) {
		if err := ready(); err != nil {
			return nil, err
		}
		var input struct {
			URL string `json:"url"`
		}
		if r.ContentLength != 0 {
			if err := decode(r, &input); err != nil {
				return nil, err
			}
		}
		target := input.URL
		if target == "" {
			if view := s.Configuration.Snapshot().OpenWebUI; view != nil {
				target = view.URL
			}
		}
		if target == "" {
			return nil, bad("configure an Open WebUI URL first")
		}
		if err := clusterconfig.ProbeOpenWebUI(r.Context(), target, ""); err != nil {
			return nil, clientError{http.StatusBadRequest, fmt.Sprintf("Open WebUI check failed: %s", err)}
		}
		return map[string]bool{"ok": true}, nil
	})
	route("GET /settings/worker-bundle", admin, func(r *request) (any, error) {
		if err := ready(); err != nil {
			return nil, err
		}
		data, err := s.Configuration.WorkerBundle(r.URL.Query().Get("name"), r.URL.Query().Get("url"), r.URL.Query().Get("os"), r.URL.Query().Get("arch"), r.URL.Query().Get("kind"))
		if err != nil {
			return nil, err
		}
		return attachment{Name: "Sluicework-worker-setup.zip", ContentType: "application/zip", Data: data}, nil
	})
}
