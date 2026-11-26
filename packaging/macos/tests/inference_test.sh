#!/bin/bash
# Non-privileged tests: all app publication happens inside a disposable directory.
set -euo pipefail
repo=$(cd "$(dirname "$0")/../../.." && pwd)
source "$repo/packaging/macos/inference.sh"
check() { "$@" || { echo "Assertion failed: $*" >&2; exit 1; }; }
run_case() (
    local test=$1
    tmp=$(mktemp -d)
    trap 'rm -rf "$tmp"' EXIT
    base="$tmp/base"; etc="$base/etc"; srv="$etc/server.json"
    mkdir -p "$tmp/Applications" "$etc"
    bin="$tmp/sluicework"
    cat > "$bin" <<'BINARY'
#!/bin/bash
if [[ "$1" == help ]]; then echo "runtime-check open-webui-check"; exit 0; fi
exit 1
BINARY
    chmod +x "$bin"
    ollama_applications_dir() { printf '%s\n' "$tmp/Applications"; }
    inference_probe() { return 1; }
    ollama_existing_cli() { return 1; }
    sw_vers() { echo 14.0; }
    inference_desktop_user() { inferenceUser=test-user; inferenceUID=501; inferenceHome="$tmp/user"; return 0; }
    inference_download() { echo 'Unexpected download' >&2; return 99; }
    install_inference_helpers() { :; }
    "$test"
    echo "PASS $test"
)
test_skip() {
    read_text() { printf -v "$2" '%s' 3; }
    inference_setup
    check test ! -e "$tmp/Applications/Ollama.app"
}
test_macos_gate() {
    sw_vers() { echo 13.6; }
    if ollama_install_app; then exit 1; fi
    check test ! -e "$tmp/Ollama-darwin.zip"
}
test_no_desktop() {
    inference_desktop_user() { return 1; }
    if ollama_install_app; then exit 1; fi
    check test ! -e "$tmp/Ollama-darwin.zip"
}
test_preserve_existing() {
    mkdir "$tmp/Applications/Ollama.app"
    echo existing > "$tmp/Applications/Ollama.app/sentinel"
    ollama_install_app
    check test "$(cat "$tmp/Applications/Ollama.app/sentinel")" = existing
}
test_preserve_cli() {
    ollama_existing_cli() { return 0; }
    if ollama_install_app; then exit 1; fi
    check test ! -e "$tmp/Ollama-darwin.zip"
}
test_checksum_rejection() {
    inference_download() { echo invalid > "$2"; }
    if ollama_install_app; then exit 1; fi
    check test ! -e "$tmp/Applications/Ollama.app"
}
prepare_archive() {
    mkdir -p "$tmp/fixture/Ollama.app"
    echo official-fixture > "$tmp/fixture/Ollama.app/sentinel"
    /usr/bin/ditto -c -k --keepParent "$tmp/fixture/Ollama.app" "$tmp/fixture.zip"
    inference_download() { cp "$tmp/fixture.zip" "$2"; }
    inference_checksum() { return 0; }
    ollama_verify_app() { test -f "$1/sentinel"; }
    chown() { :; }
    id() { if [[ "$1" == -gn ]]; then echo staff; else command id "$@"; fi; }
}
test_publish_verified_archive() {
    prepare_archive
    ollama_install_app
    check test "$(cat "$tmp/Applications/Ollama.app/sentinel")" = official-fixture
}
test_signature_rejection() {
    prepare_archive
    ollama_verify_app() { return 1; }
    if ollama_install_app; then exit 1; fi
    check test ! -e "$tmp/Applications/Ollama.app"
}
test_publish_race_preserves_target() {
    prepare_archive
    ollama_verify_app() { mkdir -p "$tmp/Applications/Ollama.app"; echo concurrent > "$tmp/Applications/Ollama.app/sentinel"; }
    if ollama_install_app; then exit 1; fi
    check test "$(cat "$tmp/Applications/Ollama.app/sentinel")" = concurrent
    check test ! -e "$tmp/Applications/Ollama.app/Ollama.app"
}
test_launch_as_desktop_user() {
    mkdir "$tmp/Applications/Ollama.app"
    inference_as_user() { printf '%s\n' "$@" > "$tmp/launch-args"; }
    inference_wait() { test "$1" = http://127.0.0.1:11434; }
    ollama_start_app
    check test "$(head -1 "$tmp/launch-args")" = /usr/bin/open
    check grep -q -- --args "$tmp/launch-args"
}
test_reuse_endpoint() {
    read_text() { if [[ "$1" == Choose* ]]; then printf -v "$2" '%s' 1; else printf -v "$2" '%s' http://127.0.0.1:11435; fi; }
    inference_probe() { printf '{"version":"test"}' > "$tmp/runtime.json"; }
    inference_register() { echo "$1" > "$tmp/registered"; }
    inference_setup
    check test "$(cat "$tmp/registered")" = http://127.0.0.1:11435
}
test_herd_declined() {
    local reads=0
    read_text() { reads=$((reads+1)); if [[ $reads -eq 1 ]]; then printf -v "$2" '%s' 1; else printf -v "$2" '%s' n; fi; }
    herd_install_uv() { echo 'Herd must not install after declining' >&2; exit 1; }
    herd_setup
    check test ! -e "$tmp/Ollama-darwin.zip"
}
test_open_webui_reuse_existing() {
    local reads=0
    read_text() {
        reads=$((reads+1))
        case "$reads" in
            1) printf -v "$2" '%s' 5;;
            2) printf -v "$2" '%s' 2;;
            3) printf -v "$2" '%s' http://127.0.0.1:8080;;
            4) printf -v "$2" '%s' http://127.0.0.1:11434;;
        esac
    }
    open_webui_probe() { test "$1" = http://127.0.0.1:8080; }
    open_webui_register() { printf '%s %s\n' "$1" "$2" > "$tmp/open-webui-registered"; }
    inference_setup
    check test "$(cat "$tmp/open-webui-registered")" = "http://127.0.0.1:8080 http://127.0.0.1:11434"
}
test_open_webui_intel_preserves_host() {
    read_text() { printf -v "$2" '%s' 1; }
    uname() { echo x86_64; }
    herd_install_uv() { echo 'Intel must not install a managed runtime' >&2; exit 1; }
    ollama_install_app() { echo 'Intel must not install Ollama for this option' >&2; exit 1; }
    open_webui_setup
}
test_open_webui_preserve_existing_cli() {
    mkdir -p "$tmp/user/.local/bin"
    touch "$tmp/user/.local/bin/open-webui"
    read_text() { printf -v "$2" '%s' 1; }
    herd_install_uv() { echo 'Open WebUI must not install over existing CLI' >&2; exit 1; }
    open_webui_setup
}
test_open_webui_prefers_herd_when_running() {
    read_text() { printf -v "$2" '%s' 1; }
    inference_probe() { test "$1" = http://127.0.0.1:11435; }
    open_webui_probe() { return 1; }
    herd_install_uv() { return 0; }
    inference_as_user() { printf '%s\n' "$@" > "$tmp/open-webui-args"; }
    open_webui_wait() { test "$1" = http://127.0.0.1:8080; }
    open_webui_register() { printf '%s %s\n' "$1" "$2" > "$tmp/open-webui-registered"; }
    open_webui_setup
    check grep -q 'http://127.0.0.1:11435' "$tmp/open-webui-args"
    check test "$(cat "$tmp/open-webui-registered")" = "http://127.0.0.1:8080 http://127.0.0.1:11435"
}
for case_name in test_skip test_macos_gate test_no_desktop test_preserve_existing test_preserve_cli test_checksum_rejection test_publish_verified_archive test_signature_rejection test_publish_race_preserves_target test_launch_as_desktop_user test_reuse_endpoint test_herd_declined test_open_webui_reuse_existing test_open_webui_preserve_existing_cli test_open_webui_prefers_herd_when_running; do
    run_case "$case_name"
done

run_case test_open_webui_intel_preserves_host
