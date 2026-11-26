#!/usr/bin/env bash
# Downloads only verified tooling/runtime packages; never installs a host service.
set -euo pipefail
umask 077
repo=$(cd "$(dirname "$0")/../../.." && pwd)
source_dir="$repo/packaging/macos/open-webui"
temp=$(mktemp -d)
trap 'rm -rf "$temp"' EXIT
uv=${1:-}
if [[ -z "$uv" ]]; then
    case "$(uname -s)/$(uname -m)" in
        Darwin/arm64) target=uv-aarch64-apple-darwin; digest=5aeca67f2283f75235de40d0bd9c0a5bbc5fce23b6d48b3bf9fa1b2db08382ca;;
        Darwin/x86_64) target=uv-x86_64-apple-darwin; digest=617a3211261b0fc739562f0071aa34e9739797c510208ede8e2977c30903d041;;
        Linux/aarch64) target=uv-aarch64-unknown-linux-gnu; digest=0797542018e541a9288855df555c8077eed06f7a47d206362030480e8ecc9f3d;;
        Linux/x86_64) target=uv-x86_64-unknown-linux-gnu; digest=a7fbaff2b9a271d85c1bdb540941090fd1ce011797b4abeb48e4dcd39c2471d5;;
        *) echo 'Unsupported Open WebUI test host' >&2; exit 1;;
    esac
    curl --disable --fail --show-error --silent --location --proto '=https' --proto-redir '=https' \
        --connect-timeout 20 --max-time 180 \
        "https://github.com/astral-sh/uv/releases/download/0.13.9/$target.tar.gz" -o "$temp/uv.tar.gz"
    printf '%s  %s\n' "$digest" "$temp/uv.tar.gz" | shasum -a 256 -c -
    tar -xzf "$temp/uv.tar.gz" -C "$temp"
    uv="$temp/$target/uv"
fi
export UV_CACHE_DIR="$temp/cache"
export UV_PYTHON_INSTALL_DIR="$temp/python"
export UV_PYTHON_DOWNLOADS=automatic
if [[ $(uname -s) == Darwin ]]; then
    # Exercise the actual installer preparation path, without loading a LaunchAgent.
    bash "$source_dir/setup-user.sh" "$temp/state" prepare http://127.0.0.1:11434 '' "$source_dir" "$uv"
    projects=("$temp/state/"open-webui-*)
    project=${projects[0]}
else
    project="$temp/project"
    mkdir "$project"
    cp "$source_dir/pyproject.toml" "$source_dir/uv.lock" "$source_dir/uv.toml" "$project/"
    "$uv" sync --project "$project" --config-file "$project/uv.toml" --locked --no-dev \
        --no-install-project --managed-python --python 3.12 --quiet
fi
"$project/.venv/bin/python" "$source_dir/audit.py"
"$project/.venv/bin/python" "$source_dir/smoke.py" "$project"
