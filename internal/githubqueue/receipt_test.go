package githubqueue

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestReceiptTransitionsReuseCreatedIDWhileSearchIsStale(t *testing.T) {
	p, m, f := peerFixture(t)
	r := Request{ID: f.ID, Repository: "o/r", ParentID: 1, ParentAttempt: 1, CheckSHA: f.CheckSHA, State: "queued"}
	posts, searches, patches := 0, 0, 0
	var saved map[string]any
	m.call = func(method, path string, body any) (any, error) {
		switch {
		case method == "GET" && strings.Contains(path, "/commits/"):
			searches++
			// GitHub search has not indexed the first creation, even after PATCH.
			return map[string]any{"total_count": 0, "check_runs": []any{}}, nil
		case method == "POST" && strings.HasSuffix(path, "/check-runs"):
			posts++
			saved = body.(map[string]any)
			saved["id"] = int64(900)
			saved["app"] = map[string]any{"id": p.Config.AppID}
			return saved, nil
		case method == "GET" && strings.HasSuffix(path, "/check-runs/900"):
			return saved, nil
		case method == "PATCH" && strings.HasSuffix(path, "/check-runs/900"):
			patches++
			for k, v := range body.(map[string]any) {
				saved[k] = v
			}
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected request %s %s", method, path)
		}
	}
	for _, state := range []string{"queued", "dispatching", "running", "completed"} {
		r.State = state
		if err := p.Publish(context.Background(), r); err != nil {
			t.Fatal(state, err)
		}
	}
	if posts != 1 || searches != 1 || patches != 3 {
		t.Fatalf("posts=%d searches=%d patches=%d", posts, searches, patches)
	}
	// A changed cached identity must never cause another POST or update.
	saved["external_id"] = "different-request"
	if err := p.Publish(context.Background(), r); err == nil || posts != 1 || patches != 3 {
		t.Fatal("changed identity was accepted", err, posts, patches)
	}
}

func TestReceiptPublicationStillRejectsExistingDuplicates(t *testing.T) {
	p, m, f := peerFixture(t)
	r := Request{ID: f.ID, Repository: "o/r", ParentID: 1, ParentAttempt: 1, CheckSHA: f.CheckSHA}
	m.call = func(method, path string, body any) (any, error) {
		if method != "GET" || !strings.Contains(path, "/commits/") {
			t.Fatalf("duplicate receipt caused mutation: %s %s", method, path)
		}
		check := map[string]any{"id": 900, "app": map[string]any{"id": p.Config.AppID}, "name": "Sluicework queue", "head_sha": r.CheckSHA, "external_id": r.ExternalID()}
		return map[string]any{"total_count": 2, "check_runs": []any{check, check}}, nil
	}
	if err := p.Publish(context.Background(), r); err == nil {
		t.Fatal("ambiguous existing receipts were accepted")
	}
}
