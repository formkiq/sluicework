package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

func (s *Store) EnrollToken(actor string) (string, error) {
	token := randomID()
	err := transact(s.DB, func(tx *sql.Tx) error {
		now := time.Now()
		if _, err := tx.Exec("DELETE FROM enrollment_tokens WHERE expires<=?", now.UnixMilli()); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO enrollment_tokens(token_hash,expires) VALUES(?,?)", hash(token), now.Add(10*time.Minute).UnixMilli()); err != nil {
			return err
		}
		return AuditTx(tx, actor, "issue enrollment token", "")
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

func validateInfo(i WorkerInfo) error {
	if i.Workstation != nil {
		if i.Workstation.ControlVersion < 0 || i.Workstation.ControlVersion > 1 || len(i.Workstation.AppliedControl) > 128 {
			return invalid("invalid workstation control report")
		}
		if i.Workstation.SettingsVersion != 1 {
			return invalid("unsupported workstation settings version")
		}
		if err := i.Workstation.Local.Validate(i.OS); err != nil {
			return err
		}
		if err := i.Workstation.Effective.Validate(i.OS); err != nil {
			return err
		}
	}
	if !validPlatform(i.OS, i.Arch) {
		return invalid("worker platform must be darwin or linux, and arm64 or amd64")
	}
	if strings.TrimSpace(i.Name) == "" || len(i.Name) > 128 || len(i.Version) > 128 || len(i.Roles) == 0 || !uniqueNames(i.Roles) || len(i.RunnerStatus) > 128 || len(i.RunnerReason) > 256 {
		return invalid("invalid worker name, version, roles, or runner status")
	}
	return nil
}
func (s *Store) Enroll(token string, info WorkerInfo) (Enrollment, error) {
	if err := validateInfo(info); err != nil {
		return Enrollment{}, err
	}
	e := Enrollment{ID: randomID(), Credential: randomID()}
	err := transact(s.DB, func(tx *sql.Tx) error {
		now := time.Now().UnixMilli()
		res, err := tx.Exec("DELETE FROM enrollment_tokens WHERE token_hash=? AND expires>?", hash(token), now)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrUnauthorized
		}
		if _, err = tx.Exec("INSERT INTO machines(id,credential_hash,info,heartbeat) VALUES(?,?,?,?)", e.ID, hash(e.Credential), encode(info), now); err != nil {
			return err
		}
		return AuditTx(tx, e.ID, "enroll worker", e.ID)
	})
	if err != nil {
		return Enrollment{}, err
	}
	return e, nil
}
func (s *Store) AuthenticateWorker(token string) (string, error) {
	var id string
	err := s.DB.QueryRow("SELECT id FROM machines WHERE credential_hash=? AND revoked=0", hash(token)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrUnauthorized
	}
	return id, err
}
func (s *Store) Heartbeat(id string, info WorkerInfo) error {
	if err := validateInfo(info); err != nil {
		return err
	}
	res, err := s.DB.Exec("UPDATE machines SET info=?,heartbeat=? WHERE id=? AND revoked=0", encode(info), time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return ErrUnauthorized
	}
	return err
}

const machineSelect = `SELECT m.id,m.info,m.labels,m.slots,m.role_limits,m.runner,m.heartbeat,m.draining,m.revoked,
 (SELECT count(*) FROM attempts a WHERE a.machine_id=m.id AND a.status='running') FROM machines m`

type scanner interface{ Scan(...any) error }

func (s *Store) scanMachine(row scanner) (Machine, error) {
	var m Machine
	var info, labels, limits, runner string
	var heartbeat int64
	if err := row.Scan(&m.ID, &info, &labels, &m.Slots, &limits, &runner, &heartbeat, &m.Draining, &m.Revoked, &m.Running); err != nil {
		return m, missing(err)
	}
	for _, field := range []struct {
		raw string
		to  any
	}{{info, &m.WorkerInfo}, {labels, &m.Labels}, {limits, &m.RoleLimits}, {runner, &m.Runner}} {
		if err := json.Unmarshal([]byte(field.raw), field.to); err != nil {
			return m, err
		}
	}
	m.LastHeartbeat = fromMillis(heartbeat)
	m.Online = !m.Revoked && m.LastHeartbeat.After(time.Now().Add(-s.offlineAfter()))
	return m, nil
}
func (s *Store) Machine(id string) (Machine, error) {
	return s.scanMachine(s.DB.QueryRow(machineSelect+" WHERE m.id=?", id))
}
func (s *Store) Machines() ([]Machine, error) {
	rows, err := s.DB.Query(machineSelect + " ORDER BY m.heartbeat DESC,m.id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	machines := []Machine{}
	for rows.Next() {
		m, err := s.scanMachine(rows)
		if err != nil {
			return nil, err
		}
		machines = append(machines, m)
	}
	return machines, rows.Err()
}

var scopePattern = regexp.MustCompile(`^[a-zA-Z0-9-]+(/[a-zA-Z0-9_.-]+)?$`)
var servicePattern = regexp.MustCompile(`^((system|gui/[0-9]+|user/[0-9]+)/)?actions\.runner\.[a-zA-Z0-9_.-]+$`)

func (s *Store) UpdateMachine(actor, id string, p MachinePatch) error {
	return transact(s.DB, func(tx *sql.Tx) error {
		m, err := s.scanMachine(tx.QueryRow(machineSelect+" WHERE m.id=?", id))
		if err != nil {
			return err
		}
		if m.Revoked {
			return ErrConflict
		}
		if p.Labels != nil {
			if !uniqueNames(*p.Labels) {
				return invalid("invalid or duplicate labels")
			}
			m.Labels = *p.Labels
		}
		if p.Slots != nil {
			if *p.Slots < 1 || *p.Slots > 256 {
				return invalid("slots must be between 1 and 256")
			}
			m.Slots = *p.Slots
		}
		if p.RoleLimits != nil {
			if len(*p.RoleLimits) > 64 {
				return invalid("too many role limits")
			}
			for role, n := range *p.RoleLimits {
				if !uniqueNames([]string{role}) || n < 0 || n > 256 {
					return invalid("invalid role limit")
				}
			}
			m.RoleLimits = *p.RoleLimits
		}
		if p.Draining != nil {
			m.Draining = *p.Draining
		}
		if p.Runner != nil {
			if p.PreserveRunnerSettings && m.Runner != nil {
				p.Runner.Settings = m.Runner.Settings
			}
			if p.Runner.Control != nil {
				return invalid("use the workstation availability endpoint to control capacity")
			}
			if m.Runner != nil && p.Runner.Workstation {
				p.Runner.Control = m.Runner.Control
			}
			if p.Runner.Name == "" {
				m.Runner = nil
			} else {
				if p.Runner.Settings != nil {
					unchanged := m.Runner != nil && m.Runner.Settings != nil && *m.Runner.Settings == *p.Runner.Settings
					if !p.Runner.Workstation || ((!unchanged) && (m.Workstation == nil || m.Workstation.SettingsVersion != 1)) {
						return invalid("upgrade the workstation controller before managing its settings in admin")
					}
					if err := p.Runner.Settings.Validate(m.OS); err != nil {
						return err
					}
				}
				if p.Runner.Workstation && (len(m.Roles) != 1 || m.Roles[0] != "ci") {
					return invalid("workstation runners require a separate worker with only the ci role")
				}
				if len(p.Runner.Name) > 128 || len(p.Runner.Scope) > 256 || !scopePattern.MatchString(p.Runner.Scope) || strings.HasSuffix(p.Runner.Scope, "/.") || strings.HasSuffix(p.Runner.Scope, "/..") || !uniqueNames(p.Runner.Labels) || len(p.Runner.Service) > 255 || (!p.Runner.Workstation && !servicePattern.MatchString(p.Runner.Service)) || (p.Runner.Workstation && (!strings.Contains(p.Runner.Scope, "/") || m.OS == "" || m.Arch == "")) {
					return invalid("invalid runner registration")
				}
				if err := validateRunnerPlatform(m.WorkerInfo, *p.Runner); err != nil {
					return err
				}
				m.Runner = p.Runner
			}
		}
		if _, err := tx.Exec("UPDATE machines SET labels=?,slots=?,role_limits=?,draining=?,runner=? WHERE id=?", encode(m.Labels), m.Slots, encode(m.RoleLimits), m.Draining, encode(m.Runner), id); err != nil {
			return err
		}
		return AuditTx(tx, actor, "update worker", id)
	})
}
func (s *Store) Revoke(actor, id string) error {
	return transact(s.DB, func(tx *sql.Tx) error {
		res, err := tx.Exec("UPDATE machines SET revoked=1,credential_hash=NULL WHERE id=?", id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return AuditTx(tx, actor, "revoke worker", id)
	})
}
