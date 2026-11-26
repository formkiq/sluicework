package api_test

import "testing"

func TestSharedServerRejectsSubscriptionCredentialHandling(t *testing.T) {
	c := setup(t)
	for _, test := range []struct{ method, path string }{
		{"POST", "/chatgpt/sign-in"}, {"GET", "/chatgpt/accounts"}, {"POST", "/chatgpt/handoff/any/start"}, {"POST", "/chatgpt/handoff/any/complete"}, {"GET", "/connections/any/chatgpt/models"},
	} {
		c.do(test.method, test.path, map[string]any{}, c.admin, 410, nil)
	}
	for _, operation := range []string{"pause", "resume", "approve", "stop", "sign-in", "execute"} {
		c.do("POST", "/personal-agents/any/"+operation, map[string]any{}, c.admin, 404, nil)
	}
}
