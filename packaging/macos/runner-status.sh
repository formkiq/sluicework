#!/bin/bash
# Run as the same non-administrator user as the official runner LaunchAgent.
set -eu
export PATH=/usr/bin:/bin
umask 027
status_dir=/Library/Sluicework/ci-status
helper=/Library/Sluicework/bin/runner-status.sh
service=${2:-}
if [[ ! "$service" =~ ^actions\.runner\.[A-Za-z0-9_.-]+$ || $EUID -eq 0 ]]; then
    echo "Run as the logged-in runner account with: $0 install <actions.runner.service-label>" >&2
    exit 1
fi
target="gui/$(id -u)/$service"
case "${1:-}" in
    install)
        [[ -d "$status_dir" && -w "$status_dir" ]] || { echo "Ask an administrator to prepare $status_dir for the runner account." >&2; exit 1; }
        agent_dir="$HOME/Library/LaunchAgents"
        plist="$agent_dir/com.sluicework.runner-status.plist"
        mkdir -p "$agent_dir"
        cat > "$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.sluicework.runner-status</string>
  <key>ProgramArguments</key><array><string>$helper</string><string>report</string><string>$service</string></array>
  <key>StartInterval</key><integer>30</integer>
  <key>RunAtLoad</key><true/>
</dict></plist>
PLIST
        plutil -lint "$plist" >/dev/null
        if launchctl print "gui/$(id -u)/com.sluicework.runner-status" >/dev/null 2>&1; then
            launchctl bootout "gui/$(id -u)/com.sluicework.runner-status"
        fi
        launchctl bootstrap "gui/$(id -u)" "$plist"
        ;;
    report)
        if state=$(set -o pipefail
            launchctl print "$target" 2>/dev/null | awk '$1=="state" && $2=="=" && $3=="running" {running=1} END {print (running ? "running" : "stopped")}'
        ); then :; else state=unloaded; fi
        tmp=$(mktemp "$status_dir/.status.XXXXXX")
        trap 'rm -f "$tmp"' EXIT
        printf '{"service":"%s","status":"%s"}\n' "$target" "$state" > "$tmp"
        chmod 0640 "$tmp"
        mv -f "$tmp" "$status_dir/status.json"
        ;;
    *) echo "Expected install or report" >&2; exit 1;;
esac
