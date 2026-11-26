package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"sluicework/internal/awsspot"
	"sluicework/internal/githubapp"
	"sluicework/internal/githubqueue"
	"sluicework/internal/spot"
)

func runGitHubQueue(ctx context.Context, path string, out io.Writer) error {
	var cfg githubqueue.Config
	if err := readConfig(path, &cfg); err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if !filepath.IsAbs(cfg.StateFile) || !filepath.IsAbs(cfg.PrivateKeyFile) {
		return errors.New("queue stateFile and privateKeyFile must be absolute paths")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.StateFile), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(cfg.StateFile, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	f.Close()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return errors.New("queue state must be a private regular file")
	}
	db, err := sql.Open("sqlite", cfg.StateFile)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	store, err := githubqueue.NewSQLiteStore(db)
	if err != nil {
		return err
	}
	engine, err := githubqueue.NewLocalEngine(cfg, store)
	if err != nil {
		return err
	}
	if cfg.Spot != nil {
		// The private key has already passed githubapp.New's ownership/file checks.
		key, e := os.ReadFile(cfg.PrivateKeyFile)
		if e != nil {
			return e
		}
		cloud, e := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Spot.Region))
		if e != nil {
			return errors.New("cannot load the Spot AWS execution identity")
		}
		fleetStore, e := spot.OpenSQLite(cfg.StateFile, cfg.BackendID)
		if e != nil {
			return e
		}
		defer fleetStore.Close()
		repos := []string{}
		sha := ""
		for _, r := range cfg.Repositories {
			repos = append(repos, r.Name)
			if r.Name == cfg.Spot.ControlRepository {
				sha = r.HeartbeatSHA
			}
		}
		fleet, e := awsspot.NewFleet(ctx, *cfg.Spot, cfg.BackendID, repos, githubapp.Config{AppID: cfg.AppID, InstallationID: cfg.InstallationID}, key, cloud, fleetStore)
		if e != nil {
			return e
		}
		fleet.HeartbeatSHA = sha
		engine.Fleet = fleet
	}
	fmt.Fprintln(out, "GitHub priority dispatcher running:", cfg.BackendID)
	return engine.Run(ctx, 30*time.Second, func(err error) { fmt.Fprintln(out, "Queue tick:", err) })
}
