package personal

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"sluicework/internal/provider"
)

// No task names, prompts, reports, URLs or model text enter desktop alerts.
// Command arguments and AppleScript are fixed application strings.
func notificationCommand(platform, status string) (name string, args []string, input string, err error) {
	body := "A personal scheduled task finished. View its report in your private Sluicework runtime."
	if status != "succeeded" {
		body = "A personal scheduled task did not complete. Check your private Sluicework runtime."
	}
	switch platform {
	case "darwin":
		return "/usr/bin/osascript", []string{"-"}, `display notification "` + body + `" with title "Sluicework"`, nil
	case "linux":
		return "notify-send", []string{"--app-name=Sluicework", "--", "Sluicework", body}, "", nil
	default:
		return "", nil, "", errors.New("desktop notifications are unavailable on this platform")
	}
}
func desktopNotify(ctx context.Context, status string) error {
	name, args, input, err := notificationCommand(runtime.GOOS, status)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(input)
	if err = cmd.Run(); err != nil {
		return errors.New("desktop notification could not be submitted; check the logged-in desktop session, OS notification permissions and notify-send on Linux")
	}
	return nil
}
func (a *Agent) finishRun(id, status, message, text string, citations []provider.Citation, searched, notify bool) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE personal_local_runs SET status=?,message=?,result=? WHERE id=?`, status, message, text, id); err != nil {
		return err
	}
	raw, err := json.Marshal(citations)
	if err != nil {
		return err
	}
	notification := "not_requested"
	if notify {
		notification = "pending"
	}
	if _, err = tx.Exec(`INSERT INTO personal_run_details(run_id,citations,web_searched,notification) VALUES(?,?,?,?)`, id, string(raw), searched, notification); err != nil {
		return err
	}
	return tx.Commit()
}
func (a *Agent) notifyRun(id, status string) {
	// Reserve delivery durably before contacting the desktop. Never replay an
	// ambiguous submission after a crash, or retry inference for a delivery error.
	res, err := a.db.Exec(`UPDATE personal_run_details SET notification='attempting' WHERE run_id=? AND notification='pending'`, id)
	if err != nil {
		return
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	outcome := "submitted"
	if a.notify(ctx, status) != nil {
		outcome = "failed"
	}
	_, _ = a.db.Exec(`UPDATE personal_run_details SET notification=? WHERE run_id=? AND notification='attempting'`, outcome, id)
}
