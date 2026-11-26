package provider

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sluicework/internal/state"
)

type planFixture struct {
	s                                   *Service
	key                                 *rsa.PrivateKey
	nonce                               string
	claimOverride                       map[string]any
	scope                               string
	stream                              string
	refreshes, generations, revocations atomic.Int32
	refreshBlock                        <-chan struct{}
	refreshStarted                      chan struct{}
	generationStarted                   chan struct{}
	generationBlock                     <-chan struct{}
}

func setupPlan(t *testing.T) *planFixture {
	t.Helper()
	s := setup(t)
	s.personalOwner = "alice"
	if _, err := s.db.Exec(`CREATE TABLE auth_users(username TEXT PRIMARY KEY,role TEXT,passhash BLOB); INSERT INTO auth_users VALUES('alice','admin',X'00')`); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &planFixture{s: s, key: key, scope: planScope, stream: "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"fixture summary\"}]}],\"usage\":{\"input_tokens\":12,\"output_tokens\":4}}}\n\n"}
	s.cloud = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host + r.URL.Path {
		case "auth.openai.com/.well-known/openid-configuration":
			return ok(`{"issuer":"https://auth.openai.com","jwks_uri":"https://auth.openai.com/jwks","revocation_endpoint":"https://auth.openai.com/revoke"}`), nil
		case "auth.openai.com/jwks":
			b, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "fixture", "use": "sig", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
			return ok(string(b)), nil
		case "auth.openai.com/api/accounts/oauth/token":
			if r.Method != "POST" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
				t.Error("invalid token exchange contract")
			}
			body, _ := io.ReadAll(r.Body)
			form, _ := url.ParseQuery(string(body))
			if form.Get("resource") != planResource || form.Get("client_id") != "oaiapp_fixture" {
				t.Error("wrong resource or issued client ID")
			}
			claims := map[string]any{"iss": planIssuer, "aud": "oaiapp_fixture", "sub": "account-fixture", "email": "alice@example.test", "nonce": f.nonce, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
			for name, value := range f.claimOverride {
				claims[name] = value
			}
			idToken := signedPlanJWT(t, key, claims)
			if form.Get("grant_type") == "refresh_token" {
				f.refreshes.Add(1)
				if f.refreshStarted != nil {
					close(f.refreshStarted)
				}
				if f.refreshBlock != nil {
					select {
					case <-f.refreshBlock:
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
				}
				if form.Has("scope") || form.Get("refresh_token") != "fixture-refresh" {
					t.Error("refresh changed grant or used stale token")
				}
				idToken = ""
			} else {
				if form.Get("code_verifier") == "" || form.Get("code") != "fixture-code" || !strings.HasPrefix(form.Get("redirect_uri"), "http://127.0.0.1:") {
					t.Error("incomplete PKCE exchange")
				}
			}
			b, _ := json.Marshal(planCredential{AccessToken: "fixture-access", RefreshToken: func() string {
				if form.Get("grant_type") == "refresh_token" {
					return "fixture-refresh-2"
				}
				return "fixture-refresh"
			}(), IDToken: idToken, TokenType: "Bearer", ExpiresIn: 3600, Scope: f.scope})
			return ok(string(b)), nil
		case "auth.openai.com/revoke":
			f.revocations.Add(1)
			body, _ := io.ReadAll(r.Body)
			form, _ := url.ParseQuery(string(body))
			expected := "fixture-refresh"
			if f.refreshes.Load() > 0 {
				expected = "fixture-refresh-2"
			}
			if form.Get("token") != expected || form.Get("token_type_hint") != "refresh_token" || form.Get("client_id") != "oaiapp_fixture" {
				t.Error("incorrect revocation")
			}
			return ok(""), nil
		case "api.openai.com/v1/models":
			if r.Header.Get("Authorization") != "Bearer fixture-access" {
				t.Error("wrong plan bearer credential")
			}
			return ok(`{"models":[{"slug":"fixture-plan-model","display_name":"Fixture model","visibility":"list"},{"slug":"hidden","visibility":"hidden"}]}`), nil
		case "api.openai.com/v1/responses":
			f.generations.Add(1)
			if f.generationStarted != nil {
				close(f.generationStarted)
			}
			if f.generationBlock != nil {
				select {
				case <-f.generationBlock:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if r.Header.Get("Authorization") != "Bearer fixture-access" || body["store"] != false || body["stream"] != true || body["max_output_tokens"] != nil {
				t.Error("invalid SIWC inference contract")
			}
			if _, ok := body["input"].([]any); !ok {
				t.Error("SIWC input must be an array")
			}
			return ok(f.stream), nil
		default:
			t.Fatalf("unexpected outbound request: host=%s path=%s", r.URL.Host, r.URL.Path)
		}
		return nil, nil
	})}
	return f
}

func signedPlanJWT(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(claims)
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"fixture"}`))
	unsigned := head + "." + base64.RawURLEncoding.EncodeToString(b)
	hash := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func (f *planFixture) start(t *testing.T, owner, connectionID string) (PlanSignIn, url.Values) {
	t.Helper()
	view, err := f.s.StartPlanSignIn(context.Background(), owner, connectionID, owner+" plan", true, true)
	if err != nil {
		t.Fatal(err)
	}
	start, err := f.s.StartPlanHandoff(view.AttemptID, view.HandoffSecret, "http://127.0.0.1:1455/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(start.AuthorizationURL)
	q := u.Query()
	f.nonce = q.Get("nonce")
	if connectionID != "" && view.FirstSignIn {
		t.Error("returning sign-in repeated the first-use welcome")
	}
	if q.Get("ext_agent_host_id") == "" || q.Get("code_challenge_method") != "S256" || q.Get("scope") != planScope || q.Get("resource") != planResource {
		t.Fatal("incomplete authorization request")
	}
	return view, url.Values{"code": {"fixture-code"}, "state": {q.Get("state")}, "client_id": {"oaiapp_fixture"}}
}

func (f *planFixture) connect(t *testing.T, owner string) Connection {
	t.Helper()
	view, callback := f.start(t, owner, "")
	result, err := f.s.CompletePlanHandoff(context.Background(), view.AttemptID, view.HandoffSecret, callback)
	if err != nil || result.Status != "succeeded" {
		t.Fatalf("sign-in: %+v %v", result, err)
	}
	c, err := f.s.Get(context.Background(), view.ConnectionID)
	if err != nil {
		t.Fatal(err)
	}
	c, err = f.s.Update(WithActor(context.Background(), owner), c.ID, Config{Name: c.Name, Kind: "chatgpt", Models: []string{"fixture-plan-model"}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func planRequest(t *testing.T, s *Service, c Connection, actor string) Request {
	t.Helper()
	st := &state.Store{DB: s.db}
	j, err := st.SaveJob(actor, "", state.Job{Name: "Plan task " + actor, Type: "ai", Role: "ai", Prompt: "fixture input", Connection: c.ID, Model: "fixture-plan-model", AllowCloud: true, MaxTokens: 128, TimeoutSeconds: 30, OutputLimit: 4096, OutputFormat: "text"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.Queue(actor, j.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	return Request{AttemptID: run.Attempts[0].ID, ConnectionID: c.ID, Model: "fixture-plan-model", Prompt: "fixture input", AllowCloud: true, MaxTokens: 128}
}

func TestChatGPTSignInRefreshInferenceAndSignOut(t *testing.T) {
	f := setupPlan(t)
	c := f.connect(t, "alice")
	ctx := context.Background()
	if c.Owner != "alice" || c.Disabled || c.Kind != "chatgpt" {
		t.Fatalf("wrong connection: %+v", c)
	}
	public, _ := json.Marshal(c)
	for _, secret := range []string{"fixture-access", "fixture-refresh", "id_token", "secret_ref"} {
		if strings.Contains(string(public), secret) {
			t.Fatal("public connection leaked credentials")
		}
	}
	if info, err := os.Stat(filepath.Join(f.s.secretDir, c.secretRef)); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential is not private")
	}
	models, err := f.s.PlanModels(ctx, "alice", c.ID)
	if err != nil || len(models) != 1 || models[0].ID != "fixture-plan-model" {
		t.Fatalf("catalog: %+v %v", models, err)
	}
	other, err := f.s.ConnectionsFor(ctx, "bob")
	if err != nil || len(other) != 0 {
		t.Fatal("another user can see the personal connection")
	}
	if _, err = f.s.PlanModels(ctx, "bob", c.ID); err == nil {
		t.Fatal("another user can use personal metadata credentials")
	}
	r := planRequest(t, f.s, c, "alice")
	result, err := f.s.Generate(ctx, r)
	if err != nil || result.Text != "fixture summary" || result.Call.InputTokens == nil || *result.Call.InputTokens != 12 {
		t.Fatalf("inference: %+v %v", result, err)
	}
	before := f.generations.Load()
	if _, err := f.s.Generate(ctx, planRequest(t, f.s, c, "bob")); err == nil || f.generations.Load() != before {
		t.Fatal("another user spent the subscription")
	}
	r = planRequest(t, f.s, c, "alice")
	r.AllowCloud = false
	if _, err := f.s.Generate(ctx, r); err == nil || f.generations.Load() != before {
		t.Fatal("local-only task reached ChatGPT")
	}
	r = planRequest(t, f.s, c, "alice")
	if _, err := f.s.db.Exec(`UPDATE runs SET schedule_id='fixture-schedule' WHERE id=(SELECT run_id FROM attempts WHERE id=?)`, r.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Generate(ctx, r); err == nil || f.generations.Load() != before {
		t.Fatal("ownerless schedule spent a subscription")
	}
	// Force an expired token, then race metadata calls: one refresh must rotate it.
	raw, _ := f.s.key(c.secretRef)
	var token planCredential
	_ = json.Unmarshal([]byte(raw), &token)
	token.ExpiresAt = time.Now().Add(-time.Minute)
	f.s.planMu.Lock()
	err = f.s.savePlanCredential(ctx, c.ID, token, "", "", "alice")
	f.s.planMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.s.PlanModels(ctx, "alice", c.ID); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.refreshes.Load() != 1 {
		t.Fatalf("refresh raced: %d", f.refreshes.Load())
	}
	current, _ := f.s.Get(ctx, c.ID)
	signOut, err := f.s.SignOutPlan(ctx, "alice", c.ID)
	if err != nil || signOut["revoked"] != true || f.revocations.Load() != 1 {
		t.Fatalf("sign-out: %v %v", signOut, err)
	}
	if _, err := os.Stat(filepath.Join(f.s.secretDir, current.secretRef)); !os.IsNotExist(err) {
		t.Fatal("sign-out kept credential file")
	}
	if _, err := f.s.PlanModels(ctx, "alice", c.ID); err == nil {
		t.Fatal("signed-out session still works")
	}
	view, _ := f.start(t, "alice", c.ID)
	if err := f.s.CancelPlanSignIn("alice", view.AttemptID); err != nil {
		t.Fatal(err)
	}
}

func TestChatGPTRejectsInvalidAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name   string
		claims map[string]any
		scope  string
	}{
		{name: "wrong audience", claims: map[string]any{"aud": "other-client"}},
		{name: "wrong issuer", claims: map[string]any{"iss": "https://example.test"}},
		{name: "wrong nonce", claims: map[string]any{"nonce": "other-attempt"}},
		{name: "expired identity", claims: map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}},
		{name: "no plan permission", scope: "openid profile email offline_access resource.invoke"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupPlan(t)
			f.claimOverride = tc.claims
			if tc.scope != "" {
				f.scope = tc.scope
			}
			view, callback := f.start(t, "alice", "")
			result, err := f.s.CompletePlanHandoff(context.Background(), view.AttemptID, view.HandoffSecret, callback)
			if err != nil || result.Status != "failed" {
				t.Fatalf("invalid authorization accepted: %+v %v", result, err)
			}
			c, _ := f.s.Get(context.Background(), view.ConnectionID)
			if !c.Disabled || c.secretRef != "" {
				t.Fatal("failed authorization replaced credentials")
			}
		})
	}
}

func TestChatGPTHandoffIsolationAndCancellation(t *testing.T) {
	f := setupPlan(t)
	ctx := context.Background()
	view, callback := f.start(t, "alice", "")
	if _, err := f.s.PlanSignInStatus("bob", view.AttemptID); err == nil {
		t.Fatal("another user can inspect sign-in")
	}
	if _, err := f.s.CompletePlanHandoff(ctx, view.AttemptID, "wrong", callback); err == nil {
		t.Fatal("wrong capability accepted")
	}
	bad := url.Values{"code": {"fixture-code"}, "client_id": {"oaiapp_fixture"}, "state": {"wrong-state"}}
	if _, err := f.s.CompletePlanHandoff(ctx, view.AttemptID, view.HandoffSecret, bad); err == nil {
		t.Fatal("wrong state accepted")
	}
	if err := f.s.CancelPlanSignIn("bob", view.AttemptID); err == nil {
		t.Fatal("another user cancelled sign-in")
	}
	if err := f.s.CancelPlanSignIn("alice", view.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.CompletePlanHandoff(ctx, view.AttemptID, view.HandoffSecret, callback); err == nil {
		t.Fatal("cancelled sign-in accepted")
	}
	for _, redirect := range []string{"https://example.test/auth/callback", "http://localhost:1455/auth/callback", "http://127.0.0.1:1455/other", "http://127.0.0.1:1455/auth/callback?x=1"} {
		v, err := f.s.StartPlanSignIn(ctx, "alice", "", "Alice "+redirect, true, true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.StartPlanHandoff(v.AttemptID, v.HandoffSecret, redirect); err == nil {
			t.Fatalf("untrusted callback accepted: %s", redirect)
		}
	}
}

func TestChatGPTStreamRequiresTerminalCompletion(t *testing.T) {
	for _, stream := range []string{
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n",
		"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"subscription_sharing_usage_limit_exceeded\",\"message\":\"private material\"}}}\n\n",
		"data: {\"type\":\"response.incomplete\"}\n\n",
		"data: not-json\n\n",
		strings.Repeat("x", 1<<20),
	} {
		result, err := readPlanStream(strings.NewReader(stream))
		if err == nil || result.text != "" || strings.Contains(err.Error(), "private material") {
			t.Fatal("incomplete or private stream accepted")
		}
	}
}

func TestChatGPTDisableWinsOverInFlightRefresh(t *testing.T) {
	f := setupPlan(t)
	c := f.connect(t, "alice")
	ctx := context.Background()
	raw, _ := f.s.key(c.secretRef)
	var token planCredential
	_ = json.Unmarshal([]byte(raw), &token)
	token.ExpiresAt = time.Now().Add(-time.Minute)
	f.s.planMu.Lock()
	err := f.s.savePlanCredential(ctx, c.ID, token, "", "", "alice")
	f.s.planMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	f.refreshBlock = release
	f.refreshStarted = make(chan struct{})
	finished := make(chan error, 1)
	go func() { _, err := f.s.PlanModels(ctx, "alice", c.ID); finished <- err }()
	select {
	case <-f.refreshStarted:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("refresh did not start")
	}
	_, err = f.s.Update(WithActor(ctx, "alice"), c.ID, Config{Name: c.Name, Kind: "chatgpt", Models: c.Models, Disabled: true})
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err == nil {
		t.Fatal("disabled connection completed a refresh")
	}
	current, _ := f.s.Get(ctx, c.ID)
	if !current.Disabled {
		t.Fatal("refresh undid the user's disable")
	}
}

func TestChatGPTSignOutCancelsActiveGeneration(t *testing.T) {
	f := setupPlan(t)
	c := f.connect(t, "alice")
	f.generationStarted = make(chan struct{})
	f.generationBlock = make(chan struct{})
	r := planRequest(t, f.s, c, "alice")
	finished := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _, err := f.s.Generate(ctx, r); finished <- err }()
	select {
	case <-f.generationStarted:
	case <-ctx.Done():
		t.Fatal("generation did not start")
	}
	if _, err := f.s.SignOutPlan(ctx, "alice", c.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("signed-out generation succeeded")
		}
	case <-ctx.Done():
		t.Fatal("sign-out did not cancel active generation")
	}
}

func TestChatGPTReturningSignInPreservesIdentityAndNewerSettings(t *testing.T) {
	f := setupPlan(t)
	c := f.connect(t, "alice")
	view, callback := f.start(t, "alice", c.ID)
	f.claimOverride = map[string]any{"sub": "another-person"}
	result, err := f.s.CompletePlanHandoff(context.Background(), view.AttemptID, view.HandoffSecret, callback)
	if err != nil || result.Status != "failed" {
		t.Fatal("returning sign-in changed the account identity")
	}
	current, _ := f.s.Get(context.Background(), c.ID)
	if current.secretRef != c.secretRef {
		t.Fatal("failed sign-in replaced an active session")
	}
	f.claimOverride = nil
	view, callback = f.start(t, "alice", c.ID)
	_, err = f.s.Update(WithActor(context.Background(), "alice"), c.ID, Config{Name: c.Name, Kind: "chatgpt", Models: c.Models, Disabled: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err = f.s.CompletePlanHandoff(context.Background(), view.AttemptID, view.HandoffSecret, callback)
	if err != nil || result.Status != "failed" {
		t.Fatal("sign-in ignored newer connection settings")
	}
	current, _ = f.s.Get(context.Background(), c.ID)
	if !current.Disabled || current.secretRef != c.secretRef {
		t.Fatal("sign-in overwrote newer settings")
	}
}
