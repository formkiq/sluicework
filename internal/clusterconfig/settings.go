// Package clusterconfig manages operator settings in the cluster database.
// Installation files remain bootstrap inputs; credentials are never returned.
package clusterconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"sluicework/internal/githubapp"
	"sluicework/internal/state"
)

type Network struct {
	Enabled   bool   `json:"enabled"`
	Hostname  string `json:"hostname"`
	Port      int    `json:"port"`
	CertFile  string `json:"certFile,omitempty"`
	KeyFile   string `json:"keyFile,omitempty"`
	CAFile    string `json:"caFile,omitempty"`
	CAKeyFile string `json:"caKeyFile,omitempty"`
}
type Settings struct {
	Network   Network           `json:"network"`
	GitHub    *githubapp.Config `json:"github,omitempty"`
	OpenWebUI *OpenWebUI        `json:"openWebUI,omitempty"`
	LogLimit  int               `json:"logLimit"`
}
type GitHubView struct {
	WorkstationGroupID      int64    `json:"workstationGroupId"`
	WorkstationRepositories []string `json:"workstationRepositories"`
	OrganizationRunners     bool     `json:"organizationRunners"`
	AppID                   int64    `json:"appId"`
	InstallationID          int64    `json:"installationId"`
	Repository              string   `json:"repository"`
	KeyConfigured           bool     `json:"keyConfigured"`
}
type NetworkView struct {
	Enabled            bool   `json:"enabled"`
	Hostname           string `json:"hostname"`
	Port               int    `json:"port"`
	URL                string `json:"url,omitempty"`
	CAPEM              string `json:"caPem,omitempty"`
	Fingerprint        string `json:"fingerprint,omitempty"`
	Issue              string `json:"issue,omitempty"`
	CertificateExpires string `json:"certificateExpires,omitempty"`
}
type View struct {
	Hostname      string         `json:"hostname"`
	OS            string         `json:"os"`
	Arch          string         `json:"arch"`
	DataDir       string         `json:"dataDir"`
	PrimaryListen string         `json:"primaryListen"`
	Network       NetworkView    `json:"network"`
	GitHub        *GitHubView    `json:"github,omitempty"`
	OpenWebUI     *OpenWebUIView `json:"openWebUI,omitempty"`
	LogLimit      int            `json:"logLimit"`
	GitHubIssue   string         `json:"githubIssue,omitempty"`
}
type GitHubInput struct {
	WorkstationGroupID      *int64    `json:"workstationGroupId,omitempty"`
	WorkstationRepositories *[]string `json:"workstationRepositories,omitempty"`
	OrganizationRunners     *bool     `json:"organizationRunners,omitempty"`
	Enabled                 bool      `json:"enabled"`
	AppID                   int64     `json:"appId"`
	InstallationID          int64     `json:"installationId"`
	Repository              string    `json:"repository"`
	PrivateKeyPEM           string    `json:"privateKeyPem,omitempty"`
}
type NetworkInput struct {
	Enabled  bool   `json:"enabled"`
	Hostname string `json:"hostname"`
	Port     int    `json:"port"`
	RotateCA bool   `json:"rotateCa,omitempty"`
}
type Manager struct {
	db                        *sql.DB
	dir, primary              string
	write                     sync.Mutex
	mu                        sync.RWMutex
	settings                  Settings
	networkIssue, githubIssue string
	handler                   http.Handler
	networkServer             *http.Server
	certificate               atomic.Pointer[certificate]
	logLimit                  atomic.Int64
	closed                    atomic.Bool
	closeOnce                 sync.Once
	renewStarted              bool
	renewStop                 chan struct{}
}

type Seed struct {
	DataDir, Listen string
	GitHub          *githubapp.Config
	LogLimit        int
}

func Open(db *sql.DB, seed Seed) (*Manager, error) {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS cluster_configuration (id INTEGER PRIMARY KEY CHECK(id=1), settings TEXT NOT NULL)`); err != nil {
		return nil, err
	}
	m := &Manager{db: db, dir: seed.DataDir, primary: seed.Listen, renewStop: make(chan struct{}), settings: Settings{GitHub: seed.GitHub, LogLimit: seed.LogLimit, Network: Network{Port: 8443}}}
	var raw string
	err := db.QueryRow(`SELECT settings FROM cluster_configuration WHERE id=1`).Scan(&raw)
	if err == nil {
		if err = json.Unmarshal([]byte(raw), &m.settings); err != nil {
			return nil, errors.New("invalid saved cluster settings")
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if m.settings.LogLimit == 0 {
		m.settings.LogLimit = 1 << 20
	}
	if m.settings.LogLimit < 1 || m.settings.LogLimit > 64<<20 {
		return nil, errors.New("invalid saved log limit")
	}
	m.logLimit.Store(int64(m.settings.LogLimit))
	return m, nil
}
func (m *Manager) LogLimit() int     { return int(m.logLimit.Load()) }
func (m *Manager) current() Settings { m.mu.RLock(); defer m.mu.RUnlock(); return m.settings }
func (m *Manager) Snapshot() View {
	cfg := m.current()
	host, _ := os.Hostname()
	v := View{Hostname: host, OS: runtime.GOOS, Arch: runtime.GOARCH, DataDir: m.dir, PrimaryListen: m.primary, LogLimit: cfg.LogLimit, Network: NetworkView{Enabled: cfg.Network.Enabled, Hostname: cfg.Network.Hostname, Port: cfg.Network.Port}}
	m.mu.RLock()
	v.Network.Issue = m.networkIssue
	v.GitHubIssue = m.githubIssue
	m.mu.RUnlock()
	if cfg.GitHub != nil {
		v.GitHub = &GitHubView{WorkstationGroupID: cfg.GitHub.WorkstationGroupID, WorkstationRepositories: append([]string{}, cfg.GitHub.WorkstationRepositories...), OrganizationRunners: cfg.GitHub.OrganizationRunners, AppID: cfg.GitHub.AppID, InstallationID: cfg.GitHub.InstallationID, Repository: cfg.GitHub.Repository, KeyConfigured: cfg.GitHub.PrivateKeyFile != ""}
	}
	if cfg.OpenWebUI != nil {
		v.OpenWebUI = &OpenWebUIView{Enabled: cfg.OpenWebUI.Enabled, URL: cfg.OpenWebUI.URL, OllamaBaseURL: cfg.OpenWebUI.OllamaBaseURL}
	}
	if cfg.Network.Enabled {
		v.Network.URL = networkURL(cfg.Network)
	}
	if cfg.Network.CertFile != "" {
		if cert, err := loadCertificate(cfg.Network); err == nil {
			v.Network.CertificateExpires = cert.Leaf.NotAfter.Format(time.RFC3339)
		}
	}
	if cfg.Network.CAFile != "" {
		if ca, err := os.ReadFile(cfg.Network.CAFile); err == nil {
			v.Network.CAPEM = string(ca)
			v.Network.Fingerprint = fingerprint(ca)
		}
	}
	return v
}
func (m *Manager) store(actor string, cfg Settings) error {
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO cluster_configuration(id,settings) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET settings=excluded.settings`, string(b)); err != nil {
		return err
	}
	if err = state.AuditTx(tx, actor, "update cluster settings", "cluster"); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	m.mu.Lock()
	m.settings = cfg
	m.mu.Unlock()
	m.logLimit.Store(int64(cfg.LogLimit))
	return nil
}
func (m *Manager) SetLogLimit(actor string, limit int) error {
	if limit < 1 || limit > 64<<20 {
		return fmt.Errorf("%w: log limit must be 1–67108864 bytes", state.ErrInvalid)
	}
	m.write.Lock()
	defer m.write.Unlock()
	cfg := m.current()
	cfg.LogLimit = limit
	return m.store(actor, cfg)
}
func (m *Manager) OpenGitHub() (*githubapp.Broker, error) {
	cfg := m.current()
	if cfg.GitHub == nil {
		return nil, nil
	}
	broker, err := githubapp.Open(m.db, *cfg.GitHub)
	m.mu.Lock()
	if err != nil {
		m.githubIssue = "GitHub connection cannot load its private key. Reconfigure it in Settings."
	} else {
		m.githubIssue = ""
	}
	m.mu.Unlock()
	return broker, err
}

// transition serializes replacement with the previous broker's runner operations.
func (m *Manager) SetGitHub(actor string, in GitHubInput, transition func(bool, *githubapp.Broker, func() error) error) error {
	m.write.Lock()
	defer m.write.Unlock()
	cfg := m.current()
	old := cfg.GitHub
	var fresh string
	if in.Enabled {
		key := ""
		if old != nil {
			key = old.PrivateKeyFile
		}
		if in.PrivateKeyPEM != "" {
			if len(in.PrivateKeyPEM) > 16384 {
				return fmt.Errorf("%w: private key is too large", state.ErrInvalid)
			}
			var err error
			fresh, err = m.privateFile("github-key-", []byte(in.PrivateKeyPEM))
			if err != nil {
				return err
			}
			key = fresh
		}
		cfg.GitHub = &githubapp.Config{AppID: in.AppID, InstallationID: in.InstallationID, Repository: in.Repository, PrivateKeyFile: key}
		if old != nil {
			cfg.GitHub.OrganizationRunners = old.OrganizationRunners
			cfg.GitHub.WorkstationGroupID = old.WorkstationGroupID
			cfg.GitHub.WorkstationRepositories = append([]string{}, old.WorkstationRepositories...)
		}
		if in.OrganizationRunners != nil {
			cfg.GitHub.OrganizationRunners = *in.OrganizationRunners
		}
		if in.WorkstationGroupID != nil {
			cfg.GitHub.WorkstationGroupID = *in.WorkstationGroupID
		}
		if in.WorkstationRepositories != nil {
			cfg.GitHub.WorkstationRepositories = append([]string{}, (*in.WorkstationRepositories)...)
		}
		if _, err := githubapp.New(*cfg.GitHub); err != nil {
			if fresh != "" {
				os.Remove(fresh)
			}
			return fmt.Errorf("%w: provide valid App IDs, owner/repository and a PEM RSA private key", state.ErrInvalid)
		}
	} else {
		cfg.GitHub = nil
	}
	var broker *githubapp.Broker
	var err error
	if cfg.GitHub != nil {
		broker, err = githubapp.Open(m.db, *cfg.GitHub)
		if err != nil {
			if fresh != "" {
				os.Remove(fresh)
			}
			return err
		}
	}
	requireIdle := old != nil && (cfg.GitHub == nil || old.Repository != cfg.GitHub.Repository || old.AppID != cfg.GitHub.AppID || old.InstallationID != cfg.GitHub.InstallationID || old.WorkstationGroupID != cfg.GitHub.WorkstationGroupID || !slices.Equal(old.WorkstationRepositories, cfg.GitHub.WorkstationRepositories))
	err = transition(requireIdle, broker, func() error { return m.store(actor, cfg) })
	if err != nil && fresh != "" {
		os.Remove(fresh)
	}
	if err == nil {
		m.mu.Lock()
		m.githubIssue = ""
		m.mu.Unlock()
	}
	return err
}
func (m *Manager) CheckGitHub(ctx context.Context) error {
	cfg := m.current()
	if cfg.GitHub == nil {
		return fmt.Errorf("%w: configure a GitHub App first", state.ErrInvalid)
	}
	client, err := githubapp.New(*cfg.GitHub)
	if err != nil {
		return errors.New("GitHub App private key is unavailable")
	}
	return client.Check(ctx)
}
func (m *Manager) privateFile(prefix string, b []byte) (string, error) {
	dir := filepath.Join(m.dir, "configuration-secrets")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, prefix)
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}
