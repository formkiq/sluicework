package worker

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"sluicework/internal/state"
)

func updateControl(dir string, edit func(*workstationControl)) (workstationControl, error) {
	lock, err := os.OpenFile(filepath.Join(dir, "control.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return workstationControl{}, err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return workstationControl{}, err
	}
	current, err := control(filepath.Join(dir, "control.json"))
	if err != nil {
		return current, err
	}
	before := current
	edit(&current)
	if before == current {
		return current, nil
	}
	return current, atomicJSON(filepath.Join(dir, "control.json"), current)
}

func applyControl(dir string, request state.WorkstationControlRequest, session time.Time) (workstationControl, error) {
	if request.ID == "" || len(request.ID) > 128 || (request.Operation != "enable" && request.Operation != "pause") || request.IssuedAt.IsZero() {
		return workstationControl{}, errors.New("invalid admin capacity request")
	}
	if request.Operation == "enable" && (request.EnabledUntil.Before(request.IssuedAt) || request.EnabledUntil.Sub(request.IssuedAt) > 24*time.Hour) {
		return workstationControl{}, errors.New("invalid admin capacity window")
	}
	return updateControl(dir, func(c *workstationControl) {
		if c.AppliedControl == request.ID {
			return
		}
		c.AppliedControl = request.ID
		// An explicit newer local pause wins. Old requests never opt a new session
		// in after a reboot, crash or controller restart.
		if !request.IssuedAt.After(session) || !request.IssuedAt.After(c.ChangedAt) {
			return
		}
		c.ChangedAt = request.IssuedAt
		c.EnabledUntil = time.Time{}
		if request.Operation == "enable" {
			c.EnabledUntil = request.EnabledUntil
		}
	})
}
