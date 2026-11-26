#!/usr/bin/env bash
set -euo pipefail
repo=$(cd "$(dirname "$0")/../.." && pwd)
arch=${SLUICEWORK_ARCH:-arm64}
case "$arch" in arm64) machine=arm64;; amd64) machine=x86_64;; *) echo 'Choose arm64 or amd64' >&2; exit 1;; esac
out=${1:-"$repo/release/Sluicework-macOS-$arch.run"}
mkdir -p "$(dirname "$out")"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
if [[ -n "${SLUICEWORK_BINARY:-}" ]]; then
    cp "$SLUICEWORK_BINARY" "$tmp/sluicework"
else
    (cd "$repo/web" && npm ci && npm run build)
    (cd "$repo" && GOOS=darwin GOARCH="$arch" CGO_ENABLED=0 "${GO:-go}" build -buildvcs=false -trimpath -o "$tmp/sluicework" ./cmd/sluicework)
fi
file "$tmp/sluicework" | grep -q "Mach-O 64-bit.*$machine" || { echo "Expected a macOS $arch executable" >&2; exit 1; }
# LaunchDaemon templates are literal files so they can also be linted before release.
sed -e "/^__SERVER_PLIST__$/ { r $repo/packaging/macos/com.sluicework.server.plist
d
}" -e "/^__WORKER_PLIST__$/ { r $repo/packaging/macos/com.sluicework.worker.plist
d
}" -e "/^__RUNNER_STATUS__$/ { r $repo/packaging/macos/runner-status.sh
d
}" -e "/^__INFERENCE_SETUP__$/ { r $repo/packaging/macos/inference.sh
d
}" -e "/^__WORKSTATION_HELPER__$/ { r $repo/packaging/macos/workstation.py
d
}" "$repo/packaging/macos/setup.sh" > "$tmp/installer-base"
sed -e "s/__SLUICEWORK_ARCH__/$arch/g" -e "/^__HERD_USER_SETUP__$/ { r $repo/packaging/macos/herd/setup-user.sh
d
}" -e "/^__HERD_PROJECT__$/ { r $repo/packaging/macos/herd/pyproject.toml
d
}" -e "/^__HERD_LOCK__$/ { r $repo/packaging/macos/herd/uv.lock
d
}" -e "/^__HERD_LAUNCHD__$/ { r $repo/packaging/macos/herd/launchd.py
d
}" -e "/^__OPEN_WEBUI_USER_SETUP__$/ { r $repo/packaging/macos/open-webui/setup-user.sh
d
}" -e "/^__OPEN_WEBUI_PROJECT__$/ { r $repo/packaging/macos/open-webui/pyproject.toml
d
}" -e "/^__OPEN_WEBUI_LOCK__$/ { r $repo/packaging/macos/open-webui/uv.lock
d
}" -e "/^__OPEN_WEBUI_CONFIG__$/ { r $repo/packaging/macos/open-webui/uv.toml
d
}" -e "/^__OPEN_WEBUI_LAUNCHD__$/ { r $repo/packaging/macos/open-webui/launchd.py
d
}" "$tmp/installer-base" > "$tmp/installer"
bash -n "$tmp/installer"
gzip -cn "$tmp/sluicework" >> "$tmp/installer"
chmod 0755 "$tmp/installer"
mv -f "$tmp/installer" "$out"
echo "$out"
