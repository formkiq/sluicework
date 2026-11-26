package api

// Subscription sign-in and credentials are never hosted by the shared server.
// Keep retired routes explicit so older clients fail closed without forwarding tokens.
func (s *Server) chatGPTRoutes(route func(string, access, endpoint)) {
	for _, pattern := range []string{"GET /chatgpt/accounts", "POST /chatgpt/sign-in", "GET /chatgpt/sign-in/{id}", "POST /chatgpt/sign-in/{id}/cancel", "POST /connections/{id}/chatgpt/sign-out", "GET /connections/{id}/chatgpt/models", "POST /chatgpt/handoff/{id}/start", "POST /chatgpt/handoff/{id}/complete"} {
		route(pattern, public, func(r *request) (any, error) {
			return nil, clientError{410, "Subscription sign-in and controls moved to your OS-user-owned personal agent. The shared server never accepts subscription tokens."}
		})
	}
}
