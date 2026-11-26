package githubapp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type rateLimitedError struct {
	err   error
	until time.Time
}

func (e *rateLimitedError) Error() string      { return e.err.Error() }
func (e *rateLimitedError) Unwrap() error      { return e.err }
func (e *rateLimitedError) RetryAt() time.Time { return e.until }

// RetryAt returns the latest provider-requested pause in a wrapped/joined error.
// Permission denials never become rate-limit pauses.
func RetryAt(err error) time.Time {
	var latest time.Time
	var walk func(error)
	walk = func(err error) {
		if limited, ok := err.(interface{ RetryAt() time.Time }); ok && limited.RetryAt().After(latest) {
			latest = limited.RetryAt()
		}
		switch wrapped := err.(type) {
		case interface{ Unwrap() []error }:
			for _, child := range wrapped.Unwrap() {
				walk(child)
			}
		case interface{ Unwrap() error }:
			walk(wrapped.Unwrap())
		}
	}
	walk(err)
	return latest
}

// Classify provider rejections without exposing response bodies, request URLs,
// authorization headers, or provider-supplied links in logs.
func responseError(resp *http.Response) error {
	base := HTTPError(resp.StatusCode)
	if resp.StatusCode != 403 && resp.StatusCode != 429 {
		return base
	}
	var body struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body)
	message := strings.ToLower(body.Message)
	category := "unclassified rejection"
	switch {
	case strings.Contains(message, "secondary rate limit"):
		category = "secondary rate limit"
	case strings.Contains(message, "rate limit"):
		category = "API rate limit"
	case strings.Contains(message, "ip") && (strings.Contains(message, "allow list") || strings.Contains(message, "allowlist")):
		category = "organization IP access policy"
	case strings.Contains(message, "resource not accessible"):
		category = "resource permission rejected"
	case strings.Contains(message, "suspended"):
		category = "installation suspended"
	}
	details := []string{category}
	for _, field := range []struct{ header, label string }{{"X-RateLimit-Remaining", "remaining"}, {"X-RateLimit-Reset", "reset"}, {"Retry-After", "retryAfterSeconds"}} {
		value := resp.Header.Get(field.header)
		if n, e := strconv.ParseUint(value, 10, 64); e == nil {
			details = append(details, fmt.Sprintf("%s=%d", field.label, n))
		}
	}
	err := fmt.Errorf("%w (%s)", base, strings.Join(details, ", "))
	if category == "resource permission rejected" || category == "organization IP access policy" || category == "installation suspended" {
		return err
	}
	if resp.StatusCode != 429 && resp.Header.Get("X-RateLimit-Remaining") != "0" && category != "secondary rate limit" && category != "API rate limit" {
		return err
	}
	now := time.Now()
	until := now.Add(time.Minute)
	if reset, e := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); (resp.Header.Get("X-RateLimit-Remaining") == "0" || category == "API rate limit") && e == nil && reset > now.Unix() && reset <= now.Add(24*time.Hour).Unix() {
		until = time.Unix(reset, 0).Add(time.Second)
	}
	if seconds, e := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 64); e == nil && seconds > 0 && seconds <= 86400 {
		if retry := now.Add(time.Duration(seconds) * time.Second); retry.After(until) {
			until = retry
		}
	}
	return &rateLimitedError{err: err, until: until}
}
