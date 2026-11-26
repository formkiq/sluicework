package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func scheduled(at time.Time) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"source":"aws.events","time":%q}`, at.Format(time.RFC3339)))
}

func TestClockRetriesCannotMultiplyPollingWithinRuntime(t *testing.T) {
	now := time.Date(2026, 11, 24, 22, 0, 40, 0, time.UTC)
	var gate clockGate
	tick := scheduled(now.Add(-40 * time.Second))
	if ok, e := gate.allow(tick, now); e != nil || !ok {
		t.Fatal(ok, e)
	}
	for _, event := range []json.RawMessage{tick, scheduled(now.Add(-time.Minute)), json.RawMessage(`{}`)} {
		if ok, e := gate.allow(event, now.Add(10*time.Second)); e != nil || ok {
			t.Fatal("duplicate/out-of-order minute polled", ok, e)
		}
	}
	if ok, e := gate.allow(scheduled(now.Add(20*time.Second)), now.Add(time.Minute)); e != nil || !ok {
		t.Fatal("fresh next minute did not resume", ok, e)
	}
}

func TestStaleEventsCannotReplayBacklogAfterColdStart(t *testing.T) {
	now := time.Date(2026, 11, 24, 22, 0, 40, 0, time.UTC)
	var gate clockGate
	if ok, e := gate.allow(scheduled(now.Add(-3*time.Minute)), now); e != nil || ok {
		t.Fatal(ok, e)
	}
	for _, event := range []json.RawMessage{json.RawMessage(`{"source":"aws.events"}`), scheduled(now.Add(time.Minute)), json.RawMessage(`{"time":"secret"}`)} {
		if ok, e := gate.allow(event, now); e == nil || ok {
			t.Fatal("invalid clock accepted", ok, e)
		}
	}
	if ok, e := gate.allow(json.RawMessage(`{}`), now); e != nil || !ok {
		t.Fatal("manual invocation lost", ok, e)
	}
}

func TestProviderBackoffSkipsClocksUntilResetThenResumes(t *testing.T) {
	now := time.Date(2026, 11, 24, 22, 0, 40, 0, time.UTC)
	gate := clockGate{notBefore: now.Add(20 * time.Minute)}
	for _, offset := range []time.Duration{0, time.Minute, 19 * time.Minute} {
		if ok, e := gate.allow(scheduled(now.Add(offset)), now.Add(offset)); e != nil || ok {
			t.Fatal("provider backoff ignored", ok, e)
		}
	}
	if ok, e := gate.allow(scheduled(gate.notBefore), gate.notBefore); e != nil || !ok {
		t.Fatal("clock did not resume after reset", ok, e)
	}
}

type limitedPoll struct{ until time.Time }

func (e limitedPoll) Error() string      { return "provider rate limit" }
func (e limitedPoll) RetryAt() time.Time { return e.until }
func TestHandlerStopsAutomaticErrorRetryAmplification(t *testing.T) {
	now := time.Date(2026, 11, 24, 22, 0, 40, 0, time.UTC)
	var gate clockGate
	calls := 0
	until := now.Add(20 * time.Minute)
	if e := gate.run(scheduled(now), now, func() error { calls++; return fmt.Errorf("queue: %w", limitedPoll{until}) }); e != nil {
		t.Fatal("quota pause returned a retryable Lambda error", e)
	}
	if e := gate.run(scheduled(now.Add(time.Minute)), now.Add(time.Minute), func() error { calls++; return nil }); e != nil || calls != 1 {
		t.Fatal("provider was polled during its backoff", calls, e)
	}
	if e := gate.run(scheduled(until), until, func() error { calls++; return nil }); e != nil || calls != 2 {
		t.Fatal("fresh clock did not resume after reset", calls, e)
	}
	boom := fmt.Errorf("unclassified provider failure")
	next := until.Add(time.Minute)
	if e := gate.run(scheduled(next), next, func() error { calls++; return boom }); e != boom {
		t.Fatal("unexpected error was hidden", e)
	}
	if e := gate.run(scheduled(next), next.Add(time.Second), func() error { calls++; return boom }); e != nil || calls != 3 {
		t.Fatal("same clock error was retried", calls, e)
	}
}
