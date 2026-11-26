package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sluicework/internal/cliinstall"
)

func installCLICommand(args []string, out io.Writer) error {
	f := flag.NewFlagSet("install-cli", flag.ContinueOnError)
	f.SetOutput(out)
	system := f.Bool("system", false, "link /usr/local/bin/sluicework for all users (administrator only)")
	noShell := f.Bool("no-shell", false, "create the user command link without editing shell startup files")
	if err := f.Parse(args); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil && !*system {
		return err
	}
	result, err := cliinstall.Install(cliinstall.Options{Executable: executable, Home: home, Shell: os.Getenv("SHELL"), Path: os.Getenv("PATH"), ZDotDir: os.Getenv("ZDOTDIR"), System: *system, NoShell: *noShell})
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "Sluicework command:", result.Command)
	for _, profile := range result.Profiles {
		fmt.Fprintln(out, "Added PATH setup:", profile)
	}
	if len(result.Profiles) > 0 {
		fmt.Fprintln(out, "Open a new terminal to use sluicework, or run: export PATH=\"$HOME/.local/bin:$PATH\"")
	} else if !*system && *noShell {
		fmt.Fprintln(out, "If needed, add $HOME/.local/bin to PATH.")
	}
	return nil
}
