package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCodingTimeoutReportsDeadlineInsteadOfGenericBadGateway(t *testing.T) {
	w := httptest.NewRecorder()
	harnessError(w, fmt.Errorf("request: %w", context.DeadlineExceeded))
	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("timeout reported as %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Error struct{ Message, Type string }
	}
	if e := json.Unmarshal(w.Body.Bytes(), &body); e != nil {
		t.Fatal(e)
	}
	if body.Error.Type != "timeout_error" || !strings.Contains(body.Error.Message, "request timeout") || !strings.Contains(body.Error.Message, "smaller input/output") {
		t.Fatal("timeout is not actionable", w.Body.String())
	}
}
