#!/usr/bin/env bash
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
out=${1:-"$repo/release"}
[[ $# -eq 0 ]] || shift
if [[ $# -eq 0 ]]; then set -- darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; fi
for target in "$@"; do
    case "$target" in darwin/arm64|darwin/amd64|linux/arm64|linux/amd64) ;;
        *) echo "Unsupported target: $target" >&2; exit 1;; esac
done
mkdir -p "$out"
out=$(cd "$out" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
(cd "$repo/web" && npm ci && npm run build)
for target in "$@"; do
    os=${target%/*}
    arch=${target#*/}
    (cd "$repo" && GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 "${GO:-go}" build -buildvcs=false -trimpath -o "$tmp/sluicework" ./cmd/sluicework)
    if [[ "$os" == darwin ]]; then
        SLUICEWORK_ARCH="$arch" SLUICEWORK_BINARY="$tmp/sluicework" bash "$repo/packaging/macos/build-installer.sh" "$out/Sluicework-macOS-$arch.run"
    else
        name="Sluicework-Linux-$arch"
        stage="$tmp/$name"
        mkdir -p "$stage/herd" "$stage/open-webui"
        cp "$tmp/sluicework" "$stage/sluicework"
        cp "$repo/packaging/linux/"*.py "$repo/packaging/linux/"*.sh "$repo/packaging/linux/"*.service "$stage/"
        cp "$repo/packaging/linux/"*.apparmor "$stage/"
        cp "$repo/packaging/macos/herd/pyproject.toml" "$repo/packaging/macos/herd/uv.lock" "$stage/herd/"
        cp "$repo/packaging/macos/open-webui/pyproject.toml" "$repo/packaging/macos/open-webui/uv.lock" "$repo/packaging/macos/open-webui/uv.toml" "$stage/open-webui/"
        printf '%s\n' "$arch" > "$stage/target-arch"
        chmod 0755 "$stage/install.sh" "$stage/sluicework"
        tar -czf "$out/$name.tar.gz" -C "$tmp" "$name"
        echo "$out/$name.tar.gz"
    fi
done
python3 - "$out" "$@" <<'PY'
import hashlib
from pathlib import Path
import sys
root = Path(sys.argv[1])
files = []
for target in sys.argv[2:]:
    os, arch = target.split('/')
    files.append(root / (f'Sluicework-macOS-{arch}.run' if os == 'darwin' else f'Sluicework-Linux-{arch}.tar.gz'))
with (root / 'SHA256SUMS').open('w') as out:
    for path in files:
        digest = hashlib.sha256()
        with path.open('rb') as source:
            for chunk in iter(lambda: source.read(1024 * 1024), b''):
                digest.update(chunk)
        out.write(f'{digest.hexdigest()}  {path.name}\n')
PY
