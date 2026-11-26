package githubqueue

import (
	"encoding/json"
	"os"
	"testing"
)

func TestShippedExampleAllowlistIsValid(t *testing.T) {
	data, err := os.ReadFile("../../infrastructure/priority-queue/example.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err = json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if err = cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	priorities := map[string]Priority{}
	for _, repo := range cfg.Repositories {
		priorities[repo.Name] = repo.Priority
	}
	if priorities["example-org/application"] != High || priorities["example-org/documentation"] != Low || len(priorities) != 2 {
		t.Fatal(priorities)
	}
}
