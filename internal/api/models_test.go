package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"sluicework/internal/provider"
)

func TestModelSetupPermissionsAndInstall(t *testing.T) {
	c := setup(t)
	for _, who := range []identity{{}, c.operator} {
		code := 401
		if who.cookie != nil {
			code = 403
		}
		c.do("GET", "/model-catalog", nil, who, code, nil)
		c.do("GET", "/connections/anything/models", nil, who, code, nil)
		c.do("POST", "/connections/anything/models", map[string]string{"model": "qwen3.5:4b"}, who, code, nil)
		c.do("POST", "/connections/anything/models/install/cancel", nil, who, code, nil)
	}
	var presets []provider.ModelPreset
	c.do("GET", "/model-catalog", nil, c.admin, 200, &presets)
	if len(presets) != 5 || presets[4].Installable {
		t.Fatal("embedding should remain deferred")
	}
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/pull":
			var b struct{ Model string }
			if json.NewDecoder(r.Body).Decode(&b) != nil || b.Model != "qwen3.5:4b" {
				t.Error("invalid model")
			}
			fmt.Fprintln(w, `{"status":"success"}`)
		case "/api/tags":
			fmt.Fprintln(w, `{"models":[{"name":"qwen3.5:4b","size":4000000000}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer runtime.Close()
	var conn provider.Connection
	c.do("POST", "/connections", map[string]any{"name": "Mac AI", "kind": "ollama", "endpoint": runtime.URL, "models": []string{"qwen3.5:4b"}, "contextLength": 4096, "keepAliveSeconds": 0}, c.admin, 200, &conn)
	path := "/connections/" + conn.ID + "/models"
	c.do("POST", path, map[string]string{"model": "qwen3-embedding:0.6b"}, c.admin, 400, nil)
	var install provider.Installation
	c.do("POST", path, map[string]string{"model": "qwen3.5:4b"}, c.admin, 200, &install)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var inventory provider.ModelInventory
		c.do("GET", path, nil, c.admin, 200, &inventory)
		if len(inventory.Installations) == 1 && inventory.Installations[0].Status == "succeeded" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("model installation did not complete over API")
}
