// Package backup takes and restores consistent SQLite snapshots while preserving
// retained logs, job revisions, schedules, and the matching server configuration.
package backup

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"

	"sluicework/internal/state"
)

type manifest struct {
	Version int    `json:"version"`
	State   string `json:"stateSHA256"`
	Config  string `json:"configSHA256"`
}

func checksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	_, err = io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), err
}
func integrity(db *sql.DB) error {
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return errors.New("SQLite integrity check failed")
	}
	var version int
	if err := db.QueryRow("SELECT max(version) FROM state_migrations").Scan(&version); err != nil {
		return err
	}
	if version < 1 || version > state.SchemaVersion {
		return fmt.Errorf("unsupported database version %d", version)
	}
	return nil
}

// Create fails if dest already exists. VACUUM INTO includes committed WAL frames.
func Create(db *sql.DB, configPath, dest string) (err error) {
	if dest == "" {
		return errors.New("provide --out for the new backup directory")
	}
	dest, err = filepath.Abs(dest)
	if err != nil {
		return err
	}
	if err = os.Mkdir(dest, 0700); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(dest)
		}
	}()
	database := filepath.Join(dest, "state.db")
	cfg := filepath.Join(dest, "server.json")
	if _, err = db.Exec("VACUUM INTO ?", database); err != nil {
		return err
	}
	if err = os.Chmod(database, 0600); err != nil {
		return err
	}
	config, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	if err = os.WriteFile(cfg, config, 0600); err != nil {
		return err
	}
	u := url.URL{Scheme: "file", Path: database, RawQuery: "mode=ro&immutable=1"}
	snapshot, err := sql.Open("sqlite", u.String())
	if err != nil {
		return err
	}
	err = integrity(snapshot)
	snapshot.Close()
	if err != nil {
		return err
	}
	m := manifest{Version: 1}
	if m.State, err = checksum(database); err != nil {
		return err
	}
	if m.Config, err = checksum(cfg); err != nil {
		return err
	}
	meta, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dest, "manifest.json"), meta, 0600)
}

// Restore publishes a checked and reconciled database only into an empty directory.
// Explicitly invalidate copied credentials, which could predate a revocation.
func Restore(src, dataDir string) error {
	if src == "" {
		return errors.New("provide --from for the backup directory")
	}
	meta, err := os.ReadFile(filepath.Join(src, "manifest.json"))
	if err != nil {
		return err
	}
	var m manifest
	if err = json.Unmarshal(meta, &m); err != nil || m.Version != 1 {
		return errors.New("unsupported backup manifest")
	}
	cfg, err := checksum(filepath.Join(src, "server.json"))
	if err != nil {
		return err
	}
	if cfg != m.Config {
		return errors.New("backup configuration checksum mismatch")
	}
	if err = os.MkdirAll(dataDir, 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return errors.New("restore requires an empty data directory")
	}
	staging, err := os.MkdirTemp(dataDir, ".restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	tmp := filepath.Join(staging, "state.db")
	if err = copyDatabase(filepath.Join(src, "state.db"), tmp, m.State); err != nil {
		return err
	}
	db, err := state.Open(tmp)
	if err != nil {
		return err
	}
	defer db.Close()
	if err = integrity(db); err != nil {
		return err
	}
	if err = (&state.Store{DB: db}).Init(); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`DELETE FROM auth_sessions;
		DELETE FROM enrollment_tokens;
		UPDATE machines SET revoked=1,credential_hash=NULL;`)
	if err != nil {
		return err
	}
	var codingTable int
	if err = tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='harness_clients'").Scan(&codingTable); err != nil {
		return err
	}
	if codingTable > 0 {
		if _, err = tx.Exec("UPDATE harness_clients SET revoked=1"); err != nil {
			return err
		}
	}
	if err = state.InterruptRestoredAttempts(tx); err != nil {
		return err
	}
	if err = state.AuditTx(tx, "local administrator", "cluster.restore", ""); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if _, err = db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return err
	}
	if err = db.Close(); err != nil {
		return err
	}
	return os.Link(tmp, filepath.Join(dataDir, "state.db"))
}
func copyDatabase(src, dest, digest string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), f)
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if hex.EncodeToString(h.Sum(nil)) != digest {
		return errors.New("backup database checksum mismatch")
	}
	return nil
}
