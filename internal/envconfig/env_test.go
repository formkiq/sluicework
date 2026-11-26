package envconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestReadFileTreatsValuesAsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env.local")
	raw := "# private development settings\r\nexport CONFIG = '/tmp/with spaces/server.json' # note\r\nEMPTY=\nURL=https://example.com/#fragment\nLITERAL=\"$(touch /tmp/never-executed) ${HOME}\"\nHASH='a # b'\nESCAPED=\"a\\\"b\"\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(path)
	want := map[string]string{"CONFIG": "/tmp/with spaces/server.json", "EMPTY": "", "URL": "https://example.com/#fragment", "LITERAL": "$(touch /tmp/never-executed) ${HOME}", "HASH": "a # b", "ESCAPED": "a\"b"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal(got, err)
	}
}

func TestReadFileRejectsInvalidInputWithoutExposingValues(t *testing.T) {
	for _, raw := range []string{"SECRET=\"sensitive-unclosed", "SECRET='sensitive' trailing", "SECRET=sensitive\nSECRET=other", "not a valid assignment sensitive", "SECRET=\"sensitive\\nvalue\"", "SECRET=sensitive\x00value"} {
		path := filepath.Join(t.TempDir(), ".env.local")
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		values, err := ReadFile(path)
		if err == nil || values != nil || strings.Contains(err.Error(), "sensitive") {
			t.Fatal(values, err)
		}
	}
}

func TestReadFileRequiresBoundedPrivateRegularFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "environment")
	if err := os.WriteFile(file, []byte("NAME=value"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(file); err == nil {
		t.Fatal("accepted a public-readable environment file")
	}
	if err := os.Chmod(file, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(link); err == nil {
		t.Fatal("accepted a symlink")
	}
	pipe := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(pipe, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(pipe); err == nil {
		t.Fatal("accepted a named pipe")
	}
	if err := os.WriteFile(file, []byte(strings.Repeat("#", maxBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(file); err == nil {
		t.Fatal("accepted an oversized file")
	}
}
