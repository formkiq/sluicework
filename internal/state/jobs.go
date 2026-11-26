package state

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var inputName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
var placeholder = regexp.MustCompile(`\{\{([A-Za-z_][A-Za-z0-9_]*)\}\}`)

func validateJob(j *Job) error {
	if !validPlatform(j.OS, j.Arch) {
		return invalid("job platform must be darwin or linux, and arm64 or amd64")
	}
	if j.Role == "" {
		if j.Type == "ai" {
			j.Role = "agent"
		} else {
			j.Role = "script"
		}
	}
	if j.TimeoutSeconds == 0 {
		j.TimeoutSeconds = 300
	}
	if j.OutputLimit == 0 {
		j.OutputLimit = 65536
	}
	if j.OutputFormat == "" {
		j.OutputFormat = "text"
	}
	if j.Labels == nil {
		j.Labels = []string{}
	}
	if j.Inputs == nil {
		j.Inputs = []Input{}
	}
	if strings.TrimSpace(j.Name) == "" || len(j.Name) > 160 || !uniqueNames([]string{j.Role}) || !uniqueNames(j.Labels) {
		return invalid("invalid name, role, or placement labels")
	}
	if j.OutputLimit < 1 || j.OutputLimit > 4<<20 || j.TimeoutSeconds < 1 || j.TimeoutSeconds > 86400 {
		return invalid("timeout must be 1–86400 seconds and output limit 1–4194304 bytes")
	}
	if j.MaxAutoRetries < 0 || j.MaxAutoRetries > 3 || (j.MaxAutoRetries > 0 && !j.SafeToRepeat) {
		return invalid("automatic retries require a safe-to-repeat template and a limit of at most 3")
	}
	if j.OutputFormat != "json" && j.OutputFormat != "text" {
		return invalid("output format must be text or json")
	}
	if len(j.Inputs) > 64 {
		return invalid("too many declared inputs")
	}
	keys := map[string]bool{}
	for _, in := range j.Inputs {
		if !inputName.MatchString(in.Name) || keys[in.Name] {
			return invalid("invalid or duplicate input name")
		}
		keys[in.Name] = true
		if len(in.Paths) > 128 {
			return invalid("too many input paths")
		}
		for _, path := range in.Paths {
			if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) || len(path) > 4096 {
				return invalid("input paths must be clean, absolute paths")
			}
		}
	}
	var templates []string
	switch j.Type {
	case "script":
		if !filepath.IsAbs(j.Command) || strings.ContainsRune(j.Command, 0) || len(j.Command) > 4096 {
			return invalid("script command must be an absolute executable path")
		}
		if len(j.WorkDir) > 4096 || strings.ContainsRune(j.WorkDir, 0) || (j.WorkDir != "" && !filepath.IsLocal(j.WorkDir)) {
			return invalid("working directory must be inside the attempt workspace")
		}
		if len(j.Args) > 128 {
			return invalid("too many arguments")
		}
		templates = j.Args
	case "ai":
		if j.MaxTokens == 0 {
			j.MaxTokens = 1024
		}
		if j.MaxTokens < 1 || j.MaxTokens > 131072 || strings.TrimSpace(j.Connection) == "" || len(j.Connection) > 256 || strings.TrimSpace(j.Model) == "" || len(j.Model) > 256 || strings.TrimSpace(j.Prompt) == "" {
			return invalid("AI tasks require a prompt, connection, model and valid token cap")
		}
		templates = []string{j.Prompt}
	default:
		return invalid("type must be script or ai")
	}
	total := 0
	for _, field := range templates {
		total += len(field)
		if total > 128<<10 || strings.ContainsRune(field, 0) {
			return invalid("invalid command arguments or prompt")
		}
		for _, match := range placeholder.FindAllStringSubmatch(field, -1) {
			if !keys[match[1]] {
				return invalid("undeclared input " + match[1])
			}
		}
	}
	return nil
}

func validateInputs(j Job, values map[string]string) error {
	known := make(map[string]bool, len(j.Inputs))
	for _, decl := range j.Inputs {
		known[decl.Name] = true
		v, exists := values[decl.Name]
		if !exists {
			if decl.Required {
				return invalid("missing input " + decl.Name)
			}
			continue
		}
		if len(v) > 128<<10 || strings.ContainsRune(v, 0) {
			return invalid("invalid input " + decl.Name)
		}
		if len(decl.Paths) > 0 && !contains(decl.Paths, v) {
			return invalid("path is not approved for " + decl.Name)
		}
	}
	total := 0
	for key, value := range values {
		if !known[key] {
			return invalid("undeclared input " + key)
		}
		total += len(value)
	}
	if total > 512<<10 {
		return invalid("inputs exceed 512 KiB")
	}
	return nil
}
func contains(vals []string, v string) bool {
	for _, x := range vals {
		if x == v {
			return true
		}
	}
	return false
}
func job(q queryer, id string) (Job, error) {
	var j Job
	var raw string
	if err := q.QueryRow("SELECT jr.definition FROM jobs j JOIN job_revisions jr ON jr.job_id=j.id AND jr.revision=j.revision WHERE j.id=?", id).Scan(&raw); err != nil {
		return j, missing(err)
	}
	err := json.Unmarshal([]byte(raw), &j)
	return j, err
}
func (s *Store) Job(id string) (Job, error) { return job(s.DB, id) }
func (s *Store) Jobs() ([]Job, error) {
	rows, err := s.DB.Query("SELECT jr.definition FROM jobs j JOIN job_revisions jr ON jr.job_id=j.id AND jr.revision=j.revision ORDER BY j.rowid DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		var raw string
		var j Job
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &j); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
func (s *Store) SaveJob(actor, id string, j Job) (Job, error) {
	var saved Job
	err := transact(s.DB, func(tx *sql.Tx) error { var err error; saved, err = SaveJobTx(tx, actor, id, j); return err })
	return saved, err
}
func SaveJobTx(tx *sql.Tx, actor, id string, j Job) (Job, error) {
	if err := validateJob(&j); err != nil {
		return Job{}, err
	}
	if id == "" {
		j.ID = randomID()
		j.Revision = 1
		if _, err := tx.Exec("INSERT INTO jobs(id,revision) VALUES(?,?)", j.ID, j.Revision); err != nil {
			return Job{}, err
		}
	} else {
		var rev int
		if err := tx.QueryRow("SELECT revision FROM jobs WHERE id=?", id).Scan(&rev); err != nil {
			return Job{}, missing(err)
		}
		if j.Revision != 0 && j.Revision != rev {
			return Job{}, fmt.Errorf("%w: job revision changed", ErrConflict)
		}
		j.ID = id
		j.Revision = rev + 1
		if _, err := tx.Exec("UPDATE jobs SET revision=? WHERE id=?", j.Revision, id); err != nil {
			return Job{}, err
		}
	}
	if _, err := tx.Exec("INSERT INTO job_revisions(job_id,revision,definition) VALUES(?,?,?)", j.ID, j.Revision, encode(j)); err != nil {
		return Job{}, err
	}
	return j, AuditTx(tx, actor, "approve job revision", j.ID)
}
func (s *Store) ValidateInputsTx(tx *sql.Tx, jobID string, inputs map[string]string) error {
	j, err := job(tx, jobID)
	if err != nil {
		return err
	}
	return validateInputs(j, inputs)
}
func (s *Store) Queue(actor, jobID string, inputs map[string]string) (Run, error) {
	var r Run
	err := transact(s.DB, func(tx *sql.Tx) error { var err error; r, err = s.EnqueueTx(tx, actor, jobID, "", inputs); return err })
	return r, err
}
func (s *Store) EnqueueTx(tx *sql.Tx, actor, jobID, scheduleID string, inputs map[string]string) (Run, error) {
	j, err := job(tx, jobID)
	if err != nil {
		return Run{}, err
	}
	if err := validateInputs(j, inputs); err != nil {
		return Run{}, err
	}
	if inputs == nil {
		inputs = map[string]string{}
	}
	now := fromMillis(time.Now().UnixMilli())
	a := Attempt{ID: randomID(), Number: 1, State: "queued", QueuedAt: now}
	r := Run{ID: randomID(), JobID: jobID, Job: j, Inputs: inputs, Trigger: actor, ScheduleID: scheduleID, CreatedAt: now, State: "queued", Attempts: []Attempt{a}}
	if _, err := tx.Exec("INSERT INTO runs(id,job_id,revision,inputs,trigger,schedule_id,created,status) VALUES(?,?,?,?,?,?,?,?)", r.ID, jobID, j.Revision, encode(inputs), actor, scheduleID, now.UnixMilli(), r.State); err != nil {
		return Run{}, err
	}
	if _, err := tx.Exec("INSERT INTO attempts(id,run_id,number,status,queued) VALUES(?,?,?,?,?)", a.ID, r.ID, a.Number, a.State, now.UnixMilli()); err != nil {
		return Run{}, err
	}
	return r, AuditTx(tx, actor, "queue run", r.ID)
}
