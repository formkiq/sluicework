package api_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"sluicework/internal/personal"
)

func TestPersonalAgentEnrollmentNeverGrantsRemoteControl(t *testing.T) {
	c := setup(t)
	var ticket personal.Ticket
	c.do("POST", "/personal-agents/tickets", nil, c.operator, 200, &ticket)
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	enrollment := personal.Enrollment{TicketID: ticket.ID, Code: ticket.Code, Name: "Operator's private runtime", PublicKey: base64.RawURLEncoding.EncodeToString(public), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, personal.EnrollmentMessage(ticket.ID, public)))}
	var runtime personal.Runtime
	c.do("POST", "/personal-agents/enroll", enrollment, identity{}, 200, &runtime)
	c.do("POST", "/personal-agents/enroll", enrollment, identity{}, 403, nil)
	if runtime.Owner != "operator" {
		t.Fatal("enrollment owner was not taken from the authenticated ticket")
	}
	var list []personal.Runtime
	c.do("GET", "/personal-agents", nil, c.admin, 200, &list)
	if len(list) != 0 {
		t.Fatal("another admin could inspect private runtime details")
	}
	c.do("GET", "/personal-agents", nil, c.operator, 200, &list)
	if len(list) != 1 {
		t.Fatal("owner cannot inspect runtime")
	}
	manifest := personal.Manifest{Name: "Morning report", Prompt: "Approved fixture", Model: "fixture-model", Cron: "0 6 * * *", Timezone: "UTC", Expires: time.Now().Add(24 * time.Hour), MaxRunsPerDay: 1, TimeoutSeconds: 30, OutputLimit: 1024}
	body := map[string]any{"runtimeId": runtime.ID, "manifest": manifest}
	c.do("POST", "/personal-tasks", body, c.admin, 403, nil)
	var proposal personal.Proposal
	c.do("POST", "/personal-tasks", body, c.operator, 200, &proposal)
	c.do("PUT", "/personal-tasks/"+proposal.ID, body, c.admin, 403, nil)
	var tasks []personal.Proposal
	c.do("GET", "/personal-tasks", nil, c.admin, 200, &tasks)
	if len(tasks) != 0 {
		t.Fatal("other users can read personal prompts")
	}
	body["owner"] = "admin"
	c.do("POST", "/personal-tasks", body, c.operator, 400, nil)
	payload, _ := json.Marshal(personal.Report{Paused: true, Models: []string{}, Approvals: []personal.ApprovalStatus{}, Runs: []personal.Run{}})
	nonce := make([]byte, 24)
	_, _ = rand.Read(nonce)
	e := personal.Envelope{RuntimeID: runtime.ID, Timestamp: time.Now().Unix(), Nonce: base64.RawURLEncoding.EncodeToString(nonce), Payload: payload}
	e.Sign(key)
	c.do("POST", "/personal-agents/sync", e, identity{}, 200, &tasks)
	if len(tasks) != 1 || tasks[0].ID != proposal.ID {
		t.Fatal("signed agent did not receive its own proposals")
	}
	c.do("POST", "/personal-agents/sync", e, identity{}, 403, nil)
	for _, operation := range []string{"pause", "resume", "approve", "stop", "sign-in", "execute", "config"} {
		c.do("POST", "/personal-agents/"+runtime.ID+"/"+operation, map[string]any{}, c.operator, 404, nil)
		c.do("POST", "/personal-agents/"+runtime.ID+"/"+operation, map[string]any{}, c.admin, 404, nil)
	}
}
