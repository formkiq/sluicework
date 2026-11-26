package main

import (
	"encoding/json"
	"errors"
	"log"
	"sluicework/internal/githubapp"
	"time"
)

// Clock events carry no jobs: the next fresh clock discovers durable requests.
// Ignore old/replayed ticks so asynchronous retries cannot multiply polling.
// The engine's durable reservations still provide execution idempotence.
type clockGate struct {
	latestMinute int64
	notBefore    time.Time
}

func (g *clockGate) run(payload json.RawMessage, now time.Time, tick func() error) error {
	allowed, err := g.allow(payload, now)
	if err != nil || !allowed {
		return err
	}
	err = tick()
	if until := githubapp.RetryAt(err); until.After(now) {
		g.notBefore = until
		log.Printf("GitHub API polling paused until %s", until.UTC().Format(time.RFC3339))
		return nil // the existing minute clock resumes after provider backoff
	}
	return err
}

func (g *clockGate) allow(payload json.RawMessage, now time.Time) (bool, error) {
	var event struct {
		Source string    `json:"source"`
		Time   time.Time `json:"time"`
	}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &event); err != nil {
			return false, errors.New("invalid dispatcher clock envelope")
		}
	}
	at := now // existing IAM-authorized manual invocations use the current minute
	if event.Source == "aws.events" {
		if event.Time.IsZero() || event.Time.After(now.Add(30*time.Second)) {
			return false, errors.New("invalid dispatcher clock timestamp")
		}
		if now.Sub(event.Time) > 2*time.Minute {
			return false, nil
		}
		at = event.Time
	}
	if now.Before(g.notBefore) {
		return false, nil
	}
	minute := at.Unix() / 60
	if minute <= g.latestMinute {
		return false, nil
	}
	g.latestMinute = minute
	return true, nil
}
