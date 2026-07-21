#!/usr/bin/env bash
# Etronium installer — one-command bootstrap for scheduler, lord, or tenant role.
#
# Modes:
#   ./installer.sh scheduler --http=:51061                  # run as control plane
#   ./installer.sh lord --scheduler=<host>:51061             # register a lord
#   ./installer.sh tenant --scheduler=<host>:51061           # install CLI only
#   ./installer.sh token new    --scheduler=<host>:51061     # create tenant token
#   ./installer.sh status       --scheduler=<host>:51061     # show fleet
#
# Usage:
#   curl -fsSL https://github.com/midas/Etronium-Scdr/releases/latest/download/installer.sh \
#     | bash -s -- lord --scheduler=etronium.example.com:51061 --hostname=my-laptop
#
# This is the only thing a user needs to memorize.

set -euo pipefail

VERSION="${ETRONIUM_VERSION:-v0.1.0}"
INSTALL_DIR="${ETRONIUM_INSTALL_DIR:-/usr/local/bin}"
STATE_DIR="${ETRONIUM_STATE_DIR:-/var/lib/etronium}"
LOG_DIR="${ETRONIUM_LOG_DIR:-/var/log/etronium}"
SERVICE_USER="${ETRONIUM_USER:-etronium}"
GITHUB_BASE="${ETRONIUM_REPO_BASE:-https://github.com/midas/Etronium-Scdr/releases}"
ARCH="$(uname -m)"
case "$ARCH" in
    x86_64)  ARCH=amd64 ;;
    aarch64) ARCH=arm64 ;;
    *)       echo "Unsupported arch: $ARCH" >&2; exit 2 ;;
esac

bold() { printf "\033[1m%s\033[0m\n" "$*"; }
ok()   { printf "  \033[32m✓\033[0m %s\n" "$*"; }
fail() { printf "  \033[31m✗\033[0m %s\n" "$*" >&2; exit 1; }

# ─────────────────────────────── Detect / require root
if [[ "$(id -u)" -ne 0 ]]; then
    fail "installer requires root (use sudo)."
fi

# ─────────────────────────────── Subcommand dispatch
subcmd="${1:-help}"
shift || true

case "$subcmd" in
    scheduler|lord|tenant) cmd_install "$@" ;;
    token)                 cmd_token  "$@" ;;
    status)                cmd_status "$@" ;;
    update)                cmd_update "$@" ;;
    uninstall)             cmd_uninstall "$@" ;;
    help|--help|-h|"")     cmd_help ;;
    *)                     fail "unknown subcommand: $subcmd (try 'help')" ;;
esac

# ═══════════════════════════════ Implementation ═══════════════════════════════

cmd_help() {
    cat <<'EOF'

  Etronium installer — one-command bootstrap.

  Subcommands:
    scheduler   Install + run scheduler (control plane).
    lord        Install + run lord on this machine.
    tenant      Install tenant CLI only.
    token       Manage tenant access tokens (new|list|revoke).
    status      Show fleet status from scheduler.
    update      Upgrade an existing installation.
    uninstall   Remove Etronium from this machine.

  Common flags (where applicable):
    --scheduler=HOST:PORT    Scheduler gRPC address.
    --hostname=NAME          Lord hostname override.
    --advertise-cpu=N        NUMA-overcommit CPU shares (lord only).
    --advertise-mem=BYTES    NUMA-overcommit RAM bytes.
    --token=TOKEN            Tenant access token.
    --version=vX.Y.Z         Pin a release version.

  Example:
    curl -fsSL https://.../installer.sh | sudo bash -s -- \
      lord --scheduler=etronium.example.com:51061 --hostname=my-laptop --advertise-cpu=4

EOF
}

# ─────────────────────────────── Download + install binary
install_binary() {
    local bin_name="$1"
    local dest="$2"
    local archive="etronium-scdr-${VERSION}-linux-${ARCH}.tar.gz"
    local url="${GITHUB_BASE}/download/${VERSION}/${archive}"

    bold "  Downloading $bin_name from $url"
    local tmp
    tmp="$(mktemp -d)"
    trap "rm -rf '$tmp'" RETURN

    if ! curl -fsSL --max-time 60 "$url" -o "$tmp/$archive"; then
        fail "download failed. Check the URL/VERSION. Tried: $url"
    fi

    local sum
    sum="$(curl -fsSL --max-time 30 "${GITHUB_BASE}/download/${VERSION}/checksums.txt" \
        | awk -v a="$archive" '$2 == a { print $1 }')"
    if [[ -z "$sum" ]]; then
        echo "  ! WARNING: no checksum found for $archive (continuing without verification)"
    elif command -v sha256sum >/dev/null; then
        (cd "$tmp" && echo "$sum  $archive" | sha256sum -c - >/dev/null) \
            || fail "checksum verification failed — refusing to install"
        ok "checksum verified"
    fi

    tar -C "$tmp" -xzf "$tmp/$archive"
    install -m 0755 "$tmp/$bin_name" "$dest"
    ok "installed $bin_name → $dest"
}

ensure_dirs() {
    mkdir -p "$STATE_DIR" "$LOG_DIR"
}

ensure_user() {
    if ! id "$SERVICE_USER" >/dev/null 2>&1; then
        if command -v useradd >/dev/null; then
            useradd --system --home "$STATE_DIR" --shell /sbin/nologin "$SERVICE_USER"
        elif command -v adduser >/dev/null; then
            adduser --system --home "$STATE_DIR" --no-create-home --shell /sbin/nologin "$SERVICE_USER"
        fi
        ok "created service user: $SERVICE_USER"
    fi
}

# ─────────────────────────────── Install scheduler
cmd_install() {
    local role="${subcmd}"
    shift || true
    # Re-bind subcmd to role for the inner function.
    local SCHEDULER_ADDR="" HOSTNAME="" ADVERTISE_CPU=0 ADVERTISE_MEM=0 TOKEN=""
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --scheduler=*)   SCHEDULER_ADDR="${1#*=}" ;;
            --hostname=*)    HOSTNAME="${1#*=}" ;;
            --advertise-cpu=*) ADVERTISE_CPU="${1#*=}" ;;
            --advertise-mem=*) ADVERTISE_MEM="${1#*=}" ;;
            --token=*)       TOKEN="${1#*=}" ;;
            --version=*)     VERSION="${1#*=}" ;;
            --http=*)        HTTP_ADDR="${1#*=}" ;;   # scheduler only
            *)               echo "warning: ignored $1" >&2 ;;
        esac
        shift
    done

    bold "═══ Etronium installer v$VERSION ($role) ═══"

    install_binary "${role}" "${INSTALL_DIR}/etronium-${role}"
    # Tenant alias.
    if [[ "$role" == "scheduler" || "$role" == "lord" ]]; then
        ln -sf "${INSTALL_DIR}/etronium-${role}" "${INSTALL_DIR}/scheduler"
    fi
    if [[ "$role" == "etronium" || "$role" == "tenant" ]]; then
        install -m 0755 "${INSTALL_DIR}/etronium-${role}" "${INSTALL_DIR}/tenant"
        ok "tenant CLI: ${INSTALL_DIR}/tenant"
    fi

    ensure_dirs
    ensure_user

    case "$role" in
        scheduler) install_scheduler_systemd "$SCHEDULER_ADDR" "$HTTP_ADDR" ;;
        lord)      install_lord_systemd       "$SCHEDULER_ADDR" "$HOSTNAME" "$ADVERTISE_CPU" "$ADVERTISE_MEM" "$TOKEN" ;;
    esac

    bold "═══ Done ═══"
    cat <<EOF

  Etronium $role is installed.

  Binary:    ${INSTALL_DIR}/etronium-${role}
  State:     ${STATE_DIR}/
  Logs:      ${LOG_DIR}/etronium.log

  Useful commands:
    sudo systemctl status etronium-${role}
    sudo journalctl -u etronium-${role} -f
    sudo etronium-${role} --help

EOF
}

install_scheduler_systemd() {
    local sched_addr="${1:-}"
    local http_addr="${2:-:51061}"

    bold "  Generating systemd unit for scheduler (gRPC ${http_addr})"

    cat > /etc/systemd/system/etronium-scheduler.service <<EOF
[Unit]
Description=Etronium Scheduler (control plane + sched_ext loader)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
ExecStart=${INSTALL_DIR}/etronium-scheduler --addr=${http_addr} --log=info --log-format=json
Restart=always
RestartSec=10
Environment=SCHEDULER_LISTEN=${http_addr}
Environment=ETRONIUM_VERSION=${VERSION}
StateDirectory=${SERVICE_USER}
LogsDirectory=${SERVICE_USER}

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable --now etronium-scheduler.service
    ok "scheduler service started: etronium-scheduler.service"
    ok "listens on: ${http_addr}"
}

install_lord_systemd() {
    local sched_addr="${1:-}"
    local hostname="${2:-$(hostname -s)}"
    local cpu="${3:-0}"
    local mem="${4:-0}"
    local token="${5:-}"

    if [[ -z "$sched_addr" ]]; then
        fail "--scheduler is required for lord role."
    fi

    bold "  Generating systemd unit for lord → $sched_addr (hostname=$hostname)"

    local env_token=""
    [[ -n "$token" ]] && env_token="Environment=ETRONIUM_TOKEN=${token}"

    cat > /etc/systemd/system/etronium-lord.service <<EOF
[Unit]
Description=Etronium Lord (compute provider) → $sched_addr
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root                 # lord needs CAP_SYS_ADMIN for SCHED_EXT policy apply
ExecStart=${INSTALL_DIR}/etronium-lord \\
    --scheduler=${sched_addr} \\
    --hostname=${hostname} \\
    --advertise-cpu=${cpu} \\
    --advertise-mem=${mem} \\
    --log=info --log-format=json
Restart=always
RestartSec=10
Environment=ETRONIUM_SCHEDULER=${sched_addr}
Environment=ETRONIUM_VERSION=${VERSION}
${env_token}
LogsDirectory=${SERVICE_USER}

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable --now etronium-lord.service
    ok "lord service started: etronium-lord.service"
    ok "registered as hostname '$hostname' to $sched_addr"
}

# ─────────────────────────────── token subcommand
cmd_token() {
    shift
    local action="${1:-new}"; shift || true
    local SCHEDULER_ADDR="" TOKEN=""
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --scheduler=*)  SCHEDULER_ADDR="${1#*=}" ;;
            --token=*)      TOKEN="${1#*=}" ;;
            *)              ;;
        esac
        shift
    done
    if [[ -z "$SCHEDULER_ADDR" ]]; then
        fail "--scheduler is required for token subcommand."
    fi
    bold "═══ Etronium token ${action} ═══"
    case "$action" in
        new|list|revoke)
            installer_require_tenant_bin
            tenant token "$action" --scheduler="$SCHEDULER_ADDR" ${TOKEN:+--token="$TOKEN"} ;;
        *) fail "unknown token action: $action (new|list|revoke)" ;;
    esac
}

installer_require_tenant_bin() {
    if [[ ! -x "${INSTALL_DIR}/tenant" ]]; then
        # Auto-fetch tenant CLI for token operations.
        bold "  Downloading tenant CLI (first use)"
        install_binary "etronium" "${INSTALL_DIR}/etronium"
        install -m 0755 "${INSTALL_DIR}/etronium" "${INSTALL_DIR}/tenant"
        ok "tenant CLI installed: ${INSTALL_DIR}/tenant"
    fi
}

# ─────────────────────────────── status subcommand
cmd_status() {
    shift
    local SCHEDULER_ADDR=""
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --scheduler=*) SCHEDULER_ADDR="${1#*=}" ;;
        esac
        shift
    done
    if [[ -z "$SCHEDULER_ADDR" ]]; then
        fail "--scheduler=HOST:PORT is required."
    fi
    bold "═══ Etronium fleet status ═══"
    installer_require_tenant_bin
    tenant status --scheduler="$SCHEDULER_ADDR" || true
}

# ─────────────────────────────── update subcommand
cmd_update() {
    shift
    local ROLE="scheduler"
    case "${subcmd}" in
        update) ;;
        *) ;;
    esac
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --scheduler=*)   SCHEDULER_ADDR="${1#*=}" ;;
            --hostname=*)    HOSTNAME="${1#*=}" ;;
            --advertise-cpu=*) ADVERTISE_CPU="${1#*=}" ;;
            --advertise-mem=*) ADVERTISE_MEM="${1#*=}" ;;
            --token=*)       TOKEN="${1#*=}" ;;
            --version=*)     VERSION="${1#*=}" ;;
            --role=*)        ROLE="${1#*=}" ;;
            *) ;;
        esac
        shift
    done
    bold "═══ Updating Etronium ($ROLE) to $VERSION ═══"
    install_binary "${ROLE}" "${INSTALL_DIR}/etronium-${ROLE}"
    systemctl restart "etronium-${ROLE}.service" 2>/dev/null || true
    ok "updated: $ROLE → $VERSION"
}

# ─────────────────────────────── uninstall
cmd_uninstall() {
    shift
    bold "═══ Etronium uninstall ═══"
    local services=(etronium-scheduler etronium-lord)
    for svc in "${services[@]}"; do
        if systemctl list-unit-files "${svc}.service" >/dev/null 2>&1; then
            systemctl disable --now "${svc}.service" 2>/dev/null || true
            rm -f "/etc/systemd/system/${svc}.service"
        fi
    done
    systemctl daemon-reload
    rm -f "${INSTALL_DIR}/etronium-"* "${INSTALL_DIR}/scheduler" "${INSTALL_DIR}/lord" "${INSTALL_DIR}/tenant" \
          "${INSTALL_DIR}/etronium"
    ok "removed binaries + systemd units"
    if [[ -d "$STATE_DIR" ]]; then
        echo "  (kept $STATE_DIR — manually remove if you want a clean slate:)"
        echo "      sudo rm -rf $STATE_DIR"
    fi
}
