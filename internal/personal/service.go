package personal

import (
	"context"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// InstallUserService never installs a root/system daemon. Approval state remains
// in the private runtime, and login services always honor a persisted local pause.
func InstallUserService(dir, binary string) error {
	real, uid, err := privateDirectory(dir)
	if err != nil {
		return err
	}
	v := &Vault{Dir: real, UID: uid}
	config, err := v.Config()
	if err != nil {
		return err
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path, contents, label, err := serviceDefinition(runtime.GOOS, home, binary, real, config.RuntimeID, uid)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	// Do not overwrite an independently edited or symlinked login service.
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("refusing to replace a non-regular login service")
		}
		prior, err := os.ReadFile(path)
		if err != nil || string(prior) != contents {
			return errors.New("login service already exists with different contents; review it locally before replacing")
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		if err = os.WriteFile(path, []byte(contents), 0600); err != nil {
			return err
		}
	}
	log, err := os.OpenFile(filepath.Join(real, "agent.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	log.Close()
	if runtime.GOOS == "darwin" {
		domain := fmt.Sprintf("gui/%d", uid)
		if exec.Command("launchctl", "print", domain+"/"+label).Run() == nil {
			return nil
		}
		if personalProcessRunning(real) {
			return errors.New("stop the foreground personal agent before starting its login service")
		}
		if exec.Command("launchctl", "bootstrap", domain, path).Run() != nil {
			return errors.New("login service prepared, but bootstrap failed; run it from your own graphical OS login")
		}
		return nil
	}
	if exec.Command("systemctl", "--user", "is-active", "--quiet", label).Run() == nil {
		return nil
	}
	if personalProcessRunning(real) {
		return errors.New("stop the foreground personal agent before starting its user service")
	}
	if exec.Command("systemctl", "--user", "daemon-reload").Run() != nil {
		return errors.New("user systemd manager is unavailable")
	}
	if exec.Command("systemctl", "--user", "enable", "--now", label).Run() != nil {
		return errors.New("cannot start your user-scoped personal agent service")
	}
	return nil
}
func serviceDefinition(platform, home, binary, dir, id string, uid int) (string, string, string, error) {
	for _, value := range []string{home, binary, dir, id} {
		if strings.ContainsAny(value, "\r\n\x00") {
			return "", "", "", ErrInvalid
		}
	}
	decoded, decodeErr := hex.DecodeString(id)
	if decodeErr != nil || len(decoded) != 32 {
		return "", "", "", ErrInvalid
	}
	if platform == "darwin" {
		label := "com.sluicework.personal-agent." + id[:16]
		escape := func(s string) string { var b strings.Builder; _ = xml.EscapeText(&b, []byte(s)); return b.String() }
		text := xml.Header + `<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Label</key><string>` + escape(label) + `</string><key>ProgramArguments</key><array><string>` + escape(binary) + `</string><string>personal-agent</string><string>start</string><string>--dir</string><string>` + escape(dir) + `</string></array><key>RunAtLoad</key><true/><key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict><key>ProcessType</key><string>Background</string><key>ThrottleInterval</key><integer>30</integer><key>Umask</key><integer>63</integer><key>StandardOutPath</key><string>` + escape(filepath.Join(dir, "agent.log")) + `</string><key>StandardErrorPath</key><string>` + escape(filepath.Join(dir, "agent.log")) + `</string></dict></plist>`
		return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), text, label, nil
	}
	if platform == "linux" {
		label := "sluicework-personal-" + id[:16] + ".service"
		quote := func(s string) string {
			return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`).Replace(s) + `"`
		}
		text := "[Unit]\nDescription=Sluicework personal subscription agent\n\n[Service]\nType=simple\nExecStart=" + quote(binary) + " personal-agent start --dir " + quote(dir) + "\nUMask=0077\nRestart=on-failure\nRestartSec=30\nNoNewPrivileges=true\n\n[Install]\nWantedBy=default.target\n"
		return filepath.Join(home, ".config", "systemd", "user", label), text, label, nil
	}
	return "", "", "", errors.New("personal agent login services support macOS and Linux only")
}

func personalProcessRunning(dir string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return LocalCall(ctx, dir, Control{Operation: "status"}, nil) == nil
}
