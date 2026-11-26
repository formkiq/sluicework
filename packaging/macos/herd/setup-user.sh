#!/bin/bash
# Invoked in the active desktop user's session by the privileged installer.
set -euo pipefail
export PATH=/usr/bin:/bin:/usr/sbin:/sbin
umask 077
[[ $EUID -ne 0 ]] || { echo 'Herd must run as the desktop user, not root.' >&2; exit 1; }
[[ $# -eq 7 ]] || { echo 'Expected state, mode, router, Ollama URL, CA, distribution, uv.' >&2; exit 1; }
state=$1
mode=$2
router=$3
ollama=$4
ca=$5
distribution=$6
uv=$7
[[ "$state" == /* && ! -L "$state" ]] || exit 1
case "$mode" in router|node|prepare) ;; *) exit 1;; esac
mkdir -p "$state"
chmod 0700 "$state"
export UV_CACHE_DIR="$state/cache"
export UV_PYTHON_INSTALL_DIR="$state/python"
export UV_PYTHON_DOWNLOADS=automatic
# Version plus lock hash keeps old environments available for rollback.
lockhash=$(shasum -a 256 "$distribution/uv.lock")
lockhash=${lockhash%% *}
project="$state/herd-0.12.9-${lockhash:0:12}"
[[ ! -L "$project" && ! -L "$project/pyproject.toml" && ! -L "$project/uv.lock" ]] || exit 1
if [[ -e "$project" && ! -f "$project/.sluicework-managed" ]]; then
    echo "Refusing to replace an unmanaged environment: $project" >&2
    exit 1
fi
mkdir -p "$project"
cp "$distribution/pyproject.toml" "$distribution/uv.lock" "$project/"
touch "$project/.sluicework-managed"
"$uv" sync --project "$project" --locked --no-dev --no-install-project --no-build --python 3.12 --managed-python --no-config
"$project/.venv/bin/herd" --version
"$project/.venv/bin/herd-node" --version
[[ "$mode" != prepare ]] || exit 0
exec "$project/.venv/bin/python" "$distribution/launchd.py" "$state" "$mode" "$router" "$ollama" "$ca" "$project"
