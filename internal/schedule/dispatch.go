package schedule

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"sluicework/internal/state"
)

// Start reconciles overdue slots and checks for work until the context is cancelled.
func (s *Service) Start(ctx context.Context) error {
	boot := s.now().UTC()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := s.tick(ctx, s.now().UTC(), boot); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (s *Service) tick(ctx context.Context, now, boot time.Time) error {
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM schedules WHERE enabled=1 AND deleted=0 AND next_utc<=? ORDER BY next_utc,id", now.Unix())
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		if err = s.advance(ctx, id, now, boot); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) advance(ctx context.Context, id string, now, boot time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Read again under the write transaction. An edit, cancellation or a second
	// scheduler may have advanced the deadline since the initial scan.
	sc, err := readSchedule(tx.QueryRow(selectSchedules+" WHERE s.id=? AND s.enabled=1 AND s.deleted=0", id))
	if errors.Is(err, state.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if sc.NextRun == nil || sc.NextRun.After(now) {
		return nil
	}
	r, err := parseRule(sc.Cron, sc.Timezone)
	if err != nil {
		return err
	}

	cutoff := now.Truncate(time.Minute)
	if boot.After(cutoff) {
		cutoff = boot
	}
	due := *sc.NextRun
	// After a long outage, an eligible live slot takes precedence over filling
	// history. The unique local key prevents a second dispatch when the durable
	// cursor later reaches this slot, even across a process restart.
	currentMinute := now.UTC().Truncate(time.Minute)
	if due.Before(currentMinute) && !currentMinute.Before(boot) {
		current := r.next(currentMinute.Add(-time.Minute))
		if current.Equal(currentMinute) {
			if err = s.record(tx, sc, r, current, false); err != nil {
				return err
			}
		}
	}
	// Bound each writer transaction during recovery. The next tick picks up
	// the durable cursor, so other schedules and API requests can make progress.
	for count := 0; count < 256 && !due.After(now); count++ {
		if err = s.record(tx, sc, r, due, due.Before(cutoff)); err != nil {
			return err
		}
		due = r.next(due)
		if due.IsZero() {
			return errors.New("stored schedule has no next occurrence")
		}
	}
	if _, err = tx.Exec("UPDATE schedules SET next_utc=? WHERE id=?", due.Unix(), id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) record(tx *sql.Tx, sc Schedule, r rule, due time.Time, late bool) error {
	slot := due.In(r.location).Format("2006-01-02T15:04")
	status, detail := "queued", ""
	if late {
		status, detail = "missed", "The scheduler was offline at this time."
	}
	res, err := tx.Exec(`INSERT INTO schedule_occurrences(schedule_id,local_slot,at_utc,status,details)
		VALUES(?,?,?,?,?) ON CONFLICT(schedule_id,local_slot) DO NOTHING`, sc.ID, slot, due.Unix(), status, detail)
	if err != nil {
		return err
	}
	count, err := res.RowsAffected()
	if err != nil || count == 0 || late {
		return err
	}
	if !sc.AllowOverlap {
		var active bool
		err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM runs WHERE schedule_id=? AND status IN ('queued','running'))", sc.ID).Scan(&active)
		if err != nil {
			return err
		}
		if active {
			status, detail = "skipped_overlap", "The previous run was still queued or running."
		}
	}
	var runID any
	if status == "queued" {
		// Revisions may change after schedule creation. Check before writing a run.
		err = s.jobs.ValidateInputsTx(tx, sc.JobID, sc.Inputs)
		switch {
		case errors.Is(err, state.ErrInvalid), errors.Is(err, state.ErrNotFound):
			status, detail = "missed", "The current job revision requires different inputs or is unavailable."
		case err != nil:
			return err
		default:
			run, e := s.jobs.EnqueueTx(tx, "scheduler", sc.JobID, sc.ID, sc.Inputs)
			if e != nil {
				return e
			}
			runID = run.ID
		}
	}
	_, err = tx.Exec("UPDATE schedule_occurrences SET status=?,run_id=?,details=? WHERE schedule_id=? AND local_slot=?", status, runID, detail, sc.ID, slot)
	return err
}
