package schedule

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"sluicework/internal/state"
)

type Spec struct {
	Name         string            `json:"name"`
	JobID        string            `json:"jobId"`
	Cron         string            `json:"cron"`
	Timezone     string            `json:"timezone"`
	Inputs       map[string]string `json:"inputs"`
	Enabled      *bool             `json:"enabled"`
	AllowOverlap bool              `json:"allowOverlap"`
}

type Schedule struct {
	ID string `json:"id"`
	Spec
	NextRun *time.Time  `json:"nextRun"`
	Latest  *Occurrence `json:"latest"`
}

type Occurrence struct {
	ScheduledAt time.Time `json:"scheduledAt"`
	LocalSlot   string    `json:"localSlot"`
	Status      string    `json:"status"`
	RunID       string    `json:"runId,omitempty"`
	Details     string    `json:"details,omitempty"`
}

type Service struct {
	db   *sql.DB
	jobs *state.Store
	now  func() time.Time
}

func New(db *sql.DB, jobs *state.Store) *Service {
	return &Service{db: db, jobs: jobs, now: time.Now}
}

func (s *Service) Init() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("CREATE TABLE IF NOT EXISTS schedule_migrations (version INTEGER PRIMARY KEY)"); err != nil {
		return err
	}
	var version int
	if err = tx.QueryRow("SELECT coalesce(max(version), 0) FROM schedule_migrations").Scan(&version); err != nil {
		return err
	}
	if version == 1 {
		return tx.Commit()
	}
	if version != 0 {
		return fmt.Errorf("unsupported schedule schema version %d", version)
	}
	_, err = tx.Exec(`
		CREATE TABLE schedules (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, job_id TEXT NOT NULL REFERENCES jobs(id),
			cron TEXT NOT NULL, timezone TEXT NOT NULL, inputs TEXT NOT NULL,
			enabled INTEGER NOT NULL, allow_overlap INTEGER NOT NULL,
			deleted INTEGER NOT NULL DEFAULT 0, next_utc INTEGER
		);
		CREATE INDEX schedules_due ON schedules(next_utc) WHERE enabled=1 AND deleted=0;
		CREATE TABLE schedule_occurrences (
			schedule_id TEXT NOT NULL REFERENCES schedules(id), local_slot TEXT NOT NULL,
			at_utc INTEGER NOT NULL, status TEXT NOT NULL, run_id TEXT REFERENCES runs(id), details TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(schedule_id,local_slot)
		);
		CREATE INDEX occurrences_recent ON schedule_occurrences(schedule_id,at_utc DESC);
		INSERT INTO schedule_migrations(version) VALUES(1);`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) Save(actor, id string, spec Spec) (Schedule, error) {
	spec.Name = strings.TrimSpace(spec.Name)
	if spec.Name == "" || len(spec.Name) > 160 || spec.JobID == "" {
		return Schedule{}, fmt.Errorf("%w: provide a schedule name and job", state.ErrInvalid)
	}
	r, err := parseRule(spec.Cron, spec.Timezone)
	if err != nil {
		return Schedule{}, fmt.Errorf("%w: %v", state.ErrInvalid, err)
	}
	if spec.Enabled == nil {
		enabled := true
		spec.Enabled = &enabled
	}
	if spec.Inputs == nil {
		spec.Inputs = map[string]string{}
	}
	data, err := json.Marshal(spec.Inputs)
	if err != nil {
		return Schedule{}, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return Schedule{}, err
	}
	defer tx.Rollback()
	if err = s.jobs.ValidateInputsTx(tx, spec.JobID, spec.Inputs); err != nil {
		return Schedule{}, err
	}
	next := r.next(s.now())
	if next.IsZero() {
		return Schedule{}, fmt.Errorf("%w: cron has no upcoming occurrence", state.ErrInvalid)
	}
	var due any
	if *spec.Enabled {
		due = next.Unix()
	}
	action := "schedule.update"
	if id == "" {
		id = uuid.NewString()
		action = "schedule.create"
		_, err = tx.Exec(`INSERT INTO schedules (name,job_id,cron,timezone,inputs,enabled,allow_overlap,next_utc,id)
			VALUES (?,?,?,?,?,?,?,?,?)`, spec.Name, spec.JobID, spec.Cron, spec.Timezone, string(data), *spec.Enabled, spec.AllowOverlap, due, id)
	} else {
		result, e := tx.Exec(`UPDATE schedules SET name=?,job_id=?,cron=?,timezone=?,inputs=?,enabled=?,allow_overlap=?,next_utc=?
			WHERE id=? AND deleted=0`, spec.Name, spec.JobID, spec.Cron, spec.Timezone, string(data), *spec.Enabled, spec.AllowOverlap, due, id)
		err = changed(result, e)
	}
	if err != nil {
		return Schedule{}, err
	}
	if err = state.AuditTx(tx, actor, action, id); err != nil {
		return Schedule{}, err
	}
	saved, err := readSchedule(tx.QueryRow(selectSchedules+" WHERE s.id=?", id))
	if err != nil {
		return Schedule{}, err
	}
	return saved, tx.Commit()
}

func changed(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return state.ErrNotFound
	}
	return err
}

// Keep historical run and occurrence links valid when a schedule is removed.
func (s *Service) Delete(actor, id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = changed(tx.Exec("UPDATE schedules SET deleted=1,enabled=0,next_utc=NULL WHERE id=? AND deleted=0", id)); err != nil {
		return err
	}
	if err = state.AuditTx(tx, actor, "schedule.delete", id); err != nil {
		return err
	}
	return tx.Commit()
}

const selectSchedules = `SELECT s.id,s.name,s.job_id,s.cron,s.timezone,s.inputs,s.enabled,s.allow_overlap,s.next_utc,
	o.at_utc,o.local_slot,o.status,o.run_id,o.details FROM schedules s LEFT JOIN schedule_occurrences o ON o.rowid=(
	SELECT rowid FROM schedule_occurrences WHERE schedule_id=s.id ORDER BY at_utc DESC LIMIT 1)`

type scanner interface{ Scan(...any) error }

func readSchedule(row scanner) (Schedule, error) {
	var result Schedule
	var inputs string
	var enabled bool
	var due, at sql.NullInt64
	var slot, status, run, details sql.NullString
	err := row.Scan(&result.ID, &result.Name, &result.JobID, &result.Cron, &result.Timezone, &inputs, &enabled, &result.AllowOverlap, &due,
		&at, &slot, &status, &run, &details)
	if errors.Is(err, sql.ErrNoRows) {
		return Schedule{}, state.ErrNotFound
	}
	if err != nil {
		return Schedule{}, err
	}
	if err = json.Unmarshal([]byte(inputs), &result.Inputs); err != nil {
		return Schedule{}, err
	}
	result.Enabled = &enabled
	if due.Valid {
		v := time.Unix(due.Int64, 0).UTC()
		result.NextRun = &v
	}
	if at.Valid {
		result.Latest = &Occurrence{time.Unix(at.Int64, 0).UTC(), slot.String, status.String, run.String, details.String}
	}
	return result, nil
}

// A persisted cursor may point into historical catch-up work. Clients see the
// upcoming firing time while the scheduler materializes missed occurrences.
func (s *Service) upcoming(row scanner) (Schedule, error) {
	item, err := readSchedule(row)
	if err != nil || item.NextRun == nil {
		return item, err
	}
	now := s.now().UTC()
	if item.NextRun.After(now) {
		return item, nil
	}
	r, err := parseRule(item.Cron, item.Timezone)
	if err != nil {
		return Schedule{}, err
	}
	next := r.next(now)
	item.NextRun = &next
	return item, nil
}
func (s *Service) Get(id string) (Schedule, error) {
	return s.upcoming(s.db.QueryRow(selectSchedules+" WHERE s.id=? AND s.deleted=0", id))
}

func (s *Service) List() ([]Schedule, error) {
	rows, err := s.db.Query(selectSchedules + " WHERE s.deleted=0 ORDER BY s.next_utc IS NULL,s.next_utc,s.name,s.id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Schedule{}
	for rows.Next() {
		item, err := s.upcoming(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Service) Occurrences(id string) ([]Occurrence, error) {
	var found int
	err := s.db.QueryRow("SELECT 1 FROM schedules WHERE id=?", id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, state.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT at_utc,local_slot,status,coalesce(run_id,''),details FROM schedule_occurrences
		WHERE schedule_id=? ORDER BY at_utc DESC LIMIT 100`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Occurrence{}
	for rows.Next() {
		var o Occurrence
		var at int64
		if err = rows.Scan(&at, &o.LocalSlot, &o.Status, &o.RunID, &o.Details); err != nil {
			return nil, err
		}
		o.ScheduledAt = time.Unix(at, 0).UTC()
		items = append(items, o)
	}
	return items, rows.Err()
}
