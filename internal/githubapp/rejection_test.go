package githubapp

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRejectionDiagnosticsPreserveStatusAndExcludeProviderSecrets(t *testing.T) {
	for _, tt := range []struct{ message, want string }{{"You have exceeded a secondary rate limit. SECRET", "secondary rate limit"}, {"IP address is not allowed by the organization IP allow list. SECRET", "organization IP access policy"}, {"Resource not accessible by integration SECRET", "resource permission rejected"}, {"SECRET", "unclassified rejection"}} {
		resp := &http.Response{StatusCode: 403, Header: http.Header{"X-Ratelimit-Remaining": []string{"0"}, "X-Ratelimit-Reset": []string{"1234"}, "Retry-After": []string{"60"}}, Body: io.NopCloser(strings.NewReader(`{"message":"` + tt.message + `"}`))}
		e := responseError(resp)
		if !errors.Is(e, HTTPError(403)) || !strings.Contains(e.Error(), tt.want) || strings.Contains(e.Error(), "SECRET") {
			t.Fatal(e)
		}
		if !strings.Contains(e.Error(), "remaining=0") || !strings.Contains(e.Error(), "retryAfterSeconds=60") {
			t.Fatal(e)
		}
	}
	resp := &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("SECRET"))}
	if responseError(resp) != HTTPError(404) {
		t.Fatal("404 compatibility")
	}
}

func TestRateLimitResetSurvivesWrappedErrorsWithoutMaskingPermissionDenials(t *testing.T) {
	now := time.Now()
	reset := now.Add(20 * time.Minute).Truncate(time.Second)
	response := func(message, remaining, retry string) *http.Response {
		return &http.Response{StatusCode: 403, Header: http.Header{"X-Ratelimit-Remaining": []string{remaining}, "X-Ratelimit-Reset": []string{strconv.FormatInt(reset.Unix(), 10)}, "Retry-After": []string{retry}}, Body: io.NopCloser(strings.NewReader(`{"message":"` + message + `"}`))}
	}
	primary := responseError(response("API rate limit exceeded", "0", ""))
	if until := RetryAt(fmt.Errorf("queue: %w", primary)); !until.Equal(reset.Add(time.Second)) {
		t.Fatal(until)
	}
	secondary := responseError(response("You have exceeded a secondary rate limit", "4900", "120"))
	if until := RetryAt(secondary); until.Before(now.Add(119*time.Second)) || until.After(now.Add(125*time.Second)) {
		t.Fatal("secondary limit used hourly reset", until)
	}
	if until := RetryAt(errors.Join(secondary, primary)); !until.Equal(reset.Add(time.Second)) {
		t.Fatal("joined backoff shortened", until)
	}
	for _, message := range []string{"Resource not accessible by integration", "IP address denied by IP allow list", "Installation suspended"} {
		if until := RetryAt(responseError(response(message, "0", "60"))); !until.IsZero() {
			t.Fatal("permission denial became a transient pause", until)
		}
	}
}
