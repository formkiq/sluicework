#!/usr/bin/env bash
set -euo pipefail
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
exec python3 -I "$(cd "$(dirname "$0")" && pwd)/install.py" "$@"
