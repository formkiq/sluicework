package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"
	"sluicework/internal/personal"
	"sluicework/internal/provider"
)

func personalCommand(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("personal-agent {enroll|start|install-service|login|models|status|preview|approve|pause|resume|revoke|results|sign-out|stop}")
	}
	operation := args[0]
	f := flag.NewFlagSet("personal-agent "+operation, flag.ContinueOnError)
	f.SetOutput(out)
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := f.String("dir", filepath.Join(home, ".local", "share", "sluicework", "personal-agent"), "private OS-user-owned runtime directory")
	server := f.String("server", "", "shared Sluicework HTTPS origin")
	ticket := f.String("ticket", "", "one-use enrollment ticket ID from your signed-in console")
	owner := f.String("owner", "", "your signed-in cluster username")
	ca := f.String("ca", "", "verified public cluster CA PEM file")
	name := f.String("name", "My personal agent", "runtime display name")
	if err = f.Parse(args[1:]); err != nil {
		return err
	}
	if operation == "enroll" {
		if *server == "" || *ticket == "" || *owner == "" {
			return errors.New("provide --server, --ticket and --owner from your signed-in Sluicework console")
		}
		v, err := personal.OpenVault(*dir)
		if err != nil {
			return err
		}
		defer v.Close()
		if _, err := os.Lstat(filepath.Join(v.Dir, "runtime.json")); !os.IsNotExist(err) {
			return errors.New("this runtime is already enrolled; its owner and server cannot be changed remotely")
		}
		cert := ""
		if *ca != "" {
			b, err := os.ReadFile(*ca)
			if err != nil {
				return err
			}
			cert = string(b)
		}
		network, err := personal.NewNetwork(*server, cert)
		if err != nil {
			return err
		}
		key, err := v.Key()
		if err != nil {
			return err
		}
		fmt.Fprint(out, "One-use enrollment code: ")
		code, err := hiddenInput(in, out)
		if err != nil {
			return err
		}
		enrolled, err := network.Enroll(ctx, key, *ticket, strings.TrimSpace(code), *name)
		if err != nil {
			return err
		}
		if enrolled.Owner != *owner {
			return errors.New("enrollment owner differs from the signed-in user; configuration was not saved")
		}
		err = v.SaveConfig(personal.Config{Server: network.Server, Owner: enrolled.Owner, RuntimeID: enrolled.ID, UID: v.UID, Name: *name, CA: cert})
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "Enrolled for", enrolled.Owner, "in this OS login. Runtime fingerprint:", enrolled.ID)
		fmt.Fprintln(out, "Compare this public fingerprint with your console before preparing tasks. Start with: sluicework personal-agent start")
		return nil
	}
	if operation == "install-service" {
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		if err = personal.InstallUserService(*dir, binary); err != nil {
			return err
		}
		fmt.Fprintln(out, "Installed a login service for your OS account only. Existing local pauses and approvals are preserved.")
		return nil
	}
	if operation == "start" {
		v, err := personal.OpenVault(*dir)
		if err != nil {
			return err
		}
		defer v.Close()
		a, err := personal.OpenAgent(v)
		if err != nil {
			return err
		}
		defer a.Close()
		fmt.Fprintln(out, "Personal agent running for", a.Config.Owner, "with private local controls. New runtimes start paused.")
		return a.Serve(ctx)
	}
	id := ""
	if len(f.Args()) > 0 {
		id = f.Args()[0]
	}
	if len(f.Args()) > 1 {
		return errors.New("provide at most one proposal ID")
	}
	call := func(request personal.Control, out any) error { return personal.LocalCall(ctx, *dir, request, out) }
	if operation == "login" {
		fmt.Fprintln(out, "This connects your own ChatGPT plan inside your private runtime. Use an OS account that does not execute other users' CI or worker jobs; same-UID programs have the same authority. Verify OpenAI deployment eligibility. Credentials will stay in this OS account; scheduled usage requires separate local task approval.")
		if err := confirmLocal(in, out, "Type connect to sign in: ", "connect"); err != nil {
			return err
		}
		var signIn provider.PlanSignIn
		if err := call(personal.Control{Operation: "login", Consent: true}, &signIn); err != nil {
			return err
		}
		binary := "open"
		if runtime.GOOS == "linux" {
			binary = "xdg-open"
		}
		if err := exec.CommandContext(ctx, binary, signIn.AuthorizationURL).Run(); err != nil {
			return errors.New("cannot open your local system browser")
		}
		timer := time.NewTicker(time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				var status provider.PlanSignIn
				if err := call(personal.Control{Operation: "login-status", ID: signIn.AttemptID}, &status); err != nil {
					return err
				}
				if status.Status == "succeeded" {
					fmt.Fprintln(out, "You are using your ChatGPT plan. Manage allowance at https://chatgpt.com/settings/usage. Approve each background task locally before enabling it.")
					return nil
				}
				if status.Status != "pending" && status.Status != "processing" {
					return errors.New(status.Message)
				}
			}
		}
	}
	if operation == "approve" || operation == "preview" {
		if id == "" {
			return errors.New("provide the proposal ID shown in the console")
		}
		var preview personal.Preview
		if err := call(personal.Control{Operation: "preview", ID: id}, &preview); err != nil {
			return err
		}
		b, _ := json.MarshalIndent(preview, "", "  ")
		fmt.Fprintln(out, string(b))
		fmt.Fprintln(out, "This exact prompt, model, cron, timezone, expiry, daily cap, browsing/notification permissions and result-sharing choice will run using the displayed personal account. SIWC has no per-request token cap.")
		if preview.Proposal.Manifest.WebSearch == nil {
			fmt.Fprintln(out, "Public web research: disabled.")
		} else if len(preview.Proposal.Manifest.WebSearch.AllowedDomains) == 0 {
			fmt.Fprintln(out, "Public web research: enabled across public websites; queries go to OpenAI. No signed-in websites or local tools.")
		} else {
			fmt.Fprintln(out, "Public web research: enabled; search results restricted to these domains and subdomains:", strings.Join(preview.Proposal.Manifest.WebSearch.AllowedDomains, ", "))
		}
		if preview.Proposal.Manifest.DesktopNotifications {
			fmt.Fprintln(out, "Desktop notifications: status-only alerts on this runtime's computer after success or failure; no report content.")
		} else {
			fmt.Fprintln(out, "Desktop notifications: disabled.")
		}
		if operation == "preview" {
			return nil
		}
		if err := confirmLocal(in, out, "Type approve to authorize background execution: ", "approve"); err != nil {
			return err
		}
		if err := call(personal.Control{Operation: "approve", ID: id, Digest: preview.Digest}, nil); err != nil {
			return err
		}
		fmt.Fprintln(out, "Locally approved. Run personal-agent resume when you want approved tasks to run. Remote proposal edits require a new local approval.")
		return nil
	}
	allowed := map[string]bool{"status": true, "models": true, "results": true, "pause": true, "resume": true, "revoke": true, "sign-out": true, "stop": true}
	if !allowed[operation] {
		return errors.New("unknown personal agent operation")
	}
	if operation == "revoke" && id == "" {
		return errors.New("provide the locally approved proposal ID")
	}
	var result json.RawMessage
	if err := call(personal.Control{Operation: operation, ID: id}, &result); err != nil {
		return err
	}
	if string(result) != "null" {
		fmt.Fprintln(out, string(result))
	} else {
		fmt.Fprintln(out, "Personal agent:", operation)
	}
	return nil
}
func hiddenInput(in io.Reader, out io.Writer) (string, error) {
	file, ok := in.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return "", errors.New("use your signed-in user's interactive terminal; secrets are not accepted as command arguments or redirected input")
	}
	b, err := term.ReadPassword(int(file.Fd()))
	fmt.Fprintln(out)
	return string(b), err
}
func confirmLocal(in io.Reader, out io.Writer, prompt, want string) error {
	file, ok := in.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return errors.New("local subscription consent requires your interactive terminal")
	}
	fmt.Fprint(out, prompt)
	line, err := bufio.NewReader(io.LimitReader(in, 4096)).ReadString('\n')
	if err != nil {
		return err
	}
	if strings.TrimSpace(line) != want {
		return errors.New("cancelled without changing local approval")
	}
	return nil
}
