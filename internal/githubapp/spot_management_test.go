package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sluicework/internal/spot"
	"sluicework/internal/state"
	"strings"
	"testing"
	"time"
)

type fakeSpotManagement struct {
	*fakeManagement
	health     *SpotHealth
	healthErr  error
	spotRaw    string
	spotExists bool
}

func (f *fakeSpotManagement) Variable(ctx context.Context, name string) (string, bool, error) {
	if name == spot.Variable {
		return f.spotRaw, f.spotExists, nil
	}
	return f.fakeManagement.Variable(ctx, name)
}
func (f *fakeSpotManagement) SpotHealth(context.Context, DispatcherBinding) (*SpotHealth, error) {
	return f.health, f.healthErr
}
func (f *fakeSpotManagement) SaveSpot(_ context.Context, raw string, _ bool) error {
	f.spotRaw = raw
	f.spotExists = true
	f.writes++
	return nil
}
func TestSpotAdminBoundsGlobalFleetAndRequiresReadinessAndFreshRevision(t *testing.T) {
	b, f, _ := managementFixture(t)
	f.integration.Version = 3
	f.integration.Dispatcher = &DispatcherBinding{AppID: 123, BackendID: "backend", HeartbeatSHA: strings.Repeat("a", 40)}
	m := &fakeSpotManagement{fakeManagement: f}
	b.management = m
	ctx := context.Background()
	out, e := b.Spot(ctx)
	if e != nil || out.Ready {
		t.Fatal(e, out)
	}
	settings := out.Settings
	settings.Enabled = true
	settings.Repositories = map[string]string{"team/pilot": "standard"}
	if _, e = b.PublishSpot(ctx, "admin", settings, out.Revision); !errors.Is(e, state.ErrInvalid) || f.writes != 0 {
		t.Fatal("unprovisioned fleet enabled", e)
	}
	m.health = &SpotHealth{Provisioned: true}
	out, e = b.Spot(ctx)
	if e != nil {
		t.Fatal(e)
	}
	settings.MaxConcurrent = 4
	if _, e = b.PublishSpot(ctx, "admin", settings, out.Revision); !errors.Is(e, state.ErrInvalid) {
		t.Fatal("cap four accepted", e)
	}
	settings.MaxConcurrent = 3
	settings.Repositories["other/repo"] = "small"
	if _, e = b.PublishSpot(ctx, "admin", settings, out.Revision); !errors.Is(e, state.ErrInvalid) {
		t.Fatal("foreign repo accepted", e)
	}
	delete(settings.Repositories, "other/repo")
	saved, e := b.PublishSpot(ctx, "admin", settings, out.Revision)
	if e != nil || saved.Settings.MaxConcurrent != 3 || f.writes != 1 {
		t.Fatal(e, saved)
	}
	if _, e = b.PublishSpot(ctx, "admin", settings, out.Revision); !errors.Is(e, state.ErrConflict) {
		t.Fatal("stale save overwrote fleet", e)
	}
	m.healthErr = errors.New("offline")
	settings.Enabled = false
	if _, e = b.PublishSpot(ctx, "admin", settings, saved.Revision); e != nil {
		t.Fatal("could not disable offline fleet", e)
	}
	var audits int
	if e = b.db.QueryRow("SELECT count(*) FROM audit WHERE action='publish shared AWS Spot runner settings'").Scan(&audits); e != nil || audits != 2 {
		t.Fatal("save missing audit", audits, e)
	}
}
func TestSpotHealthRejectsSpoofedAppAndStaleOrForeignHeartbeat(t *testing.T) {
	for _, test := range []string{"valid", "other-app", "foreign-backend", "stale", "duplicate"} {
		t.Run(test, func(t *testing.T) {
			h := SpotHealth{Version: 1, BackendID: "backend", Provisioned: true, At: time.Now().UTC().Format(time.RFC3339), HardLimit: 3}
			app := int64(123)
			if test == "other-app" {
				app = 999
			}
			if test == "foreign-backend" {
				h.BackendID = "foreign"
			}
			if test == "stale" {
				h.At = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
			}
			raw, _ := json.Marshal(h)
			entry := map[string]any{"head_sha": strings.Repeat("a", 40), "external_id": "sluicework-spot:backend", "status": "completed", "app": map[string]any{"id": app}, "output": map[string]string{"text": string(raw)}}
			checks := []any{entry}
			if test == "duplicate" {
				checks = append(checks, entry)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(checks), "check_runs": checks})
			}))
			defer server.Close()
			c := &Client{config: Config{AppID: 123, Repository: "team/pilot"}, origin: server.URL, http: server.Client(), managementTokens: map[string]cachedManagementToken{"priority-queue": {Value: "synthetic", Expires: time.Now().Add(time.Hour)}}}
			health, e := c.SpotHealth(context.Background(), DispatcherBinding{AppID: 123, BackendID: "backend", HeartbeatSHA: strings.Repeat("a", 40)})
			if test == "valid" {
				if e != nil || health == nil {
					t.Fatal(e)
				}
			} else if e == nil && health != nil {
				t.Fatal("untrusted heartbeat accepted", health)
			}
		})
	}
}
func TestOfficialRunnerDigestFallbackNeverSendsAppTokenToPublicRelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/team/actions/runners/downloads":
			if r.Header.Get("Authorization") == "" {
				t.Error("missing scoped organization token")
			}
			_ = json.NewEncoder(w).Encode([]RunnerApplication{{OS: "linux", Architecture: "arm64", DownloadURL: "https://github.com/actions/runner/releases/download/v2.338.2/actions-runner-linux-arm64-2.338.2.tar.gz"}})
		case "/repos/actions/runner/releases/tags/v2.338.2":
			if r.Header.Get("Authorization") != "" {
				t.Error("installation credential sent to public release")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v2.338.2", "assets": []map[string]string{{"name": "actions-runner-linux-arm64-2.338.2.tar.gz", "browser_download_url": "https://github.com/actions/runner/releases/download/v2.338.2/actions-runner-linux-arm64-2.338.2.tar.gz", "digest": "sha256:" + strings.Repeat("a", 64)}}})
		default:
			t.Error("unexpected path", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := &Client{config: Config{Repository: "team/pilot", OrganizationRunners: true}, origin: server.URL, http: server.Client(), managementTokens: map[string]cachedManagementToken{"organization_self_hosted_runners": {Value: "synthetic", Expires: time.Now().Add(time.Hour)}}}
	p, e := c.ChecksummedARMRunner(context.Background())
	if e != nil || p.SHA256 != strings.Repeat("a", 64) {
		t.Fatal(p, e)
	}
}
