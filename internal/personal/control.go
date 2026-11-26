package personal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Control struct {
	Operation string `json:"operation"`
	ID        string `json:"id,omitempty"`
	Digest    string `json:"digest,omitempty"`
	Consent   bool   `json:"consent,omitempty"`
}
type Reply struct {
	Value json.RawMessage `json:"value,omitempty"`
	Error string          `json:"error,omitempty"`
}

func (a *Agent) control(ctx context.Context, r Control) (any, error) {
	switch r.Operation {
	case "status":
		report, err := a.Report()
		if err != nil {
			return nil, err
		}
		a.mu.Lock()
		message := a.lastSyncError
		a.mu.Unlock()
		return map[string]any{"owner": a.Config.Owner, "server": a.Config.Server, "report": report, "syncError": message}, nil
	case "results":
		return a.Results(20, false)
	case "preview":
		return a.Preview(r.ID)
	case "approve":
		return a.Approve(r.ID, r.Digest)
	case "pause":
		return nil, a.Pause(true)
	case "resume":
		return nil, a.Pause(false)
	case "revoke":
		return nil, a.Revoke(r.ID)
	case "models":
		return a.RefreshModels(ctx)
	case "login":
		return a.Login(ctx, r.Consent)
	case "login-status":
		return a.LoginStatus(ctx, r.ID)
	case "sign-out":
		return a.SignOut(ctx)
	case "stop":
		if err := a.Pause(true); err != nil {
			return nil, err
		}
		a.mu.Lock()
		if a.cancel != nil {
			a.cancel()
		}
		a.mu.Unlock()
		return nil, nil
	default:
		return nil, ErrInvalid
	}
}
func (a *Agent) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.mu.Lock()
	a.cancel = cancel
	a.mu.Unlock()
	path := filepath.Join(a.vault.Dir, "control.sock")
	if len(path) > 100 {
		return errors.New("choose a shorter private runtime path with --dir; OS Unix socket paths are limited")
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("refusing to replace a non-socket control path")
		}
		if err = os.Remove(path); err != nil {
			return err
		}
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return err
	}
	if err = os.Chmod(path, 0600); err != nil {
		listener.Close()
		return err
	}
	defer listener.Close()
	go func() { <-ctx.Done(); listener.Close() }()
	var controls sync.WaitGroup
	var background sync.WaitGroup
	defer func() { cancel(); controls.Wait(); background.Wait() }()
	syncOnce := func() {
		err := a.Sync(ctx)
		a.mu.Lock()
		a.lastSyncError = ""
		if err != nil {
			a.lastSyncError = err.Error()
		}
		a.mu.Unlock()
	}
	background.Add(2)
	go func() {
		defer background.Done()
		syncOnce()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				syncOnce()
			}
		}
	}()
	go func() {
		defer background.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				_ = a.Tick(ctx, now)
			}
		}
	}()
	for {
		c, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		controls.Add(1)
		go func() {
			defer controls.Done()
			defer c.Close()
			if verifyPeer(c, a.vault.UID) != nil {
				return
			}
			_ = c.SetDeadline(time.Now().Add(90 * time.Second))
			var request Control
			decoder := json.NewDecoder(io.LimitReader(c, 64<<10))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&request) != nil {
				return
			}
			requestCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			value, err := a.control(requestCtx, request)
			var reply Reply
			if err != nil {
				reply.Error = err.Error()
			} else {
				reply.Value, _ = json.Marshal(value)
			}
			_ = json.NewEncoder(c).Encode(reply)
		}()
	}
}
func LocalCall(ctx context.Context, dir string, request Control, out any) error {
	real, uid, err := privateDirectory(dir)
	if err != nil {
		return err
	}
	var dialer net.Dialer
	c, err := dialer.DialContext(ctx, "unix", filepath.Join(real, "control.sock"))
	if err != nil {
		return errors.New("personal agent is not running; start it in your own OS login")
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	socket, ok := c.(*net.UnixConn)
	if !ok {
		return ErrDenied
	}
	if err = verifyPeer(socket, uid); err != nil {
		return err
	}
	deadline := time.Now().Add(90 * time.Second)
	if when, ok := ctx.Deadline(); ok && when.Before(deadline) {
		deadline = when
	}
	_ = c.SetDeadline(deadline)
	if err = json.NewEncoder(c).Encode(request); err != nil {
		return err
	}
	var reply Reply
	if json.NewDecoder(io.LimitReader(c, 12<<20)).Decode(&reply) != nil {
		return errors.New("invalid local agent response")
	}
	if reply.Error != "" {
		return errors.New(reply.Error)
	}
	if out != nil {
		if json.Unmarshal(reply.Value, out) != nil {
			return errors.New("invalid local control result")
		}
	}
	return nil
}
