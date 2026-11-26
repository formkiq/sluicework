#!/bin/bash
# Build-installer substitutes the two plists and appends the compressed executable.
set -euo pipefail
export PATH=/usr/bin:/bin:/usr/sbin:/sbin
umask 077
base=/Library/Sluicework
etc="$base/etc"
bin="$base/bin/sluicework"
srv="$etc/server.json"
wr="$etc/worker.json"
workerCred="$base/worker/credential.json"
suName=_sluiceserver
wuName=_sluiceworker
urlLoop=http://127.0.0.1:7443
launch=/Library/LaunchDaemons
expected_arch=__SLUICEWORK_ARCH__

__INFERENCE_SETUP__

fail() { echo "$*" >&2; exit 1; }
read_text() { IFS= read -r -p "$1" "$2" </dev/tty || fail "Input closed"; }
read_secret() { IFS= read -r -s -p "$1" "$2" </dev/tty || fail "Input closed"; echo >/dev/tty; }
as_server() { sudo -H -u "$suName" -- "$bin" "$@"; }
as_worker() { sudo -H -u "$wuName" -- "$bin" "$@"; }
safe_name() { [[ "$1" =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$ ]]; }
valid_host() {
    local domain=$1 label
    [[ "$domain" =~ ^[A-Za-z0-9.-]{1,253}$ && "$domain" != *..* && "$domain" != .* && "$domain" != *. ]] || return 1
    [[ "$domain" != localhost && ! "$domain" =~ ^[0-9.]+$ ]] || return 1
    IFS=. read -r -a labels <<< "$domain"
    for label in "${labels[@]}"; do
        [[ "$label" =~ ^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$ ]] || return 1
    done
}
service_loaded() { launchctl print "system/com.sluicework.$1" >/dev/null 2>&1; }
stop_service() {
    if service_loaded "$1"; then launchctl bootout "system/com.sluicework.$1"; fi
}
start_service() {
    launchctl enable "system/com.sluicework.$1"
    if ! service_loaded "$1"; then launchctl bootstrap system "$launch/com.sluicework.$1.plist"; fi
}
wait_url() {
    local url=$1 ca=$2 count
    local opts=()
    [[ -z "$ca" ]] || opts=(--cacert "$ca")
    for count in {1..20}; do
        if curl --disable --noproxy '*' --silent --fail --max-time 2 ${opts[@]+"${opts[@]}"} "$url/api/v1/openapi.yaml" >/dev/null; then return 0; fi
        sleep 1
    done
    return 1
}
make_account() {
    local account=$1 id
    if dscl . -read "/Users/$account" >/dev/null 2>&1; then
        [[ -f "$etc/account.$account" ]] || fail "Service account $account already exists outside this installation."
        return
    fi
    if dscl . -read "/Groups/$account" >/dev/null 2>&1; then fail "Group $account already exists outside this installation."; fi
    dscl . -list /Users UniqueID > "$tmp/ids"
    dscl . -list /Groups PrimaryGroupID >> "$tmp/ids"
    for ((id=499;id>=400;id--)); do
        if ! awk -v n="$id" '$2==n {found=1} END {exit !found}' "$tmp/ids"; then break; fi
    done
    ((id >= 400)) || fail "No unused local service identity found."
    dscl . -create "/Groups/$account"
    dscl . -create "/Groups/$account" PrimaryGroupID "$id"
    dscl . -create "/Users/$account"
    dscl . -create "/Users/$account" UniqueID "$id"
    dscl . -create "/Users/$account" PrimaryGroupID "$id"
    dscl . -create "/Users/$account" UserShell /usr/bin/false
    dscl . -create "/Users/$account" NFSHomeDirectory /var/empty
    dscl . -create "/Users/$account" IsHidden 1
    dscl . -create "/Users/$account" Password '*'
    touch "$etc/account.$account"
}
unpack() {
    local line
    line=$(awk '$0=="__SLUICEWORK_PAYLOAD__" { print NR+1; exit }' "$0")
    [[ -n "$line" ]] || fail "Use the packaged .run installer."
    tail -n "+$line" "$0" | gzip -dc > "$tmp/sluicework"
    local machine=$expected_arch
    [[ "$machine" != amd64 ]] || machine=x86_64
    file "$tmp/sluicework" | grep -q "Mach-O 64-bit.*$machine" || fail "Invalid executable payload"
    chmod 0755 "$tmp/sluicework"
    local release
    release=$("$tmp/sluicework" version)
    echo "Sluicework $release"
}
install_binary() {
    install -o root -g wheel -m 0755 "$tmp/sluicework" "$base/bin/.sluicework.new"
    mv "$base/bin/.sluicework.new" "$bin"
    "$bin" install-cli --system || echo "CLI shortcut setup needs attention; Use $bin directly." >&2
    cat > "$tmp/runner-status.sh" <<'STATUS_PROBE'
__RUNNER_STATUS__
STATUS_PROBE
    install -o root -g wheel -m 0755 "$tmp/runner-status.sh" "$base/bin/runner-status.sh"
}
write_plist() {
    local label=$1 f="$tmp/$1.plist"
    if [[ "$label" == server ]]; then
        cat > "$f" <<'SERVER_PLIST'
__SERVER_PLIST__
SERVER_PLIST
    else
        cat > "$f" <<'WORKER_PLIST'
__WORKER_PLIST__
WORKER_PLIST
    fi
    plutil -lint "$f" >/dev/null
    install -o root -g wheel -m 0644 "$f" "$launch/com.sluicework.$label.plist"
}
write_server_config() {
    # All interpolated settings are installer-owned constants or restricted DNS names.
    cat > "$tmp/server.json" <<JSON
{
    "dataDir": "$base/data",
    "listen": "$1",
    "tlsCert": "$2",
    "tlsKey": "$3"
}
JSON
    install -m 0644 "$tmp/server.json" "$srv"
}
machine_name() {
    local defaultName
    defaultName=$(scutil --get LocalHostName 2>/dev/null || hostname -s)
    safe_name "$defaultName" || defaultName=mac-worker
    read_text "Worker name [$defaultName]: " node
    node=${node:-$defaultName}
    safe_name "$node" || fail "Use a machine name of up to 63 letters, digits, dots, underscores or hyphens."
}
write_worker_config() {
    cat > "$tmp/worker.json" <<JSON
{
    "url": "$1",
    "caFile": "$2",
    "credentialFile": "$workerCred",
    "name": "$node",
    "workDir": "$base/worker/jobs",
    "roles": ["script", "agent"],
    "inputRoot": "$base/inputs",
    "runnerStatusFile": "$base/ci-status/status.json"
}
JSON
    install -m 0644 "$tmp/worker.json" "$wr"
}
prepare_worker() {
    make_account "$wuName"
    install -d -o "$wuName" -g "$wuName" -m 0700 "$base/worker"
    install -d -o root -g "$wuName" -m 0750 "$base/inputs"
    write_plist worker
}
create_cluster() {
    [[ ! -f "$wr" || -f "$srv" ]] || fail "This Mac already has a worker. Use Upgrade for an existing installation."
    if [[ -e "$workerCred" ]]; then fail "This cluster is already enrolled. Choose HTTPS or Upgrade."; fi
    machine_name
    make_account "$suName"
    prepare_worker
    install -d -o "$suName" -g "$suName" -m 0700 "$base/data" "$base/backups"
    [[ -f "$bin" ]] || install_binary
    write_plist server
    write_server_config 127.0.0.1:7443 "" ""
    if [[ ! -f "$etc/initialized" ]]; then
        read_secret "New admin password (minimum 12 characters): " password
        read_secret "Confirm admin password: " confirm
        [[ "$password" == "$confirm" && ${#password} -ge 12 ]] || fail "Password mismatch or too short."
        printf '%s\n' "$password" | as_server init --config "$srv"
        unset password confirm
        touch "$etc/initialized"
    fi
    start_service server
    wait_url "$urlLoop" "" || fail "Server did not become ready at $urlLoop."
    write_worker_config "$urlLoop" ""
    as_server enroll-token --config "$srv" | as_worker join --config "$wr"
    start_service worker
    echo "Cluster created. Sign in as admin at $urlLoop."
    read_text "Enable HTTPS for other machines now? [y/N]: " yesLan
    if [[ "$yesLan" =~ ^[Yy]$ ]]; then configure_lan; fi
    inference_setup
}
generate_cert() {
    local host=$1 bundle=$2 pki="$base/pki"
    install -d -o root -g wheel -m 0700 "$pki"
    if [[ ! -f "$pki/ca.pem" ]]; then
        [[ ! -e "$pki/ca.key" ]] || fail "CA generation was interrupted. Inspect $pki before continuing."
        cat > "$tmp/ca.cnf" <<'CONF'
[req]
distinguished_name=dn
x509_extensions=ca
prompt=no
[dn]
CN=Sluicework cluster CA
[ca]
basicConstraints=critical,CA:TRUE,pathlen:0
keyUsage=critical,keyCertSign,cRLSign
subjectKeyIdentifier=hash
CONF
        openssl req -x509 -newkey rsa:3072 -nodes -days 3650 -sha256 -config "$tmp/ca.cnf" -keyout "$pki/ca.key" -out "$pki/ca.pem"
    fi
    openssl req -new -newkey rsa:2048 -nodes -sha256 -subj "/CN=$host" -keyout "$tmp/server.key" -out "$tmp/server.csr"
    cat > "$tmp/server.ext" <<CONF
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=DNS:$host
CONF
    openssl x509 -req -sha256 -days 365 -in "$tmp/server.csr" -CA "$pki/ca.pem" -CAkey "$pki/ca.key" -CAcreateserial -extfile "$tmp/server.ext" -out "$tmp/server.pem"
    install -m 0644 "$tmp/server.pem" "$bundle/server.pem"
    install -o root -g "$suName" -m 0640 "$tmp/server.key" "$bundle/server.key"
    install -m 0644 "$pki/ca.pem" "$bundle/ca.pem"
}
copy_ca() {
    local source=$1 dest=$2 accept
    [[ -f "$source" ]] || fail "CA certificate missing."
    openssl x509 -in "$source" -noout -subject -fingerprint -sha256
    read_text "Compare this CA fingerprint with the administrator's trusted copy. Trust it? [y/N]: " accept
    [[ "$accept" =~ ^[Yy]$ ]] || fail "CA trust was declined."
    install -m 0644 "$source" "$dest"
}
change_local_credential_origin() {
    # The worker pins its credential to an origin. This change is only for the
    # already-enrolled worker on the Mac hosting this same orchestrator.
    sudo -H -u "$wuName" -- /bin/bash -s -- "$workerCred" "$1" <<'REBIND'
set -e
umask 077
next=$(mktemp "$1.XXXXXX")
trap 'rm -f "$next"' EXIT
cp "$1" "$next"
plutil -replace url -string "$2" "$next"
plutil -convert json "$next"
chmod 0600 "$next"
mv -f "$next" "$1"
REBIND
}
configure_lan() {
    local host kind bundle cert key ca caSource
    [[ -f "$srv" && -f "$workerCred" ]] || fail "Create a cluster on this Mac first."
    read_text "Stable DNS name resolving to this Mac: " host
    valid_host "$host" || fail "Use a DNS hostname."
    bundle=$(mktemp -d "$etc/tls.XXXXXX")
    chmod 0755 "$bundle"
    read_text "Certificate source: [1] cluster CA, [2] customer-supplied [1]: " kind
    case "${kind:-1}" in
        1) generate_cert "$host" "$bundle"; ca="$bundle/ca.pem";;
        2)
            read_text "PEM server certificate chain file: " cert
            read_text "Matching unencrypted PEM private key file: " key
            read_text "PEM CA file (blank for macOS system trust): " caSource
            install -m 0644 "$cert" "$bundle/server.pem"
            install -o root -g "$suName" -m 0640 "$key" "$bundle/server.key"
            ca=""
            if [[ -n "$caSource" ]]; then ca="$bundle/ca.pem"; copy_ca "$caSource" "$ca"; fi;;
        *) fail "Unknown certificate source.";;
    esac
    cp "$srv" "$tmp/old-server.json"
    cp "$wr" "$tmp/old-worker.json"
    # Preserve all manually configured settings, including role and local caps.
    for name in server worker; do cp "$tmp/old-$name.json" "$tmp/new-$name.json"; done
    plutil -replace listen -string '0.0.0.0:7443' "$tmp/new-server.json"
    plutil -replace tlsCert -string "$bundle/server.pem" "$tmp/new-server.json"
    plutil -replace tlsKey -string "$bundle/server.key" "$tmp/new-server.json"
    plutil -replace url -string "https://$host:7443" "$tmp/new-worker.json"
    plutil -replace caFile -string "$ca" "$tmp/new-worker.json"
    plutil -convert json "$tmp/new-server.json"
    plutil -convert json "$tmp/new-worker.json"
    stop_service worker
    stop_service server
    install -m 0644 "$tmp/new-server.json" "$srv"
    install -m 0644 "$tmp/new-worker.json" "$wr"
    if start_service server && wait_url "https://$host:7443" "$ca" && change_local_credential_origin "https://$host:7443"; then
        start_service worker
    else
        stop_service server
        install -m 0644 "$tmp/old-server.json" "$srv"
        install -m 0644 "$tmp/old-worker.json" "$wr"
        start_service server
        start_service worker
        fail "HTTPS configuration failed. Previous settings restored. Check DNS, certificate SAN, trust chain, private key and port 7443."
    fi
    echo "Cluster URL: https://$host:7443"
    if [[ -n "$ca" ]]; then
        echo "Distribute this PUBLIC certificate and verify its fingerprint out of band: $ca"
        openssl x509 -in "$ca" -noout -fingerprint -sha256
    fi
}
join_cluster() {
    local url host caSource ca token
    [[ ! -e "$srv" && ! -e "$workerCred" ]] || fail "This machine already has cluster state."
    read_text "Cluster URL (https://hostname:7443): " url
    url=${url%/}
    [[ "$url" =~ ^https://([A-Za-z0-9.-]+)(:[0-9]{1,5})?$ ]] || fail "Enter an HTTPS URL with a DNS name."
    host=${BASH_REMATCH[1]}
    valid_host "$host" || fail "Invalid cluster hostname."
    read_text "PEM CA file (blank for macOS system trust): " caSource
    ca=""
    if [[ -n "$caSource" ]]; then ca="$etc/join-ca.pem"; copy_ca "$caSource" "$ca"; fi
    wait_url "$url" "$ca" || fail "Cannot verify the HTTPS cluster at $url."
    machine_name
    prepare_worker
    [[ -f "$bin" ]] || install_binary
    write_worker_config "$url" "$ca"
    read_secret "One-use enrollment token: " token
    printf '%s\n' "$token" | as_worker join --config "$wr"
    unset token
    start_service worker
    echo "$node joined $url. Check its heartbeat in Machines."
    inference_setup
}
reenroll() {
    local staged nextCred token
    [[ -f "$wr" && -f "$workerCred" ]] || fail "This Mac needs an existing worker configuration and credential."
    read_secret "One-use token from the restored cluster: " token
    # Stage the replacement before stopping the current worker. The worker
    # account performs all writes inside its own credential directory.
    staged=$(mktemp -d "$etc/reenroll.XXXXXX")
    chmod 0755 "$staged"
    nextCred="$base/worker/credential-${staged##*.}.json"
    cp "$wr" "$staged/worker.json"
    plutil -replace credentialFile -string "$nextCred" "$staged/worker.json"
    plutil -convert json "$staged/worker.json"
    chmod 0644 "$staged/worker.json"
    if ! printf '%s\n' "$token" | as_worker join --config "$staged/worker.json"; then
        rm -r "$staged"
        fail "Enrolment failed. Check the existing worker URL and CA, then issue a fresh token."
    fi
    unset token
    rm -r "$staged"
    stop_service worker
    sudo -H -u "$wuName" -- mv -f "$nextCred" "$workerCred"
    start_service worker
    echo "Worker re-enrolled. Assign its labels and capacity in Machines."
}

upgrade() {
    local backup url ca
    [[ -f "$bin" && -f "$wr" ]] || fail "No worker installation found."
    if [[ -f "$srv" ]]; then
        backup="$base/backups/upgrade-$(date -u +%Y%m%dT%H%M%SZ)"
        as_server backup --config "$srv" --out "$backup"
        echo "Pre-upgrade snapshot: $backup"
    fi
    stop_service worker
    if [[ -f "$srv" ]]; then stop_service server; fi
    install_binary
    if [[ -f "$srv" ]]; then write_plist server; start_service server; fi
    write_plist worker
    start_service worker
    if [[ -f "$srv" ]]; then
        url=$(plutil -extract url raw -o - "$wr")
        ca=$(plutil -extract caFile raw -o - "$wr" 2>/dev/null || true)
        wait_url "$url" "$ca" || fail "Updated services installed; the orchestrator did not become ready. Inspect launchctl state. The pre-upgrade snapshot is at $backup."
    fi
    echo "Upgrade installed. Check Machines for the current heartbeat."
}
check_platform() {
    local machine=$expected_arch
    [[ "$machine" != amd64 ]] || machine=x86_64
    [[ "$(uname -s)" == Darwin && "$(uname -m)" == "$machine" ]] || fail "This installer requires macOS $expected_arch."
    [[ "$(sw_vers -productVersion | cut -d. -f1)" -ge 12 ]] || fail "Sluicework requires macOS 12 or later."
}
workstation_binary() {
    check_platform
    [[ $EUID -ne 0 ]] || fail "Run --workstation without sudo, in the dedicated runner account."
    [[ $# -eq 1 ]] || fail "Provide a new absolute directory: --workstation /Users/runner/Sluicework-pilot"
    [[ "$1" == /* && "$1" != / && ! -e "$1" && ! -L "$1" ]] || fail "Provide a new absolute directory: --workstation /Users/runner/Sluicework-pilot"
    local destination=$1
    tmp=$(mktemp -d)
    trap 'rm -rf "$tmp"' EXIT
    unpack
    mkdir -m 0700 "$destination"
    install -m 0755 "$tmp/sluicework" "$destination/sluicework"
    cat > "$destination/manage" <<'WORKSTATION_HELPER'
__WORKSTATION_HELPER__
WORKSTATION_HELPER
    chmod 0700 "$destination/manage"
    "$destination/sluicework" install-cli || echo "CLI shortcut setup needs attention; use $destination/sluicework directly." >&2
    echo "Workstation binary: $destination/sluicework"
    echo "Next: run \"$destination/manage\" setup"
    echo "Setup: https://github.com/formkiq/sluicework/blob/main/docs/workstation.md"
}
main() {
    check_platform
    [[ $EUID -eq 0 ]] || fail "Run with sudo /bin/bash $0"
    [[ -t 0 || -e /dev/tty ]] || fail "Interactive setup requires a terminal."
    install -d -o root -g wheel -m 0755 "$base" "$base/bin" "$etc"
    tmp=$(mktemp -d "$base/.install.XXXXXX")
    trap 'rm -rf "$tmp"' EXIT
    unpack
    echo "Sluicework"
    echo "1. Create a cluster"
    echo "2. Join a cluster"
    echo "3. Enable or update cluster HTTPS"
    echo "4. Upgrade this Mac"
    echo "5. Re-enrol an existing worker after restore"
    echo "6. Configure local AI runtime (Ollama / Herd / Open WebUI)"
    read_text "Choose a setup flow: " flow
    case "$flow" in
        1) create_cluster;;
        2) join_cluster;;
        3) configure_lan;;
        4) upgrade;;
        5) reenroll;;
        6)
            [[ -f "$bin" ]] || fail "Install Sluicework on this Mac first."
            inference_setup
            ;;
        *) fail "Unknown setup flow.";;
    esac
}
if [[ "${1:-}" == --workstation ]]; then
    shift
    workstation_binary "$@"
else
    main "$@"
fi
exit 0
__SLUICEWORK_PAYLOAD__
