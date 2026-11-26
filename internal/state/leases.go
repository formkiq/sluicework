package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// All claimant data, capacity and candidate selection share the immediate write
// transaction that changes the attempt state. The queue is ordered by insertion
// time and rowid so same-millisecond submissions are FIFO too.
func (s *Store) Claim(worker string) (*Claim, error) {
	var claim *Claim
	err := transact(s.DB, func(tx *sql.Tx) error {
		now := time.Now()
		if err := reconcile(tx, now); err != nil {
			return err
		}
		m, err := s.scanMachine(tx.QueryRow(machineSelect+" WHERE m.id=?", worker))
		if err != nil {
			return err
		}
		if m.Revoked {
			return ErrUnauthorized
		}
		if !m.Online || m.Draining || m.Running >= m.Slots {
			return nil
		}
		roles := append([]string{}, m.Roles...)
		if len(m.RoleLimits) > 0 {
			rows, err := tx.Query(`SELECT json_extract(j.definition,'$.role'),count(*) FROM attempts a JOIN runs r ON a.run_id=r.id
    JOIN job_revisions j ON j.job_id=r.job_id AND j.revision=r.revision WHERE a.machine_id=? AND a.status='running' GROUP BY 1`, worker)
			if err != nil {
				return err
			}
			defer rows.Close()
			active := map[string]int{}
			for rows.Next() {
				var role string
				var n int
				if err := rows.Scan(&role, &n); err != nil {
					return err
				}
				active[role] = n
			}
			if err := rows.Err(); err != nil {
				return err
			}
			rows.Close()
			roles = roles[:0]
			for _, r := range m.Roles {
				if cap, ok := m.RoleLimits[r]; !ok || active[r] < cap {
					roles = append(roles, r)
				}
			}
		}
		var attempt string
		err = tx.QueryRow(`SELECT a.id FROM attempts a JOIN runs r ON a.run_id=r.id JOIN job_revisions j ON j.job_id=r.job_id AND j.revision=r.revision
    WHERE a.status='queued' AND json_extract(j.definition,'$.role') IN (SELECT value FROM json_each(?))
    AND NOT EXISTS(SELECT value FROM json_each(j.definition,'$.labels') EXCEPT SELECT value FROM json_each(?))
    AND (COALESCE(json_extract(j.definition,'$.os'),'')='' OR json_extract(j.definition,'$.os')=?)
    AND (COALESCE(json_extract(j.definition,'$.arch'),'')='' OR json_extract(j.definition,'$.arch')=?)
    ORDER BY a.queued,a.rowid LIMIT 1`, encode(roles), encode(m.Labels), m.OS, m.Arch).Scan(&attempt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		until := now.Add(s.leaseTTL())
		if _, err := tx.Exec("UPDATE attempts SET machine_id=?,status='running',started=?,lease_until=? WHERE id=? AND status='queued'", worker, now.UnixMilli(), until.UnixMilli(), attempt); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE runs SET status='running' WHERE id=(SELECT run_id FROM attempts WHERE id=?)", attempt); err != nil {
			return err
		}
		claim, err = activeClaim(tx, worker, attempt, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	return claim, nil
}

const activeAttemptSQL = ` FROM attempts a JOIN machines m ON m.id=a.machine_id
 JOIN runs r ON r.id=a.run_id JOIN job_revisions j ON j.job_id=r.job_id AND j.revision=r.revision
 WHERE a.id=? AND a.machine_id=? AND a.lease_until>? AND a.status='running' AND m.revoked=0`

func leaseErr(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLease
	}
	return err
}
func activeClaim(q queryer, worker, attempt string, now time.Time) (*Claim, error) {
	var c Claim
	var job, inputs string
	var lease int64
	if err := q.QueryRow("SELECT r.id,a.id,j.definition,r.inputs,a.lease_until"+activeAttemptSQL+" AND a.cancel_requested=0", attempt, worker, now.UnixMilli()).Scan(&c.RunID, &c.AttemptID, &job, &inputs, &lease); err != nil {
		return nil, leaseErr(err)
	}
	c.LeaseUntil = fromMillis(lease)
	if err := json.Unmarshal([]byte(job), &c.Job); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(inputs), &c.Inputs); err != nil {
		return nil, err
	}
	return &c, nil
}
func (s *Store) ActiveClaim(worker, attempt string) (*Claim, error) {
	return activeClaim(s.DB, worker, attempt, time.Now())
}
func (s *Store) Renew(worker, attempt string) (Renewal, error) {
	var out Renewal
	err := transact(s.DB, func(tx *sql.Tx) error {
		now := time.Now()
		var deadline int64
		if err := tx.QueryRow("SELECT a.cancel_requested,a.lease_until"+activeAttemptSQL, attempt, worker, now.UnixMilli()).Scan(&out.CancelRequested, &deadline); err != nil {
			return leaseErr(err)
		}
		// Preserve a fixed deadline for cancellation. A worker still sending
		// heartbeats cannot hold an unconfirmed process open indefinitely.
		if out.CancelRequested {
			out.LeaseUntil = fromMillis(deadline)
			return nil
		}
		out.LeaseUntil = fromMillis(now.Add(s.leaseTTL()).UnixMilli())
		_, err := tx.Exec("UPDATE attempts SET lease_until=? WHERE id=?", out.LeaseUntil.UnixMilli(), attempt)
		return err
	})
	return out, err
}
func (s *Store) Complete(worker, attempt string, result Completion) error {
	switch result.State {
	case "succeeded", "failed", "timed_out", "cancelled", "interrupted":
	default:
		return invalid("invalid completion state")
	}
	return transact(s.DB, func(tx *sql.Tx) error {
		now := time.Now().UnixMilli()
		var run, raw string
		var cancel bool
		if err := tx.QueryRow("SELECT r.id,j.definition,a.cancel_requested"+activeAttemptSQL, attempt, worker, now).Scan(&run, &raw, &cancel); err != nil {
			return leaseErr(err)
		}
		if result.State == "cancelled" && !cancel {
			return fmt.Errorf("%w: cancellation was not requested", ErrConflict)
		}
		if cancel && result.State != "cancelled" && result.State != "interrupted" {
			return fmt.Errorf("%w: cancellation confirmation required", ErrConflict)
		}
		var j Job
		if err := json.Unmarshal([]byte(raw), &j); err != nil {
			return err
		}
		if result.State == "succeeded" && j.Type == "ai" && j.OutputFormat == "json" && !j.DiscardOutput {
			if len(result.Output) > j.OutputLimit || result.OutputTruncated || !json.Valid([]byte(result.Output)) {
				return invalid("JSON results must be complete, valid JSON within the approved byte limit")
			}
		}
		output, cut := bounded(result.Output, j.OutputLimit)
		cut = cut || result.OutputTruncated
		if j.DiscardOutput {
			output = ""
			cut = false
		}
		msg, _ := bounded(result.Error, 2048)
		if _, err := tx.Exec("UPDATE attempts SET status=?,ended=?,lease_until=NULL,outcome_unknown=?,output=?,output_truncated=?,error=? WHERE id=?", result.State, now, result.State == "interrupted", output, cut, msg, attempt); err != nil {
			return err
		}
		return finishRun(tx, run, result.State, j, cancel, now)
	})
}

// A run's approved snapshot controls retries throughout its lifetime. A later edit
// to the template cannot make an earlier run repeatable. Manual retries do not
// consume or reset its automatic retry budget.
func finishRun(tx *sql.Tx, run, status string, j Job, cancelRequested bool, now int64) error {
	retryable := status == "failed" || status == "timed_out" || status == "interrupted"
	if retryable && !cancelRequested && j.SafeToRepeat && j.MaxAutoRetries > 0 {
		var used, latest int
		if err := tx.QueryRow("SELECT SUM(automatic),MAX(number) FROM attempts WHERE run_id=?", run).Scan(&used, &latest); err != nil {
			return err
		}
		if used < j.MaxAutoRetries {
			if _, err := tx.Exec("INSERT INTO attempts(id,run_id,number,status,queued,automatic) VALUES(?,?,?,'queued',?,1)", randomID(), run, latest+1, now); err != nil {
				return err
			}
			if err := AuditTx(tx, "orchestrator", "automatically retry run", run); err != nil {
				return err
			}
			status = "queued"
		}
	}
	_, err := tx.Exec("UPDATE runs SET status=? WHERE id=?", status, run)
	return err
}

func reconcile(tx *sql.Tx, now time.Time) error {
	rows, err := tx.Query(`SELECT a.id,a.run_id,a.cancel_requested,j.definition FROM attempts a
 JOIN runs r ON r.id=a.run_id JOIN job_revisions j ON j.job_id=r.job_id AND j.revision=r.revision
 WHERE a.status='running' AND a.lease_until<=?`, now.UnixMilli())
	if err != nil {
		return err
	}
	defer rows.Close()
	type expired struct {
		id, run, definition string
		cancel              bool
	}
	var due []expired
	for rows.Next() {
		var a expired
		if err := rows.Scan(&a.id, &a.run, &a.cancel, &a.definition); err != nil {
			return err
		}
		due = append(due, a)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, a := range due {
		var j Job
		if err := json.Unmarshal([]byte(a.definition), &j); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE attempts SET status='interrupted',ended=?,lease_until=NULL,outcome_unknown=1,
  error='Attempt lease expired; process outcome unknown' WHERE id=?`, now.UnixMilli(), a.id); err != nil {
			return err
		}
		if err := finishRun(tx, a.run, "interrupted", j, a.cancel, now.UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}

// InterruptRestoredAttempts fences workers from the original timeline. Restoring
// a historical snapshot requires human review before repeating in-flight work.
func InterruptRestoredAttempts(tx *sql.Tx) error {
	now := time.Now().UnixMilli()
	if _, err := tx.Exec(`UPDATE runs SET status=CASE status WHEN 'running' THEN 'interrupted' ELSE 'cancelled' END
 WHERE status IN('running','queued')`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE attempts SET status='interrupted',ended=?,lease_until=NULL,outcome_unknown=1,
 error='Backup restored; process outcome unknown' WHERE status='running'`, now); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE attempts SET status='cancelled',ended=?,error='Queue restored from backup; review before retrying'
 WHERE status='queued'`, now)
	return err
}
func (s *Store) Reconcile() error {
	return transact(s.DB, func(tx *sql.Tx) error { return reconcile(tx, time.Now()) })
}
