package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"sluicework/internal/state"
)

// Durable log IDs work across reconnects, process restarts, and attempt retries.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	token := sessionToken(r)
	if _, err := s.Auth.Lookup(token); err != nil {
		report(w, err)
		return
	}
	cursor, err := after(r)
	if err != nil {
		report(w, err)
		return
	}
	id := r.PathValue("id")
	run, err := s.State.Run(id)
	if err != nil {
		report(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var previous []byte
	for {
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		for {
			logs, e := s.State.Logs(id, cursor)
			if e != nil {
				return
			}
			for _, line := range logs {
				data, _ := json.Marshal(line)
				if _, err = fmt.Fprintf(w, "id: %d\nevent: log\ndata: %s\n\n", line.ID, data); err != nil {
					return
				}
				cursor = line.ID
			}
			if len(logs) < 1000 {
				break
			}
		}
		data, _ := json.Marshal(run)
		if !bytes.Equal(data, previous) {
			if _, err = fmt.Fprintf(w, "event: run\ndata: %s\n\n", data); err != nil {
				return
			}
			previous = data
		} else if _, err = fmt.Fprint(w, ": live\n\n"); err != nil {
			return
		}
		if err = controller.Flush(); err != nil {
			return
		}
		if terminal(run) {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
		}
		if _, e := s.Auth.Lookup(token); e != nil {
			return
		}
		run, err = s.State.Run(id)
		if err != nil {
			return
		}
	}
}
func terminal(run state.Run) bool { return run.State != "queued" && run.State != "running" }
