package githubqueue

import (
	"context"
	"errors"
	"time"

	"sluicework/internal/githubapp"
)

func NewEngine(cfg Config, key []byte, store interface {
	Store
	RecordReader
}) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	peer := &Peer{Config: cfg, Clients: map[string]RepositoryAPI{}, Store: store}
	for _, repo := range cfg.Repositories {
		client, err := githubapp.NewWithPEM(githubapp.Config{AppID: cfg.AppID, InstallationID: cfg.InstallationID, Repository: repo.Name, OrganizationRunners: true}, key)
		if err != nil {
			return nil, err
		}
		peer.Clients[repo.Name] = client
	}
	return &Engine{Store: store, GitHub: peer, ID: cfg.BackendID}, nil
}

// NewLocalEngine preserves the private-file checks used by server installations.
func NewLocalEngine(cfg Config, store interface {
	Store
	RecordReader
}) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	peer := &Peer{Config: cfg, Clients: map[string]RepositoryAPI{}, Store: store}
	for _, repo := range cfg.Repositories {
		client, err := githubapp.New(githubapp.Config{AppID: cfg.AppID, InstallationID: cfg.InstallationID, Repository: repo.Name, OrganizationRunners: true, PrivateKeyFile: cfg.PrivateKeyFile})
		if err != nil {
			return nil, err
		}
		peer.Clients[repo.Name] = client
	}
	return &Engine{Store: store, GitHub: peer, ID: cfg.BackendID}, nil
}

// Run hosts the identical dispatcher in an always-on Sluicework process.
func (e *Engine) Run(ctx context.Context, interval time.Duration, report func(error)) error {
	if interval < 15*time.Second || interval > time.Minute {
		return errors.New("dispatcher poll interval must be 15–60 seconds")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := e.Tick(ctx); err != nil && ctx.Err() == nil && report != nil {
			report(err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
