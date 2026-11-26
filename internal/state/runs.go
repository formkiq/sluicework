package state

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const runSelect = `SELECT r.id,r.job_id,j.definition,r.inputs,r.trigger,r.schedule_id,r.created,r.status
 FROM runs r JOIN job_revisions j ON j.job_id=r.job_id AND j.revision=r.revision`
const attemptSelect = `SELECT id,number,status,COALESCE(machine_id,''),queued,started,ended,lease_until,automatic,cancel_requested,outcome_unknown,
 output,output_truncated,logs_truncated,error FROM attempts`

func scanRun(row scanner) (Run, error) {
	var r Run
	var rawJob, rawInputs string
	var created int64
	if err := row.Scan(&r.ID, &r.JobID, &rawJob, &rawInputs, &r.Trigger, &r.ScheduleID, &created, &r.State); err != nil {
		return r, missing(err)
	}
	r.CreatedAt = fromMillis(created)
	if err := json.Unmarshal([]byte(rawJob), &r.Job); err != nil {
		return r, err
	}
	err := json.Unmarshal([]byte(rawInputs), &r.Inputs)
	return r, err
}
func attempts(q queryer, id string) ([]Attempt, error) {
	rows, err := q.Query(attemptSelect+" WHERE run_id=? ORDER BY number", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attempt{}
	for rows.Next() {
		var a Attempt
		var queued int64
		var started, ended, lease sql.NullInt64
		if err := rows.Scan(&a.ID, &a.Number, &a.State, &a.MachineID, &queued, &started, &ended, &lease, &a.Automatic, &a.CancelRequested, &a.OutcomeUnknown, &a.Output, &a.OutputTruncated, &a.LogsTruncated, &a.Error); err != nil {
			return nil, err
		}
		a.QueuedAt = fromMillis(queued)
		a.StartedAt = optionalTime(started)
		a.EndedAt = optionalTime(ended)
		a.LeaseUntil = optionalTime(lease)
		out = append(out, a)
	}
	return out, rows.Err()
}
func optionalTime(t sql.NullInt64) *time.Time {
	if !t.Valid {
		return nil
	}
	v := fromMillis(t.Int64)
	return &v
}
func readRun(q queryer, id string) (Run, error) {
	r, err := scanRun(q.QueryRow(runSelect+" WHERE r.id=?", id))
	if err != nil {
		return Run{}, err
	}
	r.Attempts, err = attempts(q, id)
	return r, err
}
func (s *Store) Run(id string) (Run, error) {
	var r Run
	err := transact(s.DB, func(tx *sql.Tx) error { var err error; r, err = readRun(tx, id); return err })
	return r, err
}
func (s *Store) Runs(job, machine, status string) ([]Run, error) {
	return s.RunsBefore(job, machine, status, "")
}

// RunsBefore uses the opaque last run ID as a stable cursor. Inserts while an
// operator reviews history do not shift older runs into or out of a page.
func (s *Store) RunsBefore(job, machine, status, beforeID string) ([]Run, error) {
	clauses := []string{"1=1"}
	args := []any{}
	if job != "" {
		clauses = append(clauses, "r.job_id=?")
		args = append(args, job)
	}
	if machine != "" {
		clauses = append(clauses, "EXISTS (SELECT 1 FROM attempts a WHERE a.run_id=r.id AND a.machine_id=?)")
		args = append(args, machine)
	}
	if status != "" {
		clauses = append(clauses, "r.status=?")
		args = append(args, status)
	}
	out := []Run{}
	err := transact(s.DB, func(tx *sql.Tx) error {
		if beforeID != "" {
			var created int64
			if err := tx.QueryRow("SELECT created FROM runs WHERE id=?", beforeID).Scan(&created); err != nil {
				return missing(err)
			}
			clauses = append(clauses, "(r.created,r.id)<(?,?)")
			args = append(args, created, beforeID)
		}
		rows, err := tx.Query(runSelect+" WHERE "+strings.Join(clauses, " AND ")+" ORDER BY r.created DESC,r.id DESC LIMIT 100", args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRun(rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		for i := range out {
			out[i].Attempts, err = attempts(tx, out[i].ID)
			if err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

func (s *Store) Cancel(actor, id string) error {
	return transact(s.DB, func(tx *sql.Tx) error {
		var attempt, status string
		if err := tx.QueryRow("SELECT id,status FROM attempts WHERE run_id=? ORDER BY number DESC LIMIT 1", id).Scan(&attempt, &status); err != nil {
			return missing(err)
		}
		switch status {
		case "queued":
			if _, err := tx.Exec("UPDATE attempts SET status='cancelled',ended=? WHERE id=?", time.Now().UnixMilli(), attempt); err != nil {
				return err
			}
			if _, err := tx.Exec("UPDATE runs SET status='cancelled' WHERE id=?", id); err != nil {
				return err
			}
		case "running":
			if _, err := tx.Exec("UPDATE attempts SET cancel_requested=1 WHERE id=?", attempt); err != nil {
				return err
			}
		case "cancelled":
			return nil
		default:
			return ErrConflict
		}
		return AuditTx(tx, actor, "cancel run", id)
	})
}
func (s *Store) Retry(actor, id string) (Run, error) {
	var r Run
	err := transact(s.DB, func(tx *sql.Tx) error {
		var status, schedule string
		var n int
		if err := tx.QueryRow("SELECT r.status,r.schedule_id,MAX(a.number) FROM runs r JOIN attempts a ON a.run_id=r.id WHERE r.id=? GROUP BY r.id", id).Scan(&status, &schedule, &n); err != nil {
			return missing(err)
		}
		if status == "running" || status == "queued" {
			return ErrConflict
		}
		if schedule != "" {
			var busy bool
			if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM runs WHERE schedule_id=? AND status IN ('queued','running')) AND COALESCE((SELECT allow_overlap FROM schedules WHERE id=? AND deleted=0),0)=0`, schedule, schedule).Scan(&busy); err != nil {
				return err
			}
			if busy {
				return fmt.Errorf("%w: the schedule has an active run", ErrConflict)
			}
		}
		if _, err := tx.Exec("INSERT INTO attempts(id,run_id,number,status,queued) VALUES(?,?,?,'queued',?)", randomID(), id, n+1, time.Now().UnixMilli()); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE runs SET status='queued' WHERE id=?", id); err != nil {
			return err
		}
		if err := AuditTx(tx, actor, "retry run", id); err != nil {
			return err
		}
		var err error
		r, err = readRun(tx, id)
		return err
	})
	return r, err
}

func bounded(value string, size int) (string, bool) {
	if size <= 0 {
		return "", len(value) > 0
	}
	value = strings.ToValidUTF8(value, "�")
	if len(value) <= size {
		return value, false
	}
	for !utf8.RuneStart(value[size]) {
		size--
	}
	return value[:size], true
}
func (s *Store) AppendLog(worker, attempt, text string) error {
	return s.AppendLogChunk(worker, attempt, text, 0)
}

// Sequence numbers are optional for v1 clients. The worker increments after
// acknowledgment so retrying a dropped response cannot duplicate stored logs.
func (s *Store) AppendLogChunk(worker, attempt, text string, sequence int64) error {
	if sequence < 0 {
		return invalid("negative log sequence")
	}
	return transact(s.DB, func(tx *sql.Tx) error {
		var used int
		var truncated, discard bool
		var last int64
		if err := tx.QueryRow("SELECT a.log_bytes,a.logs_truncated,a.log_sequence,COALESCE(json_extract(j.definition,'$.discardOutput'),0)"+activeAttemptSQL, attempt, worker, time.Now().UnixMilli()).Scan(&used, &truncated, &last, &discard); err != nil {
			return leaseErr(err)
		}
		if sequence > 0 {
			if sequence <= last {
				return nil
			}
			if sequence != last+1 {
				return fmt.Errorf("%w: log sequence is out of order", ErrConflict)
			}
		} else {
			sequence = last
		}
		if truncated || discard || text == "" {
			if sequence == last {
				return nil
			}
			_, err := tx.Exec("UPDATE attempts SET log_sequence=? WHERE id=?", sequence, attempt)
			return err
		}
		text, cut := bounded(text, s.logLimit()-used)
		if _, err := tx.Exec("INSERT INTO logs(attempt_id,at,text,truncated) VALUES(?,?,?,?)", attempt, time.Now().UnixMilli(), text, cut); err != nil {
			return err
		}
		_, err := tx.Exec("UPDATE attempts SET log_bytes=log_bytes+?,logs_truncated=?,log_sequence=? WHERE id=?", len(text), cut, sequence, attempt)
		return err
	})
}
func (s *Store) Logs(run string, after int64) ([]Log, error) {
	if after < 0 {
		return nil, invalid("negative log cursor")
	}
	var exists int
	if err := s.DB.QueryRow("SELECT 1 FROM runs WHERE id=?", run).Scan(&exists); err != nil {
		return nil, missing(err)
	}
	rows, err := s.DB.Query(`SELECT l.id,l.attempt_id,l.at,l.text,l.truncated FROM logs l JOIN attempts a ON a.id=l.attempt_id
 WHERE a.run_id=? AND l.id>? ORDER BY l.id LIMIT 1000`, run, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Log{}
	for rows.Next() {
		var l Log
		var at int64
		if err := rows.Scan(&l.ID, &l.AttemptID, &at, &l.Text, &l.Truncated); err != nil {
			return nil, err
		}
		l.At = fromMillis(at)
		out = append(out, l)
	}
	return out, rows.Err()
}
