package githubapp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"sluicework/internal/state"
)

type runners interface {
	Create(context.Context, string, []string) (Grant, error)
	Get(context.Context, int64) (Runner, error)
	Labels(context.Context, int64, []string) error
	Delete(context.Context, int64) error
}

type Broker struct {
	primaryRepository string
	children          map[string]*Broker
	adminMu           sync.Mutex
	management        managementSource
	organization      bool
	retired           bool
	db                *sql.DB
	client            runners
	repository        string
	mu                sync.Mutex
	activityClient    activitySource
	activityMu        sync.Mutex
	activityNext      time.Time
}

func Open(db *sql.DB, cfg Config) (*Broker, error) {
	c, err := New(cfg)
	if err != nil {
		return nil, err
	}
	b := &Broker{db: db, client: c, repository: cfg.Repository, activityClient: c, management: c, organization: cfg.OrganizationRunners}
	_, err = db.Exec(leaseSchema + ";" + activitySchema)
	if err != nil {
		return nil, err
	}
	b.primaryRepository = cfg.Repository
	b.children = map[string]*Broker{}
	for _, repository := range cfg.WorkstationRepositories {
		if strings.EqualFold(repository, cfg.Repository) {
			continue
		}
		childConfig := cfg
		childConfig.Repository = repository
		child, e := New(childConfig)
		if e != nil {
			return nil, e
		}
		b.children[strings.ToLower(repository)] = &Broker{db: db, repository: repository, primaryRepository: cfg.Repository, organization: cfg.OrganizationRunners, client: child, activityClient: child, management: child}
	}
	return b, nil
}

const leaseSchema = `CREATE TABLE IF NOT EXISTS github_runner_leases (machine TEXT PRIMARY KEY, runner INTEGER NOT NULL, repository TEXT NOT NULL, issued INTEGER NOT NULL, withdrawn INTEGER NOT NULL DEFAULT 0)`

// Reconcile withdraws stale/revoked machines even when their worker can no longer
// authenticate. A running one-job runner is allowed to finish.
func (b *Broker) Reconcile(ctx context.Context, store *state.Store) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.retired {
		return nil
	}
	rows, err := b.db.Query("SELECT machine,runner,withdrawn FROM github_runner_leases WHERE repository=?", b.repository)
	if err != nil {
		return err
	}
	type lease struct {
		machine       string
		id, withdrawn int64
	}
	var leases []lease
	for rows.Next() {
		var l lease
		if err = rows.Scan(&l.machine, &l.id, &l.withdrawn); err != nil {
			rows.Close()
			return err
		}
		leases = append(leases, l)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, l := range leases {
		m, e := store.Machine(l.machine)
		if e == nil && m.Online && !m.Draining && !m.Revoked && m.Runner != nil && m.Runner.Workstation && strings.EqualFold(m.Runner.Scope, b.repository) && len(m.Roles) == 1 && m.Roles[0] == "ci" {
			continue
		}
		if l.withdrawn == 0 {
			err = b.client.Labels(ctx, l.id, nil)
		} else {
			err = nil
		}
		if errors.Is(err, HTTPError(404)) {
			_, err = b.db.Exec("DELETE FROM github_runner_leases WHERE machine=?", l.machine)
			if err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if l.withdrawn == 0 {
			_, err = b.db.Exec("UPDATE github_runner_leases SET withdrawn=? WHERE machine=?", time.Now().Unix(), l.machine)
			if err != nil {
				return err
			}
			continue
		}
		if time.Now().Unix()-l.withdrawn < 75 {
			continue
		}
		r, e := b.client.Get(ctx, l.id)
		if e != nil && !errors.Is(e, HTTPError(404)) {
			return e
		}
		if r.Busy {
			continue
		}
		if e == nil {
			if e = b.client.Delete(ctx, l.id); e != nil && !errors.Is(e, HTTPError(404)) {
				return e
			}
		}
		if _, err = b.db.Exec("DELETE FROM github_runner_leases WHERE machine=?", l.machine); err != nil {
			return err
		}
	}
	return nil
}

// Act uses only administrator-approved scope/labels. A worker cannot choose an
// arbitrary repository, runner ID, or grant itself another machine's labels.
func (b *Broker) Act(ctx context.Context, m state.Machine, operation string) (Grant, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.retired {
		return Grant{}, state.ErrConflict
	}
	if m.Runner == nil || !m.Runner.Workstation || !strings.EqualFold(m.Runner.Scope, b.repository) || m.OS == "" || m.Arch == "" || len(m.Roles) != 1 || m.Roles[0] != "ci" {
		return Grant{}, fmt.Errorf("%w: an administrator must approve a workstation runner for this repository", state.ErrInvalid)
	}
	var id, issued int64
	var repo string
	err := b.db.QueryRow("SELECT runner,repository,issued FROM github_runner_leases WHERE machine=?", m.ID).Scan(&id, &repo, &issued)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Grant{}, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	if repo != "" && !strings.EqualFold(repo, b.repository) {
		return Grant{}, state.ErrConflict
	}
	var current Runner
	if id > 0 && operation != "enable" && operation != "pause" {
		current, err = b.client.Get(ctx, id)
		if errors.Is(err, HTTPError(404)) {
			_, err = b.db.Exec("DELETE FROM github_runner_leases WHERE machine=?", m.ID)
			id = 0
		}
		if err != nil {
			return Grant{}, err
		}
	}
	labels := []string{}
	seen := map[string]bool{}
	for _, label := range append(append([]string{}, m.Runner.Labels...), "sluicework-"+m.ID[:12]) {
		if !seen[strings.ToLower(label)] {
			labels = append(labels, label)
			seen[strings.ToLower(label)] = true
		}
	}
	grant := Grant{Runner: current}
	switch operation {
	case "acquire":
		if m.Draining || m.Revoked {
			return Grant{}, state.ErrConflict
		}
		if id > 0 {
			return grant, nil
		}
		grant, err = b.client.Create(ctx, "sluicework-"+m.ID, labels)
		if err != nil {
			return Grant{}, err
		}
		_, err = b.db.Exec("INSERT INTO github_runner_leases(machine,runner,repository,issued) VALUES(?,?,?,?)", m.ID, grant.Runner.ID, b.repository, time.Now().Unix())
		if err == nil {
			store := state.Store{DB: b.db}
			err = store.Audit("machine:"+m.ID, "register GitHub workstation runner", b.repository)
		}
		if err != nil {
			_ = b.client.Delete(ctx, grant.Runner.ID)
			return Grant{}, err
		}
		return grant, nil
	case "enable":
		if m.Draining || m.Revoked {
			return Grant{}, state.ErrConflict
		}
		if id > 0 {
			err = b.client.Labels(ctx, id, labels)
			if err == nil {
				_, err = b.db.Exec("UPDATE github_runner_leases SET withdrawn=0 WHERE machine=?", m.ID)
			}
		}
	case "pause":
		// Withdraw matching custom labels before inspecting busy state. Do not
		// interrupt the listener: an already assigned build must finish.
		if id > 0 {
			err = b.client.Labels(ctx, id, nil)
		}
	case "status":
	case "release":
		if id > 0 {
			if current.Busy {
				return Grant{}, state.ErrConflict
			}
			err = b.client.Delete(ctx, id)
			if err == nil {
				_, err = b.db.Exec("DELETE FROM github_runner_leases WHERE machine=?", m.ID)
			}
		}
	default:
		return Grant{}, fmt.Errorf("%w: invalid runner operation", state.ErrInvalid)
	}
	return grant, err
}

// Replace serializes connection changes with runner registration. Waiting calls
// on this broker fail closed after replacement and retry against the new one.
func (b *Broker) Replace(requireIdle bool, change func() error) error {
	b.adminMu.Lock()
	defer b.adminMu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	if requireIdle {
		var count int
		if err := b.db.QueryRow("SELECT count(*) FROM github_runner_leases WHERE repository=?", b.repository).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("%w: drain workstation runners and wait for registrations to clear before changing the GitHub App, installation or repository", state.ErrConflict)
		}
	}
	for _, child := range b.children {
		child.adminMu.Lock()
		defer child.adminMu.Unlock()
		child.mu.Lock()
		defer child.mu.Unlock()
	}
	if err := change(); err != nil {
		return err
	}
	b.retired = true
	for _, child := range b.children {
		child.retired = true
	}
	return nil
}
