package state

import (
	"database/sql"
	"encoding/json"
	"math"
	"time"
)

// WorkstationSettings are centrally managed operating preferences. Host paths,
// connection trust and worker credentials stay with the installation.
type WorkstationSettings struct {
	MaxLoadPerCPU        float64 `json:"maxLoadPerCpu"`
	MinAvailableMemoryMB int     `json:"minAvailableMemoryMb"`
	MinIdleSeconds       int     `json:"minIdleSeconds"`
	RequireACPower       bool    `json:"requireAcPower"`
	BuildThreads         int     `json:"buildThreads"`
	EnableMinutes        int     `json:"enableMinutes"`
}

type WorkstationStatus struct {
	ControlVersion  int                 `json:"controlVersion,omitempty"`
	AppliedControl  string              `json:"appliedControl,omitempty"`
	SettingsVersion int                 `json:"settingsVersion"`
	Local           WorkstationSettings `json:"local"`
	Effective       WorkstationSettings `json:"effective"`
	EnabledUntil    string              `json:"enabledUntil,omitempty"`
}

type WorkstationControlRequest struct {
	ID           string    `json:"id"`
	Operation    string    `json:"operation"`
	IssuedAt     time.Time `json:"issuedAt"`
	EnabledUntil time.Time `json:"enabledUntil"`
}

func (s *Store) SetWorkstationAvailability(actor, id, operation string, minutes int) error {
	if operation != "enable" && operation != "pause" {
		return invalid("choose enable or pause")
	}
	return transact(s.DB, func(tx *sql.Tx) error {
		m, err := s.scanMachine(tx.QueryRow(machineSelect+" WHERE m.id=?", id))
		if err != nil {
			return err
		}
		if m.Revoked || len(m.Roles) != 1 || m.Roles[0] != "ci" || m.Runner == nil || !m.Runner.Workstation || m.Workstation == nil || m.Workstation.ControlVersion != 1 {
			return invalid("upgrade and approve the workstation controller before controlling capacity in admin")
		}
		if operation == "enable" && m.Draining {
			return ErrConflict
		}
		if minutes == 0 {
			minutes = m.Workstation.Local.EnableMinutes
			if m.Runner.Settings != nil {
				minutes = m.Runner.Settings.EnableMinutes
			}
		}
		if minutes < 1 || minutes > 1440 {
			return invalid("availability must be 1–1440 minutes")
		}
		now := time.Now().UTC()
		request := &WorkstationControlRequest{ID: randomID(), Operation: operation, IssuedAt: now}
		if operation == "enable" {
			request.EnabledUntil = now.Add(time.Duration(minutes) * time.Minute)
		}
		m.Runner.Control = request
		raw, err := json.Marshal(m.Runner)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE machines SET runner=? WHERE id=?", string(raw), id); err != nil {
			return err
		}
		return AuditTx(tx, actor, "request workstation "+operation, id)
	})
}

func (w WorkstationSettings) Validate(os string) error {
	if math.IsNaN(w.MaxLoadPerCPU) || math.IsInf(w.MaxLoadPerCPU, 0) || w.MaxLoadPerCPU <= 0 || w.MaxLoadPerCPU > 1 || w.MinAvailableMemoryMB < 512 || w.MinAvailableMemoryMB > 1048576 || w.BuildThreads < 1 || w.BuildThreads > 8 || w.EnableMinutes < 1 || w.EnableMinutes > 1440 || w.MinIdleSeconds < 0 || w.MinIdleSeconds > 86400 {
		return invalid("workstation settings require 1–8 build threads, 512–1048576 MiB memory, CPU load fraction >0 and <=1, 0–86400 idle seconds and 1–1440 enabled minutes")
	}
	if os == "linux" && w.MinIdleSeconds != 0 {
		return invalid("Linux workstation settings cannot require input idle time")
	}
	return nil
}
