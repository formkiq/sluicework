package githubapp

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type queueTransport func(*http.Request) (*http.Response, error)

func (f queueTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestQueueArtifactChecksDigestAndNeverForwardsTokenToStorage(t *testing.T) {
	for _, variant := range []string{"valid", "digest", "traversal", "extra", "origin"} {
		t.Run(variant, func(t *testing.T) {
			var b bytes.Buffer
			z := zip.NewWriter(&b)
			name := "sluicework-request.json"
			if variant == "traversal" {
				name = "../sluicework-request.json"
			}
			w, _ := z.Create(name)
			w.Write([]byte(`{"version":3}`))
			if variant == "extra" {
				z.Create("extra")
			}
			z.Close()
			sum := sha256.Sum256(b.Bytes())
			digest := "sha256:" + hex.EncodeToString(sum[:])
			if variant == "digest" {
				digest = "sha256:" + strings.Repeat("0", 64)
			}
			calls := 0
			c := &Client{config: Config{Repository: "o/r"}, origin: "https://api.github.com", managementTokens: map[string]cachedManagementToken{"priority-queue": {Value: "private-synthetic-token", Expires: time.Now().Add(time.Hour)}}}
			c.http = &http.Client{Transport: queueTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					if r.Header.Get("Authorization") != "Bearer private-synthetic-token" {
						t.Fatal("missing scoped token")
					}
					location := "https://test.blob.core.windows.net/signed"
					if variant == "origin" {
						location = "https://evil.invalid/payload"
					}
					return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{location}}, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				if r.Header.Get("Authorization") != "" || r.URL.Host != "test.blob.core.windows.net" {
					t.Fatal("credential forwarded")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b.Bytes()))}, nil
			}), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			data, err := c.QueueArtifact(context.Background(), 1, digest)
			if variant == "valid" {
				if err != nil || string(data) != `{"version":3}` || calls != 2 {
					t.Fatal(string(data), calls, err)
				}
			} else if err == nil {
				t.Fatal("invalid archive accepted")
			}
			if variant == "origin" && calls != 1 {
				t.Fatal("untrusted origin fetched")
			}
		})
	}
}
func TestQueueRequestsCannotEscapeRepositoryOrOrganization(t *testing.T) {
	c := &Client{config: Config{Repository: "o/r"}}
	for _, path := range []string{"/repos/evil/r/check-runs", "/orgs/evil/actions/runners", "https://evil.invalid/", "/repos/o/runner"} {
		if c.QueueRequest(context.Background(), "GET", path, nil, nil) == nil {
			t.Fatal(path)
		}
	}
}
func TestPriorityPolicyValidationPreservesLegacyTimeoutLimit(t *testing.T) {
	raw := `{"version":3,"placement":"prefer-local","platform":"linux-x64","machineLabel":"linux-runner-01","queueMinutes":60,"priority":"low","fallbackQueueMinutes":2,"fallbackRunner":{"platform":"macos-arm64","machineLabel":"mac-1"}}`
	if p, err := ParseRouting([]byte(raw)); err != nil || p.Priority != "low" {
		t.Fatal(p, err)
	}
	for _, minutes := range []string{"5", "15"} {
		high := strings.Replace(strings.Replace(raw, `"priority":"low"`, `"priority":"high"`, 1), `"queueMinutes":60`, `"queueMinutes":`+minutes, 1)
		if _, err := ParseRouting([]byte(high)); err != nil {
			t.Fatal("high policy unreadable during upgrade", err)
		}
	}
	for _, bad := range []string{strings.Replace(raw, `"queueMinutes":60`, `"queueMinutes":5`, 1), strings.Replace(raw, `"fallbackQueueMinutes":2`, `"fallbackQueueMinutes":60`, 1), strings.Replace(raw, `"priority":"low"`, `"priority":"urgent"`, 1), raw + ` {}`} {
		if _, err := ParseRouting([]byte(bad)); err == nil {
			t.Fatal("invalid priority accepted")
		}
	}
	if _, err := ParseRouting([]byte(`{"version":1,"placement":"prefer-local","platform":"linux-x64","machineLabel":"linux-runner-01","queueMinutes":60}`)); err == nil {
		t.Fatal("legacy hosted wait extended")
	}
}

func TestQueueRunnerGroupMetadataComesFromVisibleGroupInventory(t *testing.T) {
	c := &Client{config: Config{Repository: "o/r", OrganizationRunners: true}, origin: "https://api.github.com", managementTokens: map[string]cachedManagementToken{"priority-queue": {Value: "synthetic-token", Expires: time.Now().Add(time.Hour)}}}
	c.http = &http.Client{Transport: queueTransport(func(r *http.Request) (*http.Response, error) {
		body := ""
		switch r.URL.Path {
		case "/repos/o/r/actions/runners":
			body = `{"total_count":1,"runners":[{"id":1,"name":"repo-only"}]}`
		case "/orgs/o/actions/runner-groups":
			body = `{"total_count":2,"runner_groups":[{"id":1,"name":"Default"},{"id":2,"name":"Sluicework Spot"}]}`
		case "/orgs/o/actions/runner-groups/1/runners":
			body = `{"total_count":1,"runners":[{"id":2,"name":"linux-runner-02","runner_group_name":"forged"}]}`
		case "/orgs/o/actions/runner-groups/2/runners":
			body = `{"total_count":1,"runners":[{"id":3,"name":"sluicework-spot-one"}]}`
		default:
			t.Fatalf("unexpected inventory path %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	runners, err := c.QueueRunners(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]string{1: "", 2: "Default", 3: "Sluicework Spot"}
	if len(runners) != len(want) {
		t.Fatal(runners)
	}
	for _, runner := range runners {
		if runner.GroupName != want[runner.ID] {
			t.Fatalf("runner %d group=%q", runner.ID, runner.GroupName)
		}
	}
}
