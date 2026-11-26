package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"sluicework/internal/codexclient"
)

func codexCommand(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	f := flag.NewFlagSet("codex", flag.ContinueOnError)
	f.SetOutput(out)
	file := f.String("client", "", "private client JSON downloaded from Sluicework admin")
	trust := f.String("trust-ca", "", "verified cluster CA SHA-256 fingerprint")
	model := f.String("model", "", "one model permitted by this coding client")
	cwd := f.String("cwd", "", "local project directory (defaults to current directory)")
	sandbox := f.String("sandbox", "workspace-write", "read-only or workspace-write")
	binary := f.String("codex", "codex", "Codex CLI executable on this client")
	check := f.Bool("check", false, "verify server, TLS, credential and permitted models without launching Codex")
	noninteractive := f.Bool("exec", false, "run the prompt once without the interactive terminal")
	if e := f.Parse(args); e != nil {
		return e
	}
	if *file == "" {
		return errors.New("provide --client; the Sluicework server URL comes from that file and has no localhost default")
	}
	b, e := codexclient.Load(*file)
	if e != nil {
		return e
	}
	client, e := codexclient.New(b, *trust)
	if e != nil {
		return e
	}
	if *check {
		models, e := client.Models(ctx)
		if e != nil {
			return e
		}
		fmt.Fprintf(out, "Connected to %s\nPermitted models: %s\n", b.ServerURL, strings.Join(models, ", "))
		return nil
	}
	if *cwd == "" {
		*cwd, e = os.Getwd()
		if e != nil {
			return e
		}
	}
	return client.Run(ctx, codexclient.RunOptions{Binary: *binary, Directory: *cwd, Model: *model, Sandbox: *sandbox, Exec: *noninteractive, Prompt: strings.Join(f.Args(), " "), In: in, Out: out, Err: os.Stderr})
}
