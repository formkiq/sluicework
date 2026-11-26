#!/bin/bash
# Run against the native packaged installer, without admin privileges.
set -euo pipefail
package=${1:?Provide the native .run package}
test "$EUID" -ne 0
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
destination="$scratch/user tools"
before=$(ls -ldT /Library/Sluicework /Library/LaunchDaemons/com.sluicework.* 2>/dev/null || true)
mkdir -p "$scratch/home"
HOME="$scratch/home" SHELL=/bin/zsh ZDOTDIR="$scratch/home" bash "$package" --workstation "$destination"
test "$(readlink "$scratch/home/.local/bin/sluicework")" = "$(cd "$destination" && pwd -P)/sluicework"
HOME="$scratch/home" ZDOTDIR="$scratch/home" /bin/zsh -lc 'test "$(sluicework version)" = 0.1.0'
test -x "$destination/sluicework"
test -x "$destination/manage"
"$destination/manage" --help > /dev/null
test "$("$destination/sluicework" version)" = 0.1.0
test "$(stat -f '%Lp' "$destination")" = 700
digest=$(shasum -a 256 "$destination/sluicework")
if HOME="$scratch/home" SHELL=/bin/zsh ZDOTDIR="$scratch/home" bash "$package" --workstation "$destination"; then
    echo 'Workstation extraction overwrote an existing directory' >&2
    exit 1
fi
test "$digest" = "$(shasum -a 256 "$destination/sluicework")"
after=$(ls -ldT /Library/Sluicework /Library/LaunchDaemons/com.sluicework.* 2>/dev/null || true)
test "$before" = "$after"
SLUICEWORK_HELPER_TEST_BINARY="$destination/sluicework" SLUICEWORK_HELPER_TEST_SCRIPT="$destination/manage" python3 -B -m unittest discover -s "$(dirname "$0")" -p 'test_workstation.py'
echo 'Unprivileged workstation extraction passed'
