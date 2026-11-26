package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvironmentConfigurationPrecedence(t *testing.T) {
	for _, mode := range []string{"file", "process", "flag"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SLUICEWORK_CONFIG", "")
			if err := os.Unsetenv("SLUICEWORK_CONFIG"); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			configs := map[string]string{}
			for _, name := range []string{"file", "process", "flag"} {
				path := filepath.Join(dir, name+".json")
				raw, _ := json.Marshal(serverConfig{DataDir: filepath.Join(dir, name)})
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				configs[name] = path
			}
			env := filepath.Join(dir, ".env.local")
			if err := os.WriteFile(env, []byte("SLUICEWORK_CONFIG="+configs["file"]+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"--env-file", env, "init"}
			if mode != "file" {
				t.Setenv("SLUICEWORK_CONFIG", configs["process"])
			}
			if mode == "flag" {
				args = append(args, "--config", configs["flag"])
			}
			if err := execute(context.Background(), args, strings.NewReader("environment-fixture-password"), &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			for name := range configs {
				_, err := os.Stat(filepath.Join(dir, name, "state.db"))
				if (err == nil) != (name == mode) {
					t.Fatalf("unexpected configuration selected: %s (%v)", name, err)
				}
			}
		})
	}
}

func TestEnvironmentFilesAreNeverImplicitAndParseBeforeApplying(t *testing.T) {
	t.Setenv("SLUICEWORK_CONFIG", "")
	if err := os.Unsetenv("SLUICEWORK_CONFIG"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Chdir(dir)
	file := filepath.Join(dir, ".env.local")
	if err := os.WriteFile(file, []byte("SLUICEWORK_CONFIG=/tmp/example/server.json\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := execute(context.Background(), []string{"version"}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, exists := os.LookupEnv("SLUICEWORK_CONFIG"); exists {
		t.Fatal("loaded a workspace environment without explicit consent")
	}
	if err := os.WriteFile(file, []byte("SLUICEWORK_CONFIG=/tmp/example/server.json\ninvalid sensitive input"), 0600); err != nil {
		t.Fatal(err)
	}
	err := execute(context.Background(), []string{"--env-file=" + file, "version"}, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatal(err)
	}
	if _, exists := os.LookupEnv("SLUICEWORK_CONFIG"); exists {
		t.Fatal("partially applied an invalid file")
	}
}
