#!/bin/bash
# Invoked in the active desktop user's session by the privileged installer.
set -euo pipefail
export PATH=/usr/bin:/bin:/usr/sbin:/sbin
umask 077
[[ $EUID -ne 0 ]] || { echo 'Open WebUI must run as the desktop user, not root.' >&2; exit 1; }
[[ $(uname -m) == arm64 ]] || { echo 'Managed Open WebUI requires Apple Silicon; use an existing endpoint on Intel Macs.' >&2; exit 1; }
[[ $# -eq 6 ]] || { echo 'Expected state, mode, Ollama URL, CA, distribution, uv.' >&2; exit 1; }
state=$1
mode=$2
ollama=$3
ca=$4
distribution=$5
uv=$6
[[ "$state" == /* && ! -L "$state" ]] || exit 1
case "$mode" in serve|prepare) ;; *) exit 1;; esac
mkdir -p "$state"
chmod 0700 "$state"
export UV_CACHE_DIR="$state/cache"
export UV_PYTHON_INSTALL_DIR="$state/python"
export UV_PYTHON_DOWNLOADS=automatic
# Version plus lock hash keeps old environments available for rollback.
lockhash=$(cat "$distribution/pyproject.toml" "$distribution/uv.toml" "$distribution/uv.lock" | shasum -a 256)
lockhash=${lockhash%% *}
project="$state/open-webui-0.13.4-${lockhash:0:12}"
[[ ! -L "$project" && ! -L "$project/pyproject.toml" && ! -L "$project/uv.lock" && ! -L "$project/uv.toml" ]] || exit 1
if [[ -e "$project" && ! -f "$project/.sluicework-managed" ]]; then
    echo "Refusing to replace an unmanaged environment: $project" >&2
    exit 1
fi
mkdir -p "$project"
cp "$distribution/pyproject.toml" "$distribution/uv.lock" "$distribution/uv.toml" "$project/"
touch "$project/.sluicework-managed"
"$uv" sync --project "$project" --locked --no-dev --no-install-project --python 3.12 --managed-python --config-file "$project/uv.toml"
"$project/.venv/bin/open-webui" --help >/dev/null
[[ "$mode" != prepare ]] || exit 0
exec "$project/.venv/bin/python" "$distribution/launchd.py" "$state" "$ollama" "$ca" "$project"
