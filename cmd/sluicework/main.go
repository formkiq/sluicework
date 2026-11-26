// Sluicework runs a single orchestrator or an outbound worker.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"sluicework/internal/api"
	"sluicework/internal/auth"
	"sluicework/internal/backup"
	"sluicework/internal/clusterconfig"
	"sluicework/internal/githubapp"
	"sluicework/internal/provider"
	"sluicework/internal/schedule"
	"sluicework/internal/state"
	"sluicework/internal/worker"
	"sluicework/web"
)

const version = "0.1.0"
const usage = "sluicework {init|server|join|worker|workstation|workstation-install|workstation-enable|workstation-pause|workstation-status|github-queue|enroll-token|user|local-ai|open-webui|codex|personal-agent|coding-client|install-cli|backup|restore|version} --config FILE; sluicework {runtime-check|open-webui-check} --endpoint URL"

type serverConfig struct {
	GitHub   *githubapp.Config `json:"github,omitempty"`
	DataDir  string            `json:"dataDir"`
	Listen   string            `json:"listen"`
	TLSCert  string            `json:"tlsCert"`
	TLSKey   string            `json:"tlsKey"`
	LogLimit int               `json:"logLimit,omitempty"`
}

func readConfig(path string, dest any) error {
	if path == "" {
		return errors.New("provide --config with the path to a JSON config")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err = d.Decode(dest); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err = d.Decode(&struct{}{}); err != io.EOF {
		return errors.New("trailing data in config")
	}
	return nil
}
func stdinLine(in io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(in, 4097)).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	if len(line) > 4096 {
		return "", errors.New("input is too long")
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if line == "" {
		return "", errors.New("expected a nonempty line on stdin")
	}
	return line, nil
}
func main() {
	syscall.Umask(0077)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := execute(ctx, os.Args[1:], os.Stdin, os.Stdout); err != nil {
		log.Println(err)
		os.Exit(1)
	}
}
func execute(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	command := args[0]
	if command == "chatgpt-login" {
		return errors.New("use personal-agent login; subscription credentials stay in your private runtime")
	}
	if command == "personal-agent" {
		return personalCommand(ctx, args[1:], in, out)
	}
	if command == "version" || command == "--version" {
		fmt.Fprintln(out, version)
		return nil
	}
	if command == "help" || command == "--help" {
		fmt.Fprintln(out, usage)
		return nil
	}
	if command == "install-cli" {
		return installCLICommand(args[1:], out)
	}
	if command == "coding-client" {
		return harnessClientCommand(ctx, args[1:], out)
	}
	if command == "codex" {
		return codexCommand(ctx, args[1:], in, out)
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	path := f.String("config", "", "JSON configuration file")
	var dest, src, name, role, endpoint, caFile, ollamaURL string
	switch command {
	case "backup":
		f.StringVar(&dest, "out", "", "new backup directory")
	case "restore":
		f.StringVar(&src, "from", "", "backup directory")
	case "user":
		f.StringVar(&name, "name", "", "username")
		f.StringVar(&role, "role", "operator", "operator or admin")
	case "local-ai", "runtime-check":
		f.StringVar(&endpoint, "endpoint", "http://127.0.0.1:11434", "Ollama or Herd origin")
		f.StringVar(&name, "name", "Local AI", "connection name")
		if command == "runtime-check" {
			f.StringVar(&caFile, "ca", "", "additional trusted PEM CA certificate")
		}
	case "open-webui", "open-webui-check":
		f.StringVar(&endpoint, "endpoint", "http://127.0.0.1:8080", "Open WebUI origin")
		if command == "open-webui" {
			f.StringVar(&ollamaURL, "ollama-url", "http://127.0.0.1:11434", "Ollama or Herd origin used by Open WebUI")
		} else {
			f.StringVar(&caFile, "ca", "", "additional trusted PEM CA certificate")
		}
	case "github-queue", "init", "server", "worker", "join", "enroll-token", "workstation", "workstation-install", "workstation-enable", "workstation-pause", "workstation-status", "workstation-check":
	default:
		return errors.New(usage)
	}
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New(usage)
	}
	if command == "github-queue" {
		return runGitHubQueue(ctx, *path, out)
	}
	if command == "runtime-check" {
		info, err := provider.ProbeLocalWithCA(ctx, endpoint, caFile)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(info)
	}
	if command == "open-webui-check" {
		if err := clusterconfig.ProbeOpenWebUI(ctx, endpoint, caFile); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]any{"endpoint": endpoint, "ok": true})
	}
	if command == "worker" || command == "join" || strings.HasPrefix(command, "workstation") {
		var cfg worker.Config
		if err := readConfig(*path, &cfg); err != nil {
			return err
		}
		if cfg.Version == "" {
			cfg.Version = version
		}
		if strings.HasPrefix(command, "workstation") {
			return worker.WorkstationCommand(ctx, cfg, command, out)
		}
		if command == "worker" {
			if cfg.Workstation != nil {
				return errors.New("use the workstation command with a workstation config")
			}
			return worker.Run(ctx, cfg)
		}
		token, err := stdinLine(in)
		if err != nil {
			return err
		}
		return worker.Join(ctx, cfg, token)
	}
	var cfg serverConfig
	if err := readConfig(*path, &cfg); err != nil {
		return err
	}
	if !filepath.IsAbs(cfg.DataDir) {
		return errors.New("dataDir must be an absolute directory")
	}
	if command == "restore" {
		return backup.Restore(src, cfg.DataDir)
	}
	database := filepath.Join(cfg.DataDir, "state.db")
	if command != "init" {
		if _, err := os.Stat(database); err != nil {
			return fmt.Errorf("open cluster state (use init to create a cluster): %w", err)
		}
	}
	db, err := state.Open(database)
	if err != nil {
		return err
	}
	defer db.Close()
	if command == "backup" {
		return backup.Create(db, *path, dest)
	}
	st := &state.Store{DB: db, LogLimit: cfg.LogLimit}
	if cfg.LogLimit < 0 || cfg.LogLimit > 64<<20 {
		return errors.New("logLimit must be between 0 and 67108864 bytes per attempt")
	}
	if err = st.Init(); err != nil {
		return err
	}
	if command == "enroll-token" {
		token, e := st.EnrollToken("local administrator")
		if e == nil {
			fmt.Fprintln(out, token)
		}
		return e
	}
	if command == "local-ai" {
		p, err := provider.OpenForConfiguration(db, filepath.Join(cfg.DataDir, "secrets"))
		if err != nil {
			return err
		}
		defer p.Close()
		conn, err := p.EnsureLocal(provider.WithActor(ctx, "local administrator"), provider.Config{
			Name: name, Kind: "ollama", Endpoint: endpoint, Models: []string{"qwen3.5:4b"},
			LocalOptions: provider.LocalOptions{ContextLength: 4096},
		})
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(conn)
	}
	if command == "open-webui" {
		settings, err := clusterconfig.Open(db, clusterconfig.Seed{DataDir: cfg.DataDir, Listen: cfg.Listen, GitHub: cfg.GitHub, LogLimit: cfg.LogLimit})
		if err != nil {
			return err
		}
		defer settings.Close()
		view, err := settings.EnsureOpenWebUI("local administrator", endpoint, ollamaURL)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(view)
	}
	users, err := auth.New(db)
	if err != nil {
		return err
	}
	if command == "init" || command == "user" {
		first := command == "init"
		if first {
			name, role = "admin", "admin"
		}
		pass, err := stdinLine(in)
		if err != nil {
			return err
		}
		if !first {
			return users.SetUser(name, role, pass, false)
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err = auth.SetUserTx(tx, name, role, pass, true); err != nil {
			return err
		}
		if _, err = state.SaveJobTx(tx, "local administrator", "", state.Job{Name: "Worker check", Type: "script", Command: "/bin/echo", Args: []string{"Sluicework worker is ready"}, TimeoutSeconds: 15, SafeToRepeat: true}); err != nil {
			return err
		}
		return tx.Commit()
	}
	return serve(ctx, cfg, db, st, users)
}
func listenConfig(c serverConfig) (string, bool, error) {
	addr := c.Listen
	if addr == "" {
		addr = "127.0.0.1:7443"
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "", false, err
	}
	encrypted := c.TLSCert != "" && c.TLSKey != ""
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return "", false, errors.New("configure both tlsCert and tlsKey")
	}
	if !encrypted && !(net.ParseIP(host).IsLoopback() || host == "localhost") {
		return "", false, errors.New("a LAN listener requires a TLS certificate and key")
	}
	return addr, encrypted, nil
}
func serve(parent context.Context, cfg serverConfig, db *sql.DB, st *state.Store, users *auth.Service) error {
	addr, encrypted, err := listenConfig(cfg)
	if err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(cfg.DataDir, "server.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another server holds the cluster data directory")
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer l.Close()
	providers, err := provider.Open(db, filepath.Join(cfg.DataDir, "secrets"))
	if err != nil {
		return err
	}
	defer providers.Close()
	schedules := schedule.New(db, st)
	if err = schedules.Init(); err != nil {
		return err
	}
	if err = st.Reconcile(); err != nil {
		return err
	}
	ctx, stop := context.WithCancel(parent)
	defer stop()
	configuration, err := clusterconfig.Open(db, clusterconfig.Seed{DataDir: cfg.DataDir, Listen: addr, GitHub: cfg.GitHub, LogLimit: cfg.LogLimit})
	if err != nil {
		return err
	}
	defer configuration.Close()
	st.LogLimitPolicy = configuration.LogLimit
	app := api.Server{State: st, Auth: users, Provider: providers, Schedule: schedules, Configuration: configuration}
	app.GitHub, err = configuration.OpenGitHub()
	if err != nil {
		log.Print("GitHub connection needs attention in Settings")
		err = nil
	}
	handler := app.Handler(web.Handler())
	if err := configuration.StartNetwork(handler); err != nil {
		log.Print("Network HTTPS needs attention in Settings; primary access remains available")
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 90 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 64 << 10, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan error, 6)
	go func() { done <- schedules.Start(ctx) }()
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				done <- nil
				return
			case <-ticker.C:
				if err := st.Reconcile(); err != nil {
					done <- err
					return
				}
			}
		}
	}()
	go func() {
		if encrypted {
			done <- server.ServeTLS(l, cfg.TLSCert, cfg.TLSKey)
		} else {
			done <- server.Serve(l)
		}
	}()
	log.Printf("Sluicework %s listening on %s", version, l.Addr())
	pending := 3
	{
		pending++
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					done <- nil
					return
				case <-ticker.C:
					if broker := app.Broker(); broker != nil {
						if e := broker.Reconcile(ctx, st); e != nil && ctx.Err() == nil {
							log.Print("GitHub runner reconciliation will retry")
						}
					}
				}
			}
		}()
		pending++
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				if broker := app.Broker(); broker != nil {
					if machines, e := st.Machines(); e == nil {
						broker.RefreshActivity(ctx, machines)
					}
				}
				select {
				case <-ctx.Done():
					done <- nil
					return
				case <-ticker.C:
				}
			}
		}()
	}
	select {
	case <-ctx.Done():
	case err = <-done:
		pending--
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if closeErr := server.Shutdown(shutdown); closeErr != nil {
		server.Close()
		if err == nil {
			err = closeErr
		}
	}
	for ; pending > 0; pending-- {
		other := <-done
		if (err == nil || errors.Is(err, http.ErrServerClosed)) && other != nil && !errors.Is(other, http.ErrServerClosed) {
			err = other
		}
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
