package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"sluicework/internal/clusterconfig"
	"sluicework/internal/codexclient"
	"sluicework/internal/provider"
	"sluicework/internal/state"
)

// This is the headless equivalent of AI connections > Coding clients. It never
// prints a credential, replaces an existing file or restarts active inference.
func harnessClientCommand(ctx context.Context, args []string, out io.Writer) error {
	f := flag.NewFlagSet("coding-client", flag.ContinueOnError)
	f.SetOutput(out)
	config := f.String("config", os.Getenv("SLUICEWORK_CONFIG"), "server configuration (defaults to SLUICEWORK_CONFIG)")
	connection := f.String("connection", "", "existing AI connection name or ID")
	models := f.String("models", "", "comma-separated permitted installed model tags")
	name := f.String("name", "", "coding client name")
	server := f.String("server-url", "", "HTTPS orchestrator origin reachable from the client")
	dest := f.String("out", "", "new private client file (never printed or overwritten)")
	enable := f.Bool("enable", false, "enable the authenticated coding gateway")
	slots := f.Int("shared-slots", 0, "shared local inference slots, 1–4 (0 preserves current setting)")
	contextSize := f.Int("context", 16384, "coding context tokens")
	output := f.Int("max-output", 2048, "maximum output tokens per request")
	timeout := f.Int("timeout", provider.DefaultHarnessTimeoutSeconds, "request timeout seconds (15–1800)")
	existing := f.String("client-id", "", "update the timeout of an existing coding client")
	days := f.Int("days", 7, "credential lifetime days")
	if e := f.Parse(args); e != nil {
		return e
	}
	if *existing != "" {
		if *config == "" {
			return errors.New("provide --config with --client-id")
		}
		var unsupported string
		f.Visit(func(v *flag.Flag) {
			if v.Name != "config" && v.Name != "client-id" && v.Name != "timeout" {
				unsupported = v.Name
			}
		})
		if unsupported != "" {
			return fmt.Errorf("--client-id only supports --config and --timeout; unexpected --%s", unsupported)
		}
		return updateHarnessTimeout(ctx, *config, *existing, *timeout, out)
	}
	if *config == "" || *connection == "" || *models == "" || *name == "" || *server == "" || *dest == "" {
		return errors.New("provide --config, --connection, --models, --name, --server-url and --out")
	}
	origin, e := codexclient.Origin(*server)
	if e != nil {
		return e
	}
	file, e := os.OpenFile(*dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	success := false
	defer func() {
		file.Close()
		if !success {
			os.Remove(*dest)
		}
	}()
	var cfg serverConfig
	if e = readConfig(*config, &cfg); e != nil {
		return e
	}
	db, e := state.Open(cfg.DataDir + "/state.db")
	if e != nil {
		return e
	}
	defer db.Close()
	p, e := provider.OpenForConfiguration(db, cfg.DataDir+"/secrets")
	if e != nil {
		return e
	}
	defer p.Close()
	connections, e := p.List(ctx)
	if e != nil {
		return e
	}
	id := ""
	for _, c := range connections {
		if c.ID == *connection || c.Name == *connection {
			if id != "" && id != c.ID {
				return errors.New("ambiguous connection; select its ID")
			}
			id = c.ID
		}
	}
	if id == "" {
		return errors.New("AI connection not found")
	}
	allowed := []string{}
	for _, m := range strings.Split(*models, ",") {
		allowed = append(allowed, strings.TrimSpace(m))
	}
	manager, e := clusterconfig.Open(db, clusterconfig.Seed{DataDir: cfg.DataDir, Listen: cfg.Listen, GitHub: cfg.GitHub, LogLimit: cfg.LogLimit})
	if e != nil {
		return e
	}
	defer manager.Close()
	network := manager.Snapshot().Network
	ca := ""
	if network.Enabled && strings.EqualFold(network.URL, origin) {
		ca = network.CAPEM
	}
	policy, e := p.HarnessPolicy(ctx)
	if e != nil {
		return e
	}
	if *slots != 0 {
		policy.MaxConcurrent = *slots
	}
	if *enable {
		policy.Enabled = true
	}
	if policy.MaxConcurrent < 1 || policy.MaxConcurrent > 4 {
		return errors.New("shared slots must be 1–4")
	}
	c, token, e := p.CreateHarnessClient(ctx, "local administrator", provider.HarnessClientInput{Name: *name, ConnectionID: id, Models: allowed, ContextLength: *contextSize, MaxOutputTokens: *output, MaxConcurrent: 1, TimeoutSeconds: *timeout, LifetimeDays: *days})
	if e != nil {
		return e
	}
	defer func() {
		if !success {
			_ = p.RevokeHarnessClient(context.Background(), "local administrator", c.ID)
		}
	}()
	bundle := codexclient.Bundle{Version: 1, ServerURL: origin, Token: token, ClientID: c.ID, Name: c.Name, Models: c.Models, ContextLength: c.ContextLength, MaxOutputTokens: c.MaxOutputTokens, Expires: c.Expires, CAPEM: ca}
	if e = json.NewEncoder(file).Encode(bundle); e != nil {
		return e
	}
	if e = file.Sync(); e != nil {
		return e
	}
	if e = p.SetHarnessPolicy(ctx, "local administrator", policy); e != nil {
		return e
	}
	success = true
	fmt.Fprintf(out, "Created coding client %s. Private configuration: %s\nGateway: %s; enabled: %t; shared slots: %d\n", c.Name, *dest, origin, policy.Enabled, policy.MaxConcurrent)
	if ca != "" {
		fp, _ := codexclient.Fingerprint(ca)
		fmt.Fprintln(out, "Public CA SHA-256:", fp)
	}
	return nil
}

func updateHarnessTimeout(ctx context.Context, config, id string, seconds int, out io.Writer) error {
	var cfg serverConfig
	if e := readConfig(config, &cfg); e != nil {
		return e
	}
	db, e := state.Open(cfg.DataDir + "/state.db")
	if e != nil {
		return e
	}
	defer db.Close()
	p, e := provider.OpenForConfiguration(db, cfg.DataDir+"/secrets")
	if e != nil {
		return e
	}
	defer p.Close()
	if e = p.SetHarnessClientTimeout(ctx, "local administrator", id, seconds); e != nil {
		return e
	}
	fmt.Fprintf(out, "Coding client %s request timeout: %d seconds. Applies to subsequent requests; credential and expiry unchanged.\n", id, seconds)
	return nil
}
