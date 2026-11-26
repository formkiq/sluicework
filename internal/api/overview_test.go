package api

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"sluicework/internal/provider"
	"sluicework/internal/state"
)

func TestOverviewIncludesWorkBeyondRunHistoryPage(t *testing.T) {
	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "cluster.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := &state.Store{DB: db}
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p, err := provider.Open(db, filepath.Join(dir, "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := p.Create(ctx, provider.Config{Name: "model", Kind: "ollama", Endpoint: "http://localhost:11434", Models: []string{"test"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE provider_connections SET health='error' WHERE id=?", c.ID); err != nil {
		t.Fatal(err)
	}
	j, err := st.SaveJob("test", "", state.Job{Name: "diagnostics", Type: "script", Command: "/usr/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 19, 12, 17, 0, 0, time.UTC)
	start := now.Add(-24 * time.Hour)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var lastFailureID string
	// Five failures span the previous-day boundary. A long-running old job ended today.
	for i := 0; i < 120; i++ {
		r, err := st.EnqueueTx(tx, "test", j.ID, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		status := "queued"
		created := now.Add(-time.Hour)
		var ended any
		if i < 5 {
			status = "failed"
			created = start.Add(-4 * time.Hour)
			finished := start.Add(time.Duration(i-1) * time.Hour)
			ended = finished.UnixMilli()
			if i == 4 {
				lastFailureID = r.ID
				created = start.Add(2 * time.Hour)
			}
		} else if i == 5 {
			status = "running"
		}
		if _, err := tx.Exec("UPDATE runs SET created=?,status=? WHERE id=?", created.UnixMilli(), status, r.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("UPDATE attempts SET status=?,ended=? WHERE run_id=?", status, ended, r.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	result, err := loadOverview(ctx, db, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Queued != 114 || result.Running != 1 || result.Failed24h != 4 || result.UnhealthyConnections != 1 {
		t.Fatalf("incorrect metrics: %+v", result)
	}
	if len(result.Hourly) != 24 || !result.Hourly[0].Start.Equal(start) || result.Hourly[2].Failures != 1 || result.Hourly[23].Runs != 115 {
		t.Fatalf("incorrect activity: %+v", result.Hourly)
	}
	if len(result.RecentFailures) != 3 || result.RecentFailures[0].ID != lastFailureID || result.RecentFailures[0].JobName != j.Name {
		t.Fatalf("incorrect failures: %+v", result.RecentFailures)
	}
}
