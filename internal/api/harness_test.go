package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"sluicework/internal/codexclient"
	"sluicework/internal/provider"
)

func TestCodingAccessCannotBecomeConsoleOrWorkerAccess(t *testing.T) {
	c := setup(t)
	conn, e := c.api.Provider.Create(context.Background(), provider.Config{Name: "Model host", Kind: "ollama", Endpoint: "https://model.private", Models: []string{"qwen"}})
	if e != nil {
		t.Fatal(e)
	}
	input := map[string]any{"name": "Remote laptop", "serverUrl": "https://cluster.private:8443", "connectionId": conn.ID, "models": []string{"qwen"}, "contextLength": 16384, "maxOutputTokens": 2048, "maxConcurrent": 1, "timeoutSeconds": 300, "lifetimeDays": 7}
	c.do("POST", "/harness/clients", input, c.operator, 403, nil)
	c.do("GET", "/harness", nil, c.operator, 403, nil)
	var receipt struct {
		Client codexclient.Bundle `json:"client"`
	}
	c.do("POST", "/harness/clients", input, c.admin, 200, &receipt)
	if receipt.Client.ServerURL != "https://cluster.private:8443" || receipt.Client.Token == "" {
		t.Fatal("missing explicit remote configuration")
	}
	coding := identity{bearer: receipt.Client.Token}
	c.do("GET", "/settings", nil, coding, 401, nil)
	c.do("GET", "/worker/status", nil, coding, 401, nil)
	c.do("GET", "/harness/v1/models", nil, c.admin, 401, nil)
	c.do("GET", "/harness/v1/models", nil, coding, 503, nil)
	c.do("PUT", "/harness/policy", map[string]any{"enabled": true, "maxConcurrent": 1}, c.admin, 204, nil)
	raw := c.do("GET", "/harness/v1/models", nil, coding, 200, nil)
	var inventory struct{ Data []struct{ ID string } }
	_ = json.Unmarshal(raw, &inventory)
	if len(inventory.Data) != 1 || inventory.Data[0].ID != "qwen" {
		t.Fatal(string(raw))
	}
	timeoutPath := "/harness/clients/" + receipt.Client.ClientID + "/timeout"
	c.do("PUT", timeoutPath, map[string]any{"timeoutSeconds": 1800}, c.operator, 403, nil)
	c.do("PUT", timeoutPath, map[string]any{"timeoutSeconds": 1801}, c.admin, 400, nil)
	c.do("PUT", timeoutPath, map[string]any{"timeoutSeconds": 1800}, c.admin, 204, nil)
	updated, e := c.api.Provider.AuthenticateHarness(context.Background(), receipt.Client.Token)
	if e != nil || updated.TimeoutSeconds != 1800 {
		t.Fatal("existing client timeout was not updated", e)
	}
	c.do("POST", "/harness/clients/"+receipt.Client.ClientID+"/revoke", nil, c.admin, http.StatusNoContent, nil)
	c.do("GET", "/harness/v1/models", nil, coding, 401, nil)
}
