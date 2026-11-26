package personal

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"sluicework/internal/provider"
	"sluicework/internal/schedule"
	"sluicework/internal/state"
)

type Approval struct {
	Proposal     Proposal  `json:"proposal"`
	Digest       string    `json:"digest"`
	ConnectionID string    `json:"connectionId"`
	JobID        string    `json:"jobId"`
	ApprovedAt   time.Time `json:"approvedAt"`
	Paused       bool      `json:"paused"`
}
type Preview struct {
	Proposal Proposal `json:"proposal"`
	Digest   string   `json:"digest"`
	Account  string   `json:"account"`
}
type Agent struct {
	vault         *Vault
	Config        Config
	key           ed25519.PrivateKey
	db            *sql.DB
	providers     *provider.Service
	jobs          *state.Store
	network       *Network
	mu            sync.Mutex
	inbox         map[string]Proposal
	busy          bool
	activeID      string
	lastSyncError string
	activeCancel  context.CancelFunc
	boot          time.Time
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	generate      func(context.Context, provider.Request) (provider.Result, error)
	notify        func(context.Context, string) error
}

func OpenAgent(v *Vault) (*Agent, error) {
	config, err := v.Config()
	if err != nil {
		return nil, err
	}
	key, err := v.Key()
	if err != nil {
		return nil, err
	}
	db, err := state.Open(filepath.Join(v.Dir, "state.db"))
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			db.Close()
		}
	}()
	jobs := &state.Store{DB: db}
	if err = jobs.Init(); err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS auth_users(username TEXT PRIMARY KEY,role TEXT,passhash BLOB);
 CREATE TABLE IF NOT EXISTS personal_settings(name TEXT PRIMARY KEY,value TEXT NOT NULL);
 INSERT OR IGNORE INTO personal_settings VALUES('paused','true');
 CREATE TABLE IF NOT EXISTS personal_approvals(id TEXT PRIMARY KEY,approval TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS personal_local_runs(id TEXT PRIMARY KEY,schedule_id TEXT NOT NULL,slot TEXT NOT NULL,day TEXT NOT NULL,started TEXT NOT NULL,status TEXT NOT NULL,message TEXT NOT NULL DEFAULT '',result TEXT NOT NULL DEFAULT '',share INTEGER NOT NULL,UNIQUE(schedule_id,slot));`)
	if err != nil {
		return nil, err
	}
	if _, err = db.Exec(`INSERT OR IGNORE INTO auth_users VALUES(?,'admin',X'00')`, config.Owner); err != nil {
		return nil, err
	}
	svc, err := provider.OpenPersonal(db, filepath.Join(v.Dir, "credentials"), config.Owner)
	if err != nil {
		return nil, err
	}
	network, err := NewNetwork(config.Server, config.CA)
	if err != nil {
		svc.Close()
		return nil, err
	}
	// Interrupted inference is never retried automatically; it may have consumed allowance.
	if _, err = db.Exec(`UPDATE personal_local_runs SET status='interrupted',message='Runtime restarted; outcome unknown. No automatic retry.' WHERE status='running'`); err != nil {
		svc.Close()
		return nil, err
	}
	a := &Agent{vault: v, Config: config, key: key, db: db, providers: svc, jobs: jobs, network: network, inbox: map[string]Proposal{}, boot: time.Now()}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS personal_run_details(run_id TEXT PRIMARY KEY, citations TEXT NOT NULL DEFAULT '[]', web_searched INTEGER NOT NULL DEFAULT 0, notification TEXT NOT NULL DEFAULT 'not_requested'); UPDATE personal_run_details SET notification='unknown' WHERE notification IN ('pending','attempting')`); err != nil {
		svc.Close()
		return nil, err
	}
	a.generate = svc.Generate
	a.notify = desktopNotify
	ok = true
	return a, nil
}
func (a *Agent) Close() {
	a.mu.Lock()
	if a.cancel != nil {
		a.cancel()
	}
	if a.activeCancel != nil {
		a.activeCancel()
	}
	a.mu.Unlock()
	a.wg.Wait()
	a.providers.Close()
	a.db.Close()
}
func (a *Agent) setting(name string) string {
	var v string
	_ = a.db.QueryRow(`SELECT value FROM personal_settings WHERE name=?`, name).Scan(&v)
	return v
}
func (a *Agent) set(name, value string) error {
	_, err := a.db.Exec(`INSERT INTO personal_settings VALUES(?,?) ON CONFLICT(name) DO UPDATE SET value=excluded.value`, name, value)
	return err
}
func (a *Agent) profile() (provider.Connection, error) {
	id := a.setting("connection")
	if id == "" {
		return provider.Connection{}, errors.New("sign in to ChatGPT in this personal runtime first")
	}
	return a.providers.Get(context.Background(), id)
}
func (a *Agent) Preview(id string) (Preview, error) {
	a.mu.Lock()
	p, ok := a.inbox[id]
	a.mu.Unlock()
	if !ok {
		return Preview{}, errors.New("proposal not synchronized; check the cluster connection and try again")
	}
	if p.Owner != a.Config.Owner || p.RuntimeID != a.Config.RuntimeID {
		return Preview{}, ErrDenied
	}
	if err := p.Manifest.Validate(time.Now()); err != nil {
		return Preview{}, err
	}
	c, err := a.profile()
	if err != nil {
		return Preview{}, err
	}
	if err = c.ValidatePolicy(p.Manifest.Model, true); err != nil {
		return Preview{}, err
	}
	accounts, err := a.providers.PlanAccounts(context.Background())
	if err != nil {
		return Preview{}, err
	}
	account := a.Config.Owner
	for _, v := range accounts {
		if v.ConnectionID == c.ID && v.Email != "" {
			account = v.Email
		}
	}
	return Preview{p, a.consentDigest(p, c.ID), account}, nil
}
func (a *Agent) Approve(id, digest string) (Approval, error) {
	// Recheck the preview immediately before committing consent, closing the UI/CLI TOCTOU gap.
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.inbox[id]
	if !ok || p.Owner != a.Config.Owner || p.RuntimeID != a.Config.RuntimeID {
		return Approval{}, errors.New("proposal changed; preview and approve it again")
	}
	if err := p.Manifest.Validate(time.Now()); err != nil {
		return Approval{}, err
	}
	c, err := a.profile()
	if err != nil {
		return Approval{}, err
	}
	if err = c.ValidatePolicy(p.Manifest.Model, true); err != nil {
		return Approval{}, err
	}
	if a.consentDigest(p, c.ID) != digest {
		return Approval{}, errors.New("proposal or personal account changed; preview and approve again")
	}
	j, err := a.jobs.SaveJob(a.Config.Owner, "", state.Job{Name: p.Manifest.Name, Type: "ai", Role: "ai", Prompt: p.Manifest.Prompt, Connection: c.ID, Model: p.Manifest.Model, AllowCloud: true, MaxTokens: 1024, TimeoutSeconds: p.Manifest.TimeoutSeconds, OutputLimit: p.Manifest.OutputLimit, OutputFormat: "text"})
	if err != nil {
		return Approval{}, err
	}
	approved := Approval{Proposal: p, Digest: p.Digest(a.Config.Server), ConnectionID: c.ID, JobID: j.ID, ApprovedAt: time.Now().UTC()}
	data, _ := json.Marshal(approved)
	_, err = a.db.Exec(`INSERT INTO personal_approvals VALUES(?,?) ON CONFLICT(id) DO UPDATE SET approval=excluded.approval`, id, string(data))
	return approved, err
}
func (a *Agent) Approvals() ([]Approval, error) {
	rows, err := a.db.Query(`SELECT approval FROM personal_approvals ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Approval{}
	for rows.Next() {
		var raw string
		var v Approval
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (a *Agent) Pause(paused bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if paused && a.activeCancel != nil {
		a.activeCancel()
	}
	if paused {
		return a.set("paused", "true")
	}
	return a.set("paused", "false")
}
func (a *Agent) Revoke(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeCancel != nil && a.activeID == id {
		a.activeCancel()
	}
	_, err := a.db.Exec(`DELETE FROM personal_approvals WHERE id=?`, id)
	return err
}
func (a *Agent) Results(limit int, sharing bool) ([]Run, error) {
	if limit < 1 || limit > 100 {
		limit = 10
	}
	rows, err := a.db.Query(`SELECT r.id,r.schedule_id,r.slot,r.started,r.status,r.message,r.result,r.share,COALESCE(d.citations,'[]'),COALESCE(d.web_searched,0),COALESCE(d.notification,'not_requested') FROM personal_local_runs r LEFT JOIN personal_run_details d ON d.run_id=r.id ORDER BY r.started DESC,r.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		var v Run
		var started string
		var share bool
		var citations string
		if err = rows.Scan(&v.ID, &v.ScheduleID, &v.Slot, &started, &v.State, &v.Message, &v.Result, &share, &citations, &v.WebSearched, &v.Notification); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(citations), &v.Citations); err != nil {
			return nil, err
		}
		v.Started, _ = time.Parse(time.RFC3339Nano, started)
		if sharing && !share {
			v.Result = ""
			v.Citations = nil
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (a *Agent) Report() (Report, error) {
	approvals, err := a.Approvals()
	if err != nil {
		return Report{}, err
	}
	r := Report{Paused: a.setting("paused") != "false", Models: []string{}, Approvals: []ApprovalStatus{}}
	for _, v := range approvals {
		r.Approvals = append(r.Approvals, ApprovalStatus{ID: v.Proposal.ID, Digest: v.Digest, Paused: v.Paused, Revision: v.Proposal.Revision, Cron: v.Proposal.Manifest.Cron, Timezone: v.Proposal.Manifest.Timezone, Model: v.Proposal.Manifest.Model, Expires: v.Proposal.Manifest.Expires, WebSearch: v.Proposal.Manifest.WebSearch, DesktopNotifications: v.Proposal.Manifest.DesktopNotifications})
	}
	if c, err := a.profile(); err == nil && !c.Disabled && c.Health != "error" {
		r.SignedIn = true
		r.Models = c.Models
	}
	r.Runs, err = a.Results(10, true)
	return r, err
}
func (a *Agent) Sync(ctx context.Context) error {
	report, err := a.Report()
	if err != nil {
		return err
	}
	proposals, err := a.network.Sync(ctx, a.key, report)
	if err != nil {
		return err
	}
	if len(proposals) > 100 {
		return ErrInvalid
	}
	inbox := map[string]Proposal{}
	for _, p := range proposals {
		if p.RuntimeID != a.Config.RuntimeID || p.Owner != a.Config.Owner {
			return ErrDenied
		}
		inbox[p.ID] = p
	}
	a.mu.Lock()
	a.inbox = inbox
	a.mu.Unlock()
	return nil
}

// Tick uses the local clock and locally approved immutable definitions. A central
// proposal, queue message, other user's activity or sync outage cannot start a run.
func (a *Agent) Tick(ctx context.Context, now time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy || a.setting("paused") != "false" {
		return nil
	}
	approvals, err := a.Approvals()
	if err != nil {
		return err
	}
	slot := now.UTC().Truncate(time.Minute)
	if slot.Before(a.boot) {
		return nil
	}
	for _, v := range approvals {
		p := v.Proposal
		m := p.Manifest
		if v.Paused || slot.Before(v.ApprovedAt) || !now.Before(m.Expires) || p.Owner != a.Config.Owner || p.RuntimeID != a.Config.RuntimeID || p.Digest(a.Config.Server) != v.Digest {
			continue
		}
		due, err := schedule.NextOccurrence(m.Cron, m.Timezone, slot.Add(-time.Minute))
		if err != nil || !due.Equal(slot) {
			continue
		}
		location, err := time.LoadLocation(m.Timezone)
		if err != nil {
			continue
		}
		local := slot.In(location)
		slotKey := local.Format("2006-01-02T15:04")
		day := local.Format("2006-01-02")
		var count int
		if err = a.db.QueryRow(`SELECT count(*) FROM personal_local_runs WHERE schedule_id=? AND day=?`, p.ID, day).Scan(&count); err != nil {
			return err
		}
		if count >= m.MaxRunsPerDay {
			continue
		}
		id, err := randomValue()
		if err != nil {
			return err
		}
		res, err := a.db.Exec(`INSERT OR IGNORE INTO personal_local_runs(id,schedule_id,slot,day,started,status,share) VALUES(?,?,?,?,?,'running',?)`, id, p.ID, slotKey, day, now.UTC().Format(time.RFC3339Nano), m.ShareResults)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			continue
		}
		deadline := time.Now().Add(time.Duration(m.TimeoutSeconds) * time.Second)
		if m.Expires.Before(deadline) {
			deadline = m.Expires
		}
		runCtx, cancel := context.WithDeadline(ctx, deadline)
		a.busy = true
		a.activeID = p.ID
		a.activeCancel = cancel
		a.wg.Add(1)
		go a.execute(runCtx, cancel, id, v)
		break
	}
	return nil
}
func (a *Agent) execute(ctx context.Context, cancel context.CancelFunc, id string, approved Approval) {
	defer a.wg.Done()
	defer cancel()
	defer func() { a.mu.Lock(); a.busy = false; a.activeID = ""; a.activeCancel = nil; a.mu.Unlock() }()
	p := approved.Proposal
	m := p.Manifest
	run, err := a.jobs.Queue(a.Config.Owner, approved.JobID, nil)
	var result provider.Result
	if err == nil {
		result, err = a.generate(ctx, provider.Request{AttemptID: run.Attempts[0].ID, ConnectionID: approved.ConnectionID, Model: m.Model, Prompt: m.Prompt, AllowCloud: true, MaxTokens: 1024, Format: "text", WebSearch: m.WebSearch})
	}
	status, message, text := "succeeded", "", result.Text
	if err != nil {
		status = "failed"
		message = err.Error()
		if errors.Is(err, context.Canceled) {
			status = "cancelled"
		}
		text = ""
		if strings.Contains(message, "usage limit") || strings.Contains(message, "usage unavailable") || strings.Contains(message, "HTTP 429") || strings.Contains(message, "HTTP 401") || strings.Contains(message, "HTTP 403") || strings.Contains(message, "credential") || strings.Contains(message, "sign in") || strings.Contains(message, "connection disabled") || strings.Contains(message, "signed out") {
			_ = a.Pause(true)
		}
	}
	if len(text) > m.OutputLimit {
		cut := m.OutputLimit
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
		message = "Result truncated to the approved byte limit."
	}
	// Keep only citations whose character ranges survive the approved byte cap.
	citations := []provider.Citation{}
	for _, c := range result.Citations {
		if c.Start >= 0 && c.End > c.Start && c.End <= len([]rune(text)) && provider.SafeCitationURL(c.URL) {
			citations = append(citations, c)
		}
	}
	if err := a.finishRun(id, status, message, text, citations, result.WebSearched, m.DesktopNotifications); err == nil && m.DesktopNotifications {
		a.notifyRun(id, status)
	}
}
func (a *Agent) RefreshModels(ctx context.Context) ([]provider.PlanModel, error) {
	c, err := a.profile()
	if err != nil {
		return nil, err
	}
	models, err := a.providers.PlanModels(ctx, a.Config.Owner, c.ID)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	if len(ids) > 128 {
		ids = ids[:128]
	}
	if !slices.Equal(ids, c.Models) {
		_, err = a.providers.Update(provider.WithActor(ctx, a.Config.Owner), c.ID, provider.Config{Name: c.Name, Kind: "chatgpt", Models: ids})
	}
	return models, err
}
func (a *Agent) Login(ctx context.Context, consent bool) (provider.PlanSignIn, error) {
	id := a.setting("connection")
	v, err := a.providers.StartPlanSignIn(ctx, a.Config.Owner, id, "Personal ChatGPT", consent, false)
	if err == nil && id == "" {
		err = a.set("connection", v.ConnectionID)
	}
	return v, err
}
func (a *Agent) LoginStatus(ctx context.Context, id string) (provider.PlanSignIn, error) {
	v, err := a.providers.PlanSignInStatus(a.Config.Owner, id)
	if err == nil && v.Status == "succeeded" {
		_, err = a.RefreshModels(ctx)
	}
	return v, err
}
func (a *Agent) SignOut(ctx context.Context) (map[string]any, error) {
	if err := a.Pause(true); err != nil {
		return nil, err
	}
	c, err := a.profile()
	if err != nil {
		return nil, err
	}
	return a.providers.SignOutPlan(ctx, a.Config.Owner, c.ID)
}

// Bind consent to both the proposal and the exact private account connection.
func (a *Agent) consentDigest(p Proposal, connectionID string) string {
	sum := sha256.Sum256([]byte(p.Digest(a.Config.Server) + "\n" + connectionID))
	return hex.EncodeToString(sum[:])
}
