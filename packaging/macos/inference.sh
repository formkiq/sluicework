# Optional desktop inference setup, embedded into the single-file installer.
# The enclosing installer supplies bin, base, etc, srv, tmp and read_text.
ollama_release=0.40.16
ollama_sha256=4715531bc49d21337381b193765041e535a9fb4716f84e5d6eccc3a9af1b5303
uv_release=0.13.9
uv_sha256=5aeca67f2283f75235de40d0bd9c0a5bbc5fce23b6d48b3bf9fa1b2db08382ca
uv_binary_sha256=0e17c316d23b33b4e840669f516f5cb7f5c1faa816ebd3824052ab50082ecefb

install_inference_helpers() {
    install -d -o root -g wheel -m 0755 "$etc/herd"
    cat > "$tmp/herd-setup.sh" <<'HERD_USER_SETUP'
__HERD_USER_SETUP__
HERD_USER_SETUP
    cat > "$tmp/herd-pyproject.toml" <<'HERD_PROJECT'
__HERD_PROJECT__
HERD_PROJECT
    cat > "$tmp/herd-uv.lock" <<'HERD_LOCK'
__HERD_LOCK__
HERD_LOCK
    cat > "$tmp/herd-launchd.py" <<'HERD_LAUNCHD'
__HERD_LAUNCHD__
HERD_LAUNCHD
    install -o root -g wheel -m 0755 "$tmp/herd-setup.sh" "$base/bin/herd-setup.sh"
    install -o root -g wheel -m 0644 "$tmp/herd-pyproject.toml" "$etc/herd/pyproject.toml"
    install -o root -g wheel -m 0644 "$tmp/herd-uv.lock" "$etc/herd/uv.lock"
    install -o root -g wheel -m 0644 "$tmp/herd-launchd.py" "$etc/herd/launchd.py"
    install -d -o root -g wheel -m 0755 "$etc/open-webui"
    cat > "$tmp/open-webui-setup.sh" <<'OPEN_WEBUI_USER_SETUP'
__OPEN_WEBUI_USER_SETUP__
OPEN_WEBUI_USER_SETUP
    cat > "$tmp/open-webui-pyproject.toml" <<'OPEN_WEBUI_PROJECT'
__OPEN_WEBUI_PROJECT__
OPEN_WEBUI_PROJECT
    cat > "$tmp/open-webui-uv.lock" <<'OPEN_WEBUI_LOCK'
__OPEN_WEBUI_LOCK__
OPEN_WEBUI_LOCK
    cat > "$tmp/open-webui-uv.toml" <<'OPEN_WEBUI_CONFIG'
__OPEN_WEBUI_CONFIG__
OPEN_WEBUI_CONFIG
    cat > "$tmp/open-webui-launchd.py" <<'OPEN_WEBUI_LAUNCHD'
__OPEN_WEBUI_LAUNCHD__
OPEN_WEBUI_LAUNCHD
    install -o root -g wheel -m 0755 "$tmp/open-webui-setup.sh" "$base/bin/open-webui-setup.sh"
    install -o root -g wheel -m 0644 "$tmp/open-webui-pyproject.toml" "$etc/open-webui/pyproject.toml"
    install -o root -g wheel -m 0644 "$tmp/open-webui-uv.lock" "$etc/open-webui/uv.lock"
    install -o root -g wheel -m 0644 "$tmp/open-webui-uv.toml" "$etc/open-webui/uv.toml"
    install -o root -g wheel -m 0644 "$tmp/open-webui-launchd.py" "$etc/open-webui/launchd.py"
}
herd_install_uv() {
    local archive="$tmp/uv.tar.gz" unpacked="$tmp/uv-unpacked"
    local target=uv-aarch64-apple-darwin
    if [[ "$(uname -m)" == x86_64 ]]; then
        target=uv-x86_64-apple-darwin
        uv_sha256=617a3211261b0fc739562f0071aa34e9739797c510208ede8e2977c30903d041
        uv_binary_sha256=9d275208c058354f9b300ea46a499c7a29de3249ae839503cadff2c01c159d69
    fi
    if [[ -f "$base/bin/inference-uv" && ! -L "$base/bin/inference-uv" ]] && inference_checksum "$base/bin/inference-uv" "$uv_binary_sha256" >/dev/null 2>&1; then return 0; fi
    inference_download "https://github.com/astral-sh/uv/releases/download/$uv_release/$target.tar.gz" "$archive" || return 1
    inference_checksum "$archive" "$uv_sha256" || return 1
    mkdir -p "$unpacked" || return 1
    tar -xzf "$archive" -C "$unpacked" || return 1
    inference_checksum "$unpacked/$target/uv" "$uv_binary_sha256" || return 1
    install -o root -g wheel -m 0755 "$unpacked/$target/uv" "$base/bin/inference-uv"
}
herd_setup() {
    local choice mode router ollama caSource ca="" accept candidate caDigest
    if ! inference_desktop_user; then
        inference_note "Herd setup requires an active desktop login. Log in and rerun Configure local AI runtime."
        return 0
    fi
    inference_note "Herd 0.12.9 is optional routing on top of Ollama. It uses an isolated managed Python 3.12 environment."
    inference_note "1. Install a local router and node (router listens on 127.0.0.1:11435)"
    inference_note "2. Install a node connected to an existing HTTPS router"
    inference_note "3. Use an existing Herd endpoint (no installation)"
    inference_note "4. Configure later"
    read_text "Choose a Herd option [4]: " choice
    case "${choice:-4}" in
        1) mode=router; router=http://127.0.0.1:11435;;
        2)
            mode=node
            read_text "Existing HTTPS Herd router URL: " router
            [[ "$router" == https://* ]] || { inference_note "Use verified HTTPS for a remote router."; return 0; }
            read_text "Trusted PEM CA file (blank for public CA trust): " caSource
            if [[ -n "$caSource" ]]; then
                if ! openssl x509 -in "$caSource" -noout -subject -fingerprint -sha256; then inference_note "Invalid CA certificate."; return 0; fi
                read_text "Compare this fingerprint with the router administrator's trusted copy. Trust it? [y/N]: " accept
                [[ "$accept" =~ ^[Yy]$ ]] || return 0
                caDigest=$(shasum -a 256 "$caSource") || return 0
                caDigest=${caDigest%% *}
                ca="$etc/herd/router-ca-$caDigest.pem"
                install -m 0644 "$caSource" "$ca" || return 0
            fi
            if ! inference_probe "$router" "$ca"; then
                inference_note "Cannot verify the router. Check its URL and CA, then rerun runtime setup."
                return 0
            fi
            ;;
        3)
            read_text "Existing Herd endpoint [http://127.0.0.1:11435]: " router
            router=${router:-http://127.0.0.1:11435}
            if inference_probe "$router"; then inference_register "$router" 'Herd routing'; else inference_note "Herd is not ready. Start it and rerun runtime setup."; fi
            return 0;;
        *) return 0;;
    esac
    for candidate in "$inferenceHome/.local/bin/herd" /opt/homebrew/bin/herd /usr/local/bin/herd; do
        if [[ -e "$candidate" || -L "$candidate" ]]; then
            inference_note "An existing Herd installation was found at $candidate. Start it using its current configuration and choose Use an existing Herd endpoint. It was not replaced."
            return 0
        fi
    done
    inference_note "Herd's node agent can expose Ollama to your LAN on TCP 11434 without authentication. Other machines on that network can then use its inference and model-management API."
    inference_note "Only proceed on a trusted network. The local router's loopback binding does not encrypt or protect the node's LAN proxy."
    read_text "Install and start these Herd user agents, including the LAN proxy? [y/N]: " accept
    [[ "$accept" =~ ^[Yy]$ ]] || { inference_note "Herd setup skipped. Ollama and existing services are unchanged."; return 0; }
    ollama=http://127.0.0.1:11434
    if ! inference_probe "$ollama"; then
        if ! ollama_install_app || ! ollama_start_app; then
            inference_note "Herd needs a running Ollama runtime. Finish Ollama setup and retry."
            return 0
        fi
    fi
    if [[ "$mode" == router ]] && inference_probe "$router"; then
        inference_note "A runtime already uses port 11435. Reusing it without replacing its installation or agents."
        inference_register "$router" 'Herd routing'
        return 0
    fi
    if ! herd_install_uv || ! inference_as_user /bin/bash "$base/bin/herd-setup.sh" \
        "$inferenceHome/Library/Application Support/Sluicework/inference" "$mode" "$router" "$ollama" "$ca" "$etc/herd" "$base/bin/inference-uv"; then
        inference_note "Herd setup is incomplete. Sluicework and Ollama remain available. Inspect the setup output and retry."
        return 0
    fi
    if inference_wait "$router" "$ca"; then
        if [[ -z "$ca" ]]; then inference_register "$router" 'Herd routing'; else
            inference_note "Herd node started. Trust its CA in the orchestrator's System keychain before adding $router in AI connections."
        fi
        inference_note "Herd dashboard: $router/dashboard. User agents start at login; they do not provide inference before login."
    else
        inference_note "Herd agents were installed but the router is not ready. Check the user-agent logs under $inferenceHome/Library/Application Support/Sluicework/inference/logs."
    fi
}

inference_note() { printf '%s\n' "$*"; }
ollama_applications_dir() { printf '/Applications\n'; }
inference_desktop_user() {
    inferenceUser=$(stat -f %Su /dev/console) || return 1
    case "$inferenceUser" in ''|root|loginwindow|_*) return 1;; esac
    inferenceUID=$(id -u "$inferenceUser") || return 1
    [[ "$inferenceUID" -ge 500 ]] || return 1
    launchctl print "gui/$inferenceUID" >/dev/null 2>&1 || return 1
    inferenceHome=$(dscl . -read "/Users/$inferenceUser" NFSHomeDirectory) || return 1
    inferenceHome=${inferenceHome#NFSHomeDirectory: }
    [[ "$inferenceHome" == /* && -d "$inferenceHome" ]] || return 1
}
inference_as_user() {
    launchctl asuser "$inferenceUID" sudo -H -u "$inferenceUser" -- "$@"
}
inference_download() {
    curl --disable --fail --location --proto '=https' --proto-redir '=https' --tlsv1.2 \
        --connect-timeout 15 --max-time 900 --retry 2 --show-error --output "$2" "$1"
}
inference_checksum() { printf '%s  %s\n' "$2" "$1" | shasum -a 256 -c -; }
inference_probe() {
    local ca=${2:-}
    "$bin" runtime-check --endpoint "$1" --ca "$ca" > "$tmp/runtime.json" 2> "$tmp/runtime-error"
}
inference_wait() {
    local count
    for count in {1..20}; do
        if inference_probe "$1" "${2:-}"; then return 0; fi
        sleep 1
    done
    return 1
}
inference_register() {
    local endpoint=$1 name=$2 disabled
    if [[ -f "$srv" ]]; then
        if as_server local-ai --config "$srv" --endpoint "$endpoint" --name "$name" > "$tmp/runtime-connection.json"; then
            inference_note "Connection ready at $endpoint. In AI connections, open Manage models to install a preset."
            disabled=$(plutil -extract disabled raw -o - "$tmp/runtime-connection.json" 2>/dev/null || true)
            if [[ "$disabled" == true ]]; then inference_note "The existing connection remains disabled. Enable it in the console only if intended."; fi
        else
            inference_note "The runtime is ready, but the connection could not be added. Existing settings were preserved. Add $endpoint in AI connections."
        fi
    else
        inference_note "Runtime ready at $endpoint on this Mac. On the orchestrator, add this Mac's verified HTTPS inference endpoint in AI connections."
        inference_note "Loopback URLs on a remote worker cannot be reached by the orchestrator. See docs/macos.md for LAN inference setup."
    fi
}
ollama_existing_cli() {
    [[ -e /opt/homebrew/bin/ollama || -L /opt/homebrew/bin/ollama || -e /usr/local/bin/ollama || -L /usr/local/bin/ollama ]]
}
ollama_verify_app() {
    local app=$1 identifier release
    [[ -d "$app" && ! -L "$app" ]] || return 1
    identifier=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$app/Contents/Info.plist") || return 1
    release=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$app/Contents/Info.plist") || return 1
    [[ "$identifier" == com.electron.ollama && "$release" == "$ollama_release" ]] || return 1
    /usr/bin/codesign --verify --deep --strict --test-requirement '=anchor apple generic and certificate leaf[subject.OU] = "3MU9H2V9Y9"' "$app" || return 1
    /usr/sbin/spctl --assess --type execute "$app" || return 1
}
ollama_install_app() (
    local applications target archive unpacked stage major group
    applications=$(ollama_applications_dir) || return 1
    target="$applications/Ollama.app"
    if [[ -e "$target" || -L "$target" ]]; then
        inference_note "An Ollama app already exists. It will be reused without replacement."
        return 0
    fi
    if inference_probe http://127.0.0.1:11434; then
        inference_note "Ollama is already serving locally. Its installation and models will be reused."
        return 0
    fi
    if ollama_existing_cli; then
        inference_note "An Ollama CLI installation already exists. Start it as your user with ollama serve, then choose Use existing runtime."
        return 1
    fi
    major=$(sw_vers -productVersion) || return 1
    major=${major%%.*}
    if [[ "$major" -lt 14 ]]; then
        inference_note "Installing the current Ollama app requires macOS 14 or newer. Sluicework can continue without a local runtime."
        return 1
    fi
    if ! inference_desktop_user; then
        inference_note "Ollama app setup requires an active desktop login. Log in and rerun Configure local AI runtime."
        return 1
    fi
    archive="$tmp/Ollama-darwin.zip"
    unpacked="$tmp/ollama-unpacked"
    inference_note "Downloading official Ollama $ollama_release (about 230 MB). Existing model files stay in place."
    inference_download "https://github.com/ollama/ollama/releases/download/v$ollama_release/Ollama-darwin.zip" "$archive" || return 1
    inference_checksum "$archive" "$ollama_sha256" || return 1
    mkdir -p "$unpacked" || return 1
    /usr/bin/ditto -x -k "$archive" "$unpacked" || return 1
    ollama_verify_app "$unpacked/Ollama.app" || return 1
    stage=$(mktemp -d "$applications/.sluicework-ollama.XXXXXX") || return 1
    trap 'rm -rf "$stage"' EXIT
    /usr/bin/ditto "$unpacked/Ollama.app" "$stage/Ollama.app" || return 1
    group=$(id -gn "$inferenceUser") || return 1
    # Match a normal per-user app installation so its own updater can operate.
    chown -R "$inferenceUser:$group" "$stage/Ollama.app" || return 1
    # Passing the parent directory preserves a concurrently created target.
    mv -n "$stage/Ollama.app" "$applications/" || return 1
    [[ ! -e "$stage/Ollama.app" ]] || return 1
    inference_note "Installed $target. macOS first-launch and CLI-link prompts remain under your control."
)
ollama_start_app() {
    local app
    if inference_probe http://127.0.0.1:11434; then return 0; fi
    app="$(ollama_applications_dir)/Ollama.app"
    [[ -d "$app" && ! -L "$app" ]] || return 1
    if ! inference_desktop_user; then
        inference_note "Open Ollama after logging into the Mac, then rerun runtime setup."
        return 1
    fi
    inference_note "Starting Ollama as $inferenceUser. Complete any first-launch prompts on that desktop."
    inference_as_user /usr/bin/open -a "$app" --args hidden || return 1
    inference_wait http://127.0.0.1:11434
}

open_webui_probe() {
    local ca=${2:-}
    "$bin" open-webui-check --endpoint "$1" --ca "$ca" > "$tmp/open-webui.json" 2> "$tmp/open-webui-error"
}
open_webui_wait() {
    local count
    for count in {1..45}; do
        if open_webui_probe "$1" "${2:-}"; then return 0; fi
        sleep 1
    done
    return 1
}
open_webui_register() {
    local endpoint=$1 ollama=${2:-http://127.0.0.1:11434} enabled
    if [[ -f "$srv" ]]; then
        if as_server open-webui --config "$srv" --endpoint "$endpoint" --ollama-url "$ollama" > "$tmp/open-webui-connection.json"; then
            inference_note "Open WebUI registered at $endpoint (backend: $ollama). Open AI connections in the console to launch it."
            enabled=$(plutil -extract enabled raw -o - "$tmp/open-webui-connection.json" 2>/dev/null || true)
            if [[ "$enabled" == false ]]; then inference_note "Existing Open WebUI settings remain disabled. Enable it in Settings only if intended."; fi
        else
            inference_note "Open WebUI is ready, but could not be registered automatically. Existing settings were preserved. Add $endpoint in Settings."
        fi
    else
        inference_note "Open WebUI ready at $endpoint on this Mac. On the orchestrator, configure its verified HTTPS URL in Settings."
    fi
}
open_webui_setup() {
    local choice endpoint ollama ca="" candidate
    if ! "$bin" help | grep -q 'open-webui-check'; then
        inference_note "Upgrade Sluicework with this installer before configuring Open WebUI."
        return 0
    fi
    inference_note "Optional Open WebUI 0.13.4 slim chat interface (isolated managed Python 3.12 environment)"
    inference_note "Chat uses Ollama/Herd; local document extraction and RAG are not included."
    inference_note "1. Install and start Open WebUI locally (listens on http://127.0.0.1:8080)"
    inference_note "2. Use an existing Open WebUI endpoint (no installation)"
    inference_note "3. Configure later"
    read_text "Choose an Open WebUI option [3]: " choice
    case "${choice:-3}" in
        1)
            if ! inference_desktop_user; then
                inference_note "Open WebUI setup requires an active desktop login. Log in and rerun Configure local AI runtime."
                return 0
            fi
            for candidate in "$inferenceHome/.local/bin/open-webui" /opt/homebrew/bin/open-webui /usr/local/bin/open-webui; do
                if [[ -e "$candidate" || -L "$candidate" ]]; then
                    inference_note "An existing Open WebUI installation was found at $candidate. Start it with its current configuration and choose Use an existing Open WebUI endpoint. It was not replaced."
                    return 0
                fi
            done
            if [[ $(uname -m) != arm64 ]]; then
                inference_note "Managed Open WebUI requires Apple Silicon because patched dependencies have no Intel macOS wheels. Choose Use an existing Open WebUI endpoint on this Mac."
                return 0
            fi
            ollama=http://127.0.0.1:11434
            if inference_probe http://127.0.0.1:11435; then
                ollama=http://127.0.0.1:11435
            elif ! inference_probe "$ollama"; then
                if ! ollama_install_app || ! ollama_start_app; then
                    inference_note "Open WebUI needs a running Ollama or Herd runtime. Finish Ollama setup and retry."
                    return 0
                fi
            fi
            endpoint=http://127.0.0.1:8080
            if open_webui_probe "$endpoint"; then
                inference_note "Open WebUI is already serving at $endpoint. Reusing it without replacing its installation or agent."
                open_webui_register "$endpoint" "$ollama"
                return 0
            fi
            if ! herd_install_uv || ! inference_as_user /bin/bash "$base/bin/open-webui-setup.sh" \
                "$inferenceHome/Library/Application Support/Sluicework/open-webui" serve "$ollama" "$ca" "$etc/open-webui" "$base/bin/inference-uv"; then
                inference_note "Open WebUI setup is incomplete. Sluicework and Ollama remain available. Inspect the setup output and retry."
                return 0
            fi
            if open_webui_wait "$endpoint"; then
                open_webui_register "$endpoint" "$ollama"
                inference_note "Open WebUI is ready at $endpoint. First-time setup creates an admin login; telemetry and OpenAI API are disabled by default."
                inference_note "User agent starts at login; it does not serve before login."
            else
                inference_note "Open WebUI agent was installed but is still starting or unavailable. Check logs under $inferenceHome/Library/Application Support/Sluicework/open-webui/logs."
            fi
            ;;
        2)
            read_text "Existing Open WebUI endpoint [http://127.0.0.1:8080]: " endpoint
            endpoint=${endpoint:-http://127.0.0.1:8080}
            read_text "Ollama or Herd backend URL [http://127.0.0.1:11434]: " ollama
            ollama=${ollama:-http://127.0.0.1:11434}
            if open_webui_probe "$endpoint"; then
                open_webui_register "$endpoint" "$ollama"
            else
                inference_note "Open WebUI is not ready at $endpoint. Start it and check its URL or trusted HTTPS certificate, then retry."
            fi
            ;;
        *) inference_note "Open WebUI setup skipped. You can configure it later in Settings or rerun Configure local AI runtime.";;
    esac
}

inference_setup() {
    local choice endpoint version
    if ! "$bin" help | grep -q 'runtime-check'; then
        inference_note "Upgrade Sluicework with this installer before configuring its local AI runtime."
        return 0
    fi
    install_inference_helpers
    inference_note "Optional local AI runtime"
    inference_note "1. Use an existing Ollama or Herd endpoint"
    inference_note "2. Install and start Ollama on this Mac (macOS 14+)"
    inference_note "3. Configure later or use another Mac"
    inference_note "4. Install optional Herd routing (with a local Ollama runtime)"
    inference_note "5. Install or connect Open WebUI chat interface (uses local Ollama/Herd)"
    read_text "Choose a runtime option [3]: " choice
    case "${choice:-3}" in
        1)
            read_text "Ollama or Herd endpoint [http://127.0.0.1:11434]: " endpoint
            endpoint=${endpoint:-http://127.0.0.1:11434}
            if ! inference_probe "$endpoint"; then
                if [[ "$endpoint" != http://127.0.0.1:11434 ]] || ! ollama_start_app; then
                    inference_note "Runtime not ready. Start it and check its URL or trusted HTTPS certificate, then rerun Configure local AI runtime."
                    return 0
                fi
            fi
            version=$(plutil -extract version raw -o - "$tmp/runtime.json" 2>/dev/null || true)
            inference_note "Using runtime $version at $endpoint. Existing installation and settings stay in place."
            inference_register "$endpoint" 'Local AI'
            ;;
        2)
            if ollama_install_app && ollama_start_app; then
                inference_register http://127.0.0.1:11434 'Local AI'
            else
                inference_note "Local AI setup is incomplete. Sluicework remains installed. Rerun Configure local AI runtime when ready."
            fi
            ;;
        3) inference_note "Local runtime setup skipped. You can add an Ollama or Herd endpoint in AI connections later.";;
        4) herd_setup;;
        5) open_webui_setup;;
        *) inference_note "Unknown runtime option. Rerun Configure local AI runtime to try again.";;
    esac
}
