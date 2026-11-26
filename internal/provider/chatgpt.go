package provider

// ChatGPT plan usage uses OpenAI's public SIWC protocol. It deliberately does
// not read Codex/ChatGPT credentials or use their private backend endpoints.
import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"

	"sluicework/internal/state"
)

const chatGPTSchema = `CREATE TABLE IF NOT EXISTS chatgpt_accounts (
 connection_id TEXT PRIMARY KEY, owner TEXT NOT NULL, client_id TEXT NOT NULL DEFAULT '',
 subject TEXT NOT NULL DEFAULT '', email TEXT NOT NULL DEFAULT ''
);`
const planIssuer = "https://auth.openai.com"
const planResource = "https://api.openai.com/v1"
const planScope = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"

type planCredential struct {
	AccessToken     string    `json:"access_token"`
	RefreshToken    string    `json:"refresh_token"`
	IDToken         string    `json:"id_token"`
	TokenType       string    `json:"token_type"`
	Scope           string    `json:"scope"`
	ExpiresIn       int64     `json:"expires_in"`
	EarliestRefresh int64     `json:"earliest_refresh_at"`
	ExpiresAt       time.Time `json:"expires_at"`
}

type PlanAccount struct {
	ConnectionID      string `json:"connection_id"`
	Owner             string `json:"owner"`
	Email             string `json:"email"`
	SignedIn          bool   `json:"signed_in"`
	clientID, subject string
}

type PlanSignIn struct {
	AttemptID        string    `json:"attempt_id"`
	ConnectionID     string    `json:"connection_id"`
	AuthorizationURL string    `json:"authorization_url,omitempty"`
	HandoffSecret    string    `json:"handoff_secret,omitempty"`
	Status           string    `json:"status"`
	Message          string    `json:"message,omitempty"`
	ExpiresAt        time.Time `json:"expires_at"`
	FirstSignIn      bool      `json:"first_sign_in,omitempty"`
}
type planAttempt struct {
	view                                             PlanSignIn
	owner, state, nonce, verifier, redirect, handoff string
	finished                                         bool
	revision                                         int64
	account                                          PlanAccount
	stop                                             context.CancelFunc
}

// Personal plan credentials belong to a cluster user, not to the cluster.
// Repeat the account-existence check on refresh and inference.
func (s *Service) planOwner(ctx context.Context, owner string) error {
	if s.personalOwner == "" || s.personalOwner != owner {
		return fmt.Errorf("%w: subscription credentials require the owner's private personal runtime", ErrConflict)
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT count(*)=1 AND min(username)=? FROM auth_users`, owner).Scan(&exists); err != nil {
		return errors.New("cannot verify ChatGPT account owner")
	}
	if !exists {
		return fmt.Errorf("%w: ChatGPT account owner no longer exists; sign in with your own account", ErrConflict)
	}
	return nil
}

func (s *Service) PlanAccounts(ctx context.Context) ([]PlanAccount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.connection_id,a.owner,a.email,(c.secret_ref<>'' AND c.disabled=0) FROM chatgpt_accounts a JOIN provider_connections c ON c.id=a.connection_id ORDER BY c.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	accounts := []PlanAccount{}
	for rows.Next() {
		var a PlanAccount
		if err := rows.Scan(&a.ConnectionID, &a.Owner, &a.Email, &a.SignedIn); err != nil {
			return nil, err
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

func (s *Service) planAccount(ctx context.Context, connectionID string) (PlanAccount, error) {
	a := PlanAccount{ConnectionID: connectionID}
	err := s.db.QueryRowContext(ctx, `SELECT owner,client_id,subject,email FROM chatgpt_accounts WHERE connection_id=?`, connectionID).Scan(&a.Owner, &a.clientID, &a.subject, &a.Email)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return a, err
}

func randomPlanValue() (string, error) {
	b := make([]byte, 32)
	_, err := rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b), err
}

// This identity is host-local and excluded from database backups with the
// credentials. Restoring a cluster onto another machine creates a new host ID.
func (s *Service) planHostID() (string, error) {
	path := filepath.Join(s.secretDir, "chatgpt-host-id")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return "", errors.New("ChatGPT host identity must be a private regular file")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", errors.New("cannot read ChatGPT host identity")
		}
		if _, err := uuid.Parse(strings.TrimPrefix(string(b), "urn:uuid:")); err != nil {
			return "", errors.New("invalid ChatGPT host identity")
		}
		return string(b), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("cannot read ChatGPT host identity")
	}
	newID, err := uuid.NewRandom()
	if err != nil {
		return "", errors.New("cannot create ChatGPT host identity")
	}
	host := newID.URN()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", errors.New("cannot create ChatGPT host identity")
	}
	_, err = f.WriteString(host)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		os.Remove(path)
		return "", errors.New("cannot persist ChatGPT host identity")
	}
	return host, nil
}

// Direct sign-in requires a browser on the orchestrator. A remote user uses
// the scoped native helper instead; both callbacks are loopback HTTP.
func (s *Service) StartPlanSignIn(ctx context.Context, owner, connectionID, name string, consent, helper bool) (PlanSignIn, error) {
	if !consent {
		return PlanSignIn{}, fmt.Errorf("%w: confirm use of your own subscription in an eligible Sluicework deployment before signing in", ErrInvalid)
	}
	if err := s.planOwner(ctx, owner); err != nil {
		return PlanSignIn{}, err
	}
	s.planMu.Lock()
	defer s.planMu.Unlock()
	if s.planClosed {
		return PlanSignIn{}, ErrConflict
	}
	if s.planAttempts == nil {
		s.planAttempts = map[string]*planAttempt{}
	}
	// Expired attempts retain no verifier, nonce or authorization URL.
	for id, a := range s.planAttempts {
		if time.Now().After(a.view.ExpiresAt) {
			a.stop()
			delete(s.planAttempts, id)
		}
	}
	owned := 0
	for _, a := range s.planAttempts {
		if a.owner == owner {
			owned++
		}
	}
	if owned >= 8 || len(s.planAttempts) >= 128 {
		return PlanSignIn{}, fmt.Errorf("%w: too many sign-in attempts; wait for pending attempts to expire", ErrConflict)
	}
	var account PlanAccount
	var err error
	if connectionID == "" {
		name = strings.TrimSpace(name)
		if name == "" || len(name) > 128 || strings.ContainsAny(name, "\r\n\x00") {
			return PlanSignIn{}, fmt.Errorf("%w: provide a connection name", ErrInvalid)
		}
		connectionID, err = id()
		if err != nil {
			return PlanSignIn{}, err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return PlanSignIn{}, err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `INSERT INTO provider_connections(id,name,kind,endpoint,models,disabled,secret_ref) VALUES(?,?,'chatgpt',?,'[]',1,'')`, connectionID, name, endpoints["chatgpt"]); err != nil {
			return PlanSignIn{}, fmt.Errorf("%w: connection name is already used or cannot be saved", ErrConflict)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO chatgpt_accounts(connection_id,owner) VALUES(?,?)`, connectionID, owner); err != nil {
			return PlanSignIn{}, err
		}
		if err = state.AuditTx(tx, owner, "create ChatGPT plan connection", connectionID); err != nil {
			return PlanSignIn{}, err
		}
		if err = tx.Commit(); err != nil {
			return PlanSignIn{}, err
		}
	}
	account, err = s.planAccount(ctx, connectionID)
	if err != nil {
		return PlanSignIn{}, err
	}
	if account.Owner != owner {
		return PlanSignIn{}, ErrNotFound
	}
	connection, err := s.Get(ctx, connectionID)
	if err != nil {
		return PlanSignIn{}, err
	}
	for _, a := range s.planAttempts {
		if a.account.ConnectionID == connectionID && (a.view.Status == "pending" || a.view.Status == "processing") {
			return PlanSignIn{}, fmt.Errorf("%w: sign-in already in progress", ErrConflict)
		}
	}
	host, err := s.planHostID()
	if err != nil {
		return PlanSignIn{}, err
	}
	values := make([]string, 5)
	for i := range values {
		values[i], err = randomPlanValue()
		if err != nil {
			return PlanSignIn{}, err
		}
	}
	deadline := time.Now().Add(10 * time.Minute)
	life, cancel := context.WithDeadline(context.Background(), deadline)
	a := &planAttempt{owner: owner, state: values[1], nonce: values[2], verifier: values[3], account: account, stop: cancel}
	a.revision = connection.revision
	a.view = PlanSignIn{AttemptID: values[0], ConnectionID: connectionID, Status: "pending", ExpiresAt: deadline}
	var previouslyConnected bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM chatgpt_accounts WHERE owner=? AND subject<>'')`, owner).Scan(&previouslyConnected); err != nil {
		cancel()
		return PlanSignIn{}, err
	}
	a.view.FirstSignIn = !previouslyConnected
	s.planAttempts[a.view.AttemptID] = a
	go func() {
		<-life.Done()
		s.planMu.Lock()
		defer s.planMu.Unlock()
		if a.view.Status == "pending" {
			a.view.Status = "expired"
			a.view.Message = "Sign-in expired. Start again."
			a.finished = true
			a.nonce, a.verifier, a.handoff = "", "", ""
		}
	}()
	if helper {
		a.handoff = values[4]
		view := a.view
		view.HandoffSecret = a.handoff
		return view, nil
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		cancel()
		delete(s.planAttempts, a.view.AttemptID)
		return PlanSignIn{}, errors.New("cannot start local ChatGPT callback listener")
	}
	a.redirect = "http://" + listener.Addr().String() + "/auth/callback"
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 90 * time.Second, MaxHeaderBytes: 16 << 10, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.planCallback(life, a, w, r) })}
	go func() { <-life.Done(); server.Close() }()
	go func() { _ = server.Serve(listener); cancel() }()
	view := a.view
	view.AuthorizationURL = planAuthorization(a, host)
	return view, nil
}

func planAuthorization(a *planAttempt, host string) string {
	client := a.account.clientID
	q := url.Values{"client_id": {client}, "ext_agent_host_id": {host}, "response_type": {"code"}, "redirect_uri": {a.redirect}, "scope": {planScope}, "resource": {planResource}, "state": {a.state}, "nonce": {a.nonce}, "code_challenge_method": {"S256"}}
	if client == "" {
		q.Set("client_id", "dynamic_agent_client")
		q.Set("agent_name_hint", "Sluicework")
	}
	hash := sha256.Sum256([]byte(a.verifier))
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(hash[:]))
	return planIssuer + "/api/accounts/authorize?" + q.Encode()
}

func (s *Service) handoffAttempt(attemptID, secret string) (*planAttempt, error) {
	a := s.planAttempts[attemptID]
	if a == nil || secret == "" || a.handoff == "" || subtle.ConstantTimeCompare([]byte(secret), []byte(a.handoff)) != 1 || a.finished || time.Now().After(a.view.ExpiresAt) {
		return nil, ErrNotFound
	}
	return a, nil
}

// The helper only transports this attempt's callback. OAuth tokens and PKCE
// proofs stay on the orchestrator; no general-purpose session is issued.
func (s *Service) StartPlanHandoff(attemptID, secret, redirect string) (PlanSignIn, error) {
	s.planMu.Lock()
	defer s.planMu.Unlock()
	a, err := s.handoffAttempt(attemptID, secret)
	if err != nil {
		return PlanSignIn{}, err
	}
	u, err := url.Parse(redirect)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.Path != "/auth/callback" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return PlanSignIn{}, fmt.Errorf("%w: helper callback must be HTTP on 127.0.0.1 with path /auth/callback", ErrInvalid)
	}
	if a.redirect != "" || a.view.Status != "pending" {
		return PlanSignIn{}, ErrConflict
	}
	host, err := s.planHostID()
	if err != nil {
		return PlanSignIn{}, err
	}
	a.redirect = redirect
	view := a.view
	view.AuthorizationURL = planAuthorization(a, host)
	return view, nil
}

func (s *Service) CompletePlanHandoff(ctx context.Context, attemptID, secret string, callback url.Values) (PlanSignIn, error) {
	s.planMu.Lock()
	a, err := s.handoffAttempt(attemptID, secret)
	if err != nil {
		s.planMu.Unlock()
		return PlanSignIn{}, err
	}
	if a.redirect == "" || a.view.Status != "pending" || len(callback["state"]) != 1 || subtle.ConstantTimeCompare([]byte(callback.Get("state")), []byte(a.state)) != 1 {
		s.planMu.Unlock()
		return PlanSignIn{}, ErrInvalid
	}
	a.view.Status = "processing"
	s.planMu.Unlock()
	finishCtx, cancel := context.WithTimeout(ctx, time.Until(a.view.ExpiresAt))
	defer cancel()
	err = s.completePlan(finishCtx, a, callback)
	s.finishPlan(a, err)
	return s.PlanSignInStatus(a.owner, a.view.AttemptID)
}

func (s *Service) PlanSignInStatus(owner, attemptID string) (PlanSignIn, error) {
	s.planMu.Lock()
	defer s.planMu.Unlock()
	a := s.planAttempts[attemptID]
	if a == nil || a.owner != owner {
		return PlanSignIn{}, ErrNotFound
	}
	view := a.view
	if time.Now().After(view.ExpiresAt) && (view.Status == "pending" || view.Status == "processing") {
		view.Status = "expired"
		view.Message = "Sign-in expired. Start again."
	}
	return view, nil
}

func (s *Service) CancelPlanSignIn(owner, attemptID string) error {
	s.planMu.Lock()
	defer s.planMu.Unlock()
	a := s.planAttempts[attemptID]
	if a == nil || a.owner != owner {
		return ErrNotFound
	}
	if a.finished {
		return nil
	}
	a.stop()
	a.finished = true
	a.handoff = ""
	a.view.Status = "cancelled"
	a.view.Message = "Sign-in cancelled."
	return nil
}

func (s *Service) planCallback(life context.Context, a *planAttempt, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	u, _ := url.Parse(a.redirect)
	query, err := url.ParseQuery(r.URL.RawQuery)
	if r.Method != "GET" || r.URL.Path != "/auth/callback" || r.Host != u.Host || err != nil || len(r.URL.RawQuery) > 16384 || len(query["state"]) != 1 || subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(a.state)) != 1 {
		http.Error(w, "Invalid sign-in callback.", 400)
		return
	}
	s.planMu.Lock()
	if a.view.Status != "pending" || life.Err() != nil {
		s.planMu.Unlock()
		http.Error(w, "Sign-in attempt is no longer pending.", 400)
		return
	}
	a.view.Status = "processing"
	s.planMu.Unlock()
	ctx, cancel := context.WithTimeout(life, 60*time.Second)
	defer cancel()
	err = s.completePlan(ctx, a, query)
	view := s.finishPlan(a, err)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, view.Message)
	time.AfterFunc(time.Second, a.stop)
}

func (s *Service) finishPlan(a *planAttempt, err error) PlanSignIn {
	s.planMu.Lock()
	defer s.planMu.Unlock()
	a.verifier, a.nonce, a.handoff = "", "", ""
	a.finished = true
	if a.view.Status == "cancelled" {
		return a.view
	}
	a.view.Status = "succeeded"
	a.view.Message = "Signed in. Return to Sluicework to choose permitted models."
	if err != nil {
		a.view.Status = "failed"
		a.view.Message = err.Error()
	}
	return a.view
}

func (s *Service) completePlan(ctx context.Context, a *planAttempt, query url.Values) error {
	if query.Get("error") != "" {
		return errors.New("ChatGPT sign-in was declined or plan usage was not authorized. Try again after reviewing permissions.")
	}
	if len(query["code"]) != 1 || query.Get("code") == "" || len(query["client_id"]) > 1 {
		return errors.New("ChatGPT registration returned an incomplete callback. Start again.")
	}
	client := a.account.clientID
	returned := query.Get("client_id")
	if client == "" {
		if returned == "" || returned == "dynamic_agent_client" || len(returned) > 256 || strings.ContainsAny(returned, "\r\n\x00 ") {
			return errors.New("ChatGPT registration did not return an issued client ID. Start again.")
		}
		client = returned
		// Preserve the issued registration even if the one-use code expires.
		if _, err := s.db.ExecContext(ctx, `UPDATE chatgpt_accounts SET client_id=? WHERE connection_id=? AND client_id=''`, client, a.account.ConnectionID); err != nil {
			return errors.New("Cannot retain ChatGPT registration. Start again.")
		}
	} else if returned != "" && returned != client {
		return errors.New("ChatGPT returned a different account registration. Start again.")
	}
	token, err := s.planToken(ctx, url.Values{"grant_type": {"authorization_code"}, "client_id": {client}, "code": {query.Get("code")}, "code_verifier": {a.verifier}, "redirect_uri": {a.redirect}, "resource": {planResource}})
	if err != nil {
		return err
	}
	identity, err := s.verifyPlanID(ctx, token.IDToken, client, a.nonce)
	if err != nil {
		return err
	}
	if a.account.subject != "" && identity.Subject != a.account.subject {
		return errors.New("ChatGPT identity does not match this saved account. Add a separate connection.")
	}
	s.planMu.Lock()
	err = s.planOwner(ctx, a.owner)
	if ctx.Err() != nil || a.view.Status != "processing" {
		err = errors.New("ChatGPT sign-in was cancelled")
	}
	if current, getErr := s.Get(ctx, a.account.ConnectionID); getErr != nil || current.revision != a.revision {
		err = errors.New("ChatGPT connection changed during sign-in; start again")
	}
	if err == nil {
		err = s.savePlanCredential(ctx, a.account.ConnectionID, token, identity.Subject, identity.Email, a.owner, a.revision)
	}
	s.planMu.Unlock()
	return err
}

type planIdentity struct {
	Subject string `json:"sub"`
	Email   string `json:"email"`
}

// Only the fixed official issuer may supply verification or revocation URLs.
func (s *Service) planDiscovery(ctx context.Context) (map[string]string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", planIssuer+"/.well-known/openid-configuration", nil)
	res, err := s.cloud.Do(req)
	if err != nil {
		return nil, errors.New("ChatGPT identity service is unreachable")
	}
	defer res.Body.Close()
	var raw struct {
		Issuer string `json:"issuer"`
		JWKS   string `json:"jwks_uri"`
		Revoke string `json:"revocation_endpoint"`
	}
	if res.StatusCode != 200 || json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&raw) != nil || raw.Issuer != planIssuer {
		return nil, errors.New("invalid ChatGPT identity configuration")
	}
	for _, endpoint := range []string{raw.JWKS, raw.Revoke} {
		u, err := url.Parse(endpoint)
		if err != nil || u.Scheme != "https" || u.Host != "auth.openai.com" || u.User != nil || u.Fragment != "" {
			return nil, errors.New("untrusted ChatGPT identity endpoint")
		}
	}
	return map[string]string{"jwks": raw.JWKS, "revoke": raw.Revoke}, nil
}

func (s *Service) verifyPlanID(ctx context.Context, raw, client, nonce string) (planIdentity, error) {
	var identity planIdentity
	metadata, err := s.planDiscovery(ctx)
	if err != nil {
		return identity, err
	}
	ctx = oidc.ClientContext(ctx, s.cloud)
	verifier := oidc.NewVerifier(planIssuer, oidc.NewRemoteKeySet(ctx, metadata["jwks"]), &oidc.Config{ClientID: client, SupportedSigningAlgs: []string{"RS256", "ES256"}})
	token, err := verifier.Verify(ctx, raw)
	if err != nil || token.Subject == "" || subtle.ConstantTimeCompare([]byte(token.Nonce), []byte(nonce)) != 1 {
		return identity, errors.New("ChatGPT identity verification failed; start a new sign-in")
	}
	if err := token.Claims(&identity); err != nil {
		return identity, errors.New("invalid ChatGPT account identity")
	}
	// Keep identity display fields bounded; never reflect arbitrary token text.
	if len(identity.Email) > 254 || strings.ContainsAny(identity.Email, "\r\n\x00") {
		identity.Email = ""
	}
	return identity, nil
}

func validPlanToken(token planCredential) bool {
	if !strings.EqualFold(token.TokenType, "Bearer") || token.AccessToken == "" || token.RefreshToken == "" || token.ExpiresIn < 1 || token.ExpiresIn > 86400 {
		return false
	}
	for _, secret := range []string{token.AccessToken, token.RefreshToken, token.IDToken} {
		if len(secret) > 64<<10 || strings.ContainsAny(secret, "\r\n\x00") {
			return false
		}
	}
	for _, scope := range []string{"resource.invoke", "chatgpt.tokens.use.direct", "offline_access"} {
		if !slices.Contains(strings.Fields(token.Scope), scope) {
			return false
		}
	}
	return true
}

func (s *Service) planToken(ctx context.Context, form url.Values) (planCredential, error) {
	var token planCredential
	req, _ := http.NewRequestWithContext(ctx, "POST", planIssuer+"/api/accounts/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := s.cloud.Do(req)
	if err != nil {
		return token, errors.New("ChatGPT token service is unreachable; try signing in again")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return token, fmt.Errorf("ChatGPT authorization returned HTTP %d; sign in again (no API-key fallback)", res.StatusCode)
	}
	if json.NewDecoder(io.LimitReader(res.Body, 256<<10)).Decode(&token) != nil || !validPlanToken(token) {
		return planCredential{}, errors.New("ChatGPT did not grant renewable plan usage; review permissions and sign in again")
	}
	token.ExpiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	return token, nil
}

// Caller holds planMu, serializing refresh and sign-out for this session.
func (s *Service) savePlanCredential(ctx context.Context, connectionID string, token planCredential, subject, email, actor string, expectedRevision ...int64) error {
	data, _ := json.Marshal(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.Get(ctx, connectionID)
	if err != nil {
		return err
	}
	if len(expectedRevision) > 0 && c.revision != expectedRevision[0] {
		return fmt.Errorf("%w: ChatGPT connection changed during sign-in; start again", ErrConflict)
	}
	if subject == "" && c.Disabled {
		return fmt.Errorf("%w: ChatGPT connection was disabled during refresh", ErrConflict)
	}
	ref, err := s.writeSecret(string(data))
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			os.Remove(filepath.Join(s.secretDir, ref))
		}
	}()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE provider_connections SET secret_ref=?,disabled=0,revision=revision+1,health='untested',last_error='',last_checked='' WHERE id=? AND kind='chatgpt'`, ref, connectionID); err != nil {
		return err
	}
	if subject != "" {
		if _, err = tx.ExecContext(ctx, `UPDATE chatgpt_accounts SET subject=?,email=? WHERE connection_id=?`, subject, email, connectionID); err != nil {
			return err
		}
	}
	if err = state.AuditTx(tx, actor, "authorize ChatGPT plan", connectionID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	committed = true
	if c.secretRef != "" {
		os.Remove(filepath.Join(s.secretDir, c.secretRef))
	}
	return nil
}

func (s *Service) chatGPTAccess(ctx context.Context, c Connection) (Connection, string, error) {
	s.planMu.Lock()
	defer s.planMu.Unlock()
	if s.planClosed {
		return c, "", ErrConflict
	}
	account, err := s.planAccount(ctx, c.ID)
	if err != nil {
		return c, "", err
	}
	if err = s.planOwner(ctx, account.Owner); err != nil {
		return c, "", err
	}
	// Re-read after acquiring the session lock: sign-out may have won the race.
	c, err = s.Get(ctx, c.ID)
	if err != nil {
		return c, "", err
	}
	if c.Disabled || c.secretRef == "" {
		return c, "", errors.New("ChatGPT is signed out; Continue with ChatGPT to reconnect")
	}
	raw, err := s.key(c.secretRef)
	if err != nil {
		return c, "", err
	}
	var token planCredential
	if json.Unmarshal([]byte(raw), &token) != nil || !validPlanToken(token) {
		return c, "", errors.New("ChatGPT credentials unavailable; sign in again")
	}
	if time.Until(token.ExpiresAt) <= 10*time.Second {
		if token.EarliestRefresh > time.Now().Unix() {
			return c, "", errors.New("ChatGPT session is not yet renewable; try again later")
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		next, err := s.planToken(refreshCtx, url.Values{"grant_type": {"refresh_token"}, "client_id": {account.clientID}, "refresh_token": {token.RefreshToken}, "resource": {planResource}})
		if err != nil {
			return c, "", err
		}
		if next.IDToken == "" {
			next.IDToken = token.IDToken
		}
		if err = s.savePlanCredential(ctx, c.ID, next, "", "", account.Owner); err != nil {
			return c, "", err
		}
		token = next
		c, err = s.Get(ctx, c.ID)
		if err != nil {
			return c, "", err
		}
	}
	return c, token.AccessToken, nil
}

// Sign-out always clears local credentials, even if remote revocation fails.
// Keep the account/client mapping for the next sign-in, as SIWC requires.
func (s *Service) SignOutPlan(ctx context.Context, owner, connectionID string) (map[string]any, error) {
	if err := s.planOwner(ctx, owner); err != nil {
		return nil, err
	}
	s.planMu.Lock()
	defer s.planMu.Unlock()
	a, err := s.planAccount(ctx, connectionID)
	if err != nil {
		return nil, err
	}
	if a.Owner != owner {
		return nil, ErrNotFound
	}
	for _, attempt := range s.planAttempts {
		if attempt.account.ConnectionID == connectionID {
			attempt.stop()
			attempt.view.Status = "cancelled"
			attempt.finished = true
			attempt.handoff = ""
			attempt.view.Message = "Sign-in cancelled by sign-out."
		}
	}
	for _, cancel := range s.planCalls[connectionID] {
		cancel()
	}
	s.mu.Lock()
	locked := true
	defer func() {
		if locked {
			s.mu.Unlock()
		}
	}()
	c, err := s.Get(ctx, connectionID)
	if err != nil {
		return nil, err
	}
	var token planCredential
	raw, keyErr := s.key(c.secretRef)
	if keyErr == nil {
		_ = json.Unmarshal([]byte(raw), &token)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE provider_connections SET secret_ref='',disabled=1,revision=revision+1,health='untested',last_error='',last_checked='' WHERE id=?`, connectionID); err != nil {
		return nil, err
	}
	if err = state.AuditTx(tx, owner, "sign out ChatGPT plan", connectionID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if c.secretRef != "" {
		os.Remove(filepath.Join(s.secretDir, c.secretRef))
	}
	s.mu.Unlock()
	locked = false
	revoked := token.RefreshToken == ""
	if token.RefreshToken != "" {
		revokeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		for attempt := 0; attempt < 2 && !revoked && revokeCtx.Err() == nil; attempt++ {
			if attempt > 0 {
				select {
				case <-time.After(150 * time.Millisecond):
				case <-revokeCtx.Done():
				}
			}
			if metadata, err := s.planDiscovery(revokeCtx); err == nil {
				form := url.Values{"token": {token.RefreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {a.clientID}}
				req, _ := http.NewRequestWithContext(revokeCtx, "POST", metadata["revoke"], strings.NewReader(form.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				if res, err := s.cloud.Do(req); err == nil {
					revoked = res.StatusCode == 200
					res.Body.Close()
					if res.StatusCode < 500 {
						break
					}
				}
			}
		}
	}
	message := "Signed out of ChatGPT."
	if !revoked {
		message += " Remote revocation was not confirmed. Disconnect Sluicework in ChatGPT Settings → Apps to end access there."
	}
	return map[string]any{"revoked": revoked, "message": message}, nil
}

func (s *Service) closePlanSessions() {
	s.planMu.Lock()
	defer s.planMu.Unlock()
	s.planClosed = true
	for _, a := range s.planAttempts {
		a.stop()
		if !a.finished {
			a.view.Status = "cancelled"
			a.view.Message = "Server stopped; start sign-in again."
			a.finished = true
			a.handoff = ""
		}
	}
	for _, calls := range s.planCalls {
		for _, cancel := range calls {
			cancel()
		}
	}
}
