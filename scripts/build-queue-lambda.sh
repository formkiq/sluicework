#!/usr/bin/env bash
# Build only. No AWS API calls or credential upload.
set -euo pipefail
umask 077
repo=$(cd "$(dirname "$0")/.." && pwd)
export GOTOOLCHAIN="${GOTOOLCHAIN:-go$(sed -n 's/^go //p' "$repo/go.mod")}"
output=${1:?Usage: scripts/build-queue-lambda.sh OUTPUT_DIRECTORY}
mkdir -p "$output"
output=$(cd "$output" && pwd)
(cd "$repo" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -buildvcs=false -trimpath -tags lambda.norpc -ldflags='-s -w' -o "$output/bootstrap" ./cmd/sluicework-queue-lambda)
python3 - "$output" <<'PY'
import hashlib, pathlib, sys, zipfile
root = pathlib.Path(sys.argv[1])
with zipfile.ZipFile(root/'dispatcher.zip', 'w', zipfile.ZIP_DEFLATED) as z:
    info = zipfile.ZipInfo('bootstrap', (2026, 1, 1, 0, 0, 0))
    info.external_attr = 0o100755 << 16
    info.compress_type = zipfile.ZIP_DEFLATED
    z.writestr(info, (root/'bootstrap').read_bytes())
print('dispatcher.zip SHA256:', hashlib.sha256((root/'dispatcher.zip').read_bytes()).hexdigest())
PY
