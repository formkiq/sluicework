package api_test

import "testing"

func TestGitHubRunnerManagementRequiresCentralAdministrator(t *testing.T) {
	c := setup(t)
	worker := c.enroll("worker")
	for _, item := range []struct{ method, path string }{
		{"GET", "/github/spot"}, {"PUT", "/github/spot"}, {"GET", "/github/repositories"}, {"GET", "/github/runner-groups"}, {"GET", "/github/runners"}, {"GET", "/github/routing"}, {"PUT", "/github/routing"},
		{"POST", "/github/runners/registration"}, {"PUT", "/github/runners/repository/123/labels"}, {"DELETE", "/github/runners/organization/123"},
	} {
		c.do(item.method, item.path, map[string]any{}, c.operator, 403, nil)
		c.do(item.method, item.path, map[string]any{}, worker, 401, nil)
		c.do(item.method, item.path, map[string]any{}, c.admin, 409, nil)
	}
}
