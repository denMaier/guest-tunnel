#!/usr/bin/env bash
# =============================================================================
# setup-tunnel.sh — Reverse SSH Tunnel Architecture Setup
# Roles: --vps | --homeserver | --client
# Ubuntu 22.04 | Idempotent | End-to-end encrypted (VPS is a dumb relay)
# =============================================================================

set -euo pipefail

# ── Colour helpers ────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; RESET='\033[0m'

info()    { echo -e "${CYAN}[INFO]${RESET}  $*"; }
success() { echo -e "${GREEN}[OK]${RESET}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${RESET}  $*"; }
error()   { echo -e "${RED}[ERROR]${RESET} $*" >&2; }
header()  { echo -e "\n${BOLD}${CYAN}══════════════════════════════════════════${RESET}"; \
            echo -e "${BOLD}${CYAN}  $*${RESET}"; \
            echo -e "${BOLD}${CYAN}══════════════════════════════════════════${RESET}"; }
die()     { error "$*"; exit 1; }

require_root() {
    [[ $EUID -eq 0 ]] || die "This role must be run as root (use sudo)."
}

prompt() {
    # prompt <varname> <message> [default]
    local _var=$1 _msg=$2 _default=${3:-}
    local _input
    if [[ -n $_default ]]; then
        read -rp "$(echo -e "${YELLOW}  >> ${RESET}${_msg} [${_default}]: ")" _input
        printf -v "$_var" '%s' "${_input:-$_default}"
    else
        read -rp "$(echo -e "${YELLOW}  >> ${RESET}${_msg}: ")" _input
        [[ -n $_input ]] || die "Required value not provided for: $_msg"
        printf -v "$_var" '%s' "$_input"
    fi
}

# ── Argument parsing ──────────────────────────────────────────────────────────
ROLE=""
case "${1:-}" in
    --vps)        ROLE="vps"        ;;
    --homeserver) ROLE="homeserver" ;;
    --client)     ROLE="client"     ;;
    *) die "Usage: $0 [--vps | --homeserver | --client]" ;;
esac

# =============================================================================
# ROLE: VPS
# =============================================================================
setup_vps() {
    require_root
    header "VPS Setup — Relay Node Configuration"

    # ── 1. Create jumpuser ────────────────────────────────────────────────────
    info "Checking for jumpuser..."
    if id jumpuser &>/dev/null; then
        success "jumpuser already exists — skipping creation."
    else
        useradd --system --shell /usr/sbin/nologin --create-home jumpuser
        success "Created jumpuser with /usr/sbin/nologin shell."
    fi

    # ── 2. Install laptop public key ──────────────────────────────────────────
    header "Laptop Public Key Installation"
    local auth_keys_dir="/home/jumpuser/.ssh"
    local auth_keys_file="${auth_keys_dir}/authorized_keys"
    install -d -m 0700 -o jumpuser -g jumpuser "$auth_keys_dir"

    local existing_key_count=0
    if [[ -f $auth_keys_file ]]; then
        existing_key_count=$(grep -c "^ssh-" "$auth_keys_file" 2>/dev/null || true)
    fi

    if [[ $existing_key_count -gt 0 ]]; then
        success "${existing_key_count} key(s) already in authorized_keys — skipping prompt."
        info "Existing keys:"
        grep "^ssh-" "$auth_keys_file" | while IFS= read -r k; do
            echo -e "    ${CYAN}$(echo "$k" | awk '{print $NF}')${RESET}"
        done
        echo ""
        read -rp "$(echo -e "${YELLOW}  >> ${RESET}Add another key? [y/N]: ")" _add_more
        if [[ "${_add_more,,}" != "y" ]]; then
            info "Skipping key installation."
        else
            local laptop_pubkey
            read -rp "$(echo -e "${YELLOW}  >> ${RESET}Public key: ")" laptop_pubkey
            [[ -n $laptop_pubkey ]] || die "No public key provided."
            if grep -qF "$laptop_pubkey" "$auth_keys_file" 2>/dev/null; then
                success "That key is already present — skipping."
            else
                echo "$laptop_pubkey" >> "$auth_keys_file"
                chown jumpuser:jumpuser "$auth_keys_file"
                chmod 0600 "$auth_keys_file"
                success "Additional public key installed for jumpuser."
            fi
        fi
    else
        info "No keys found — paste the laptop's SSH public key for jumpuser (single line, then Enter):"
        local laptop_pubkey
        read -rp "$(echo -e "${YELLOW}  >> ${RESET}Public key: ")" laptop_pubkey
        [[ -n $laptop_pubkey ]] || die "No public key provided."
        echo "$laptop_pubkey" >> "$auth_keys_file"
        chown jumpuser:jumpuser "$auth_keys_file"
        chmod 0600 "$auth_keys_file"
        success "Laptop public key installed for jumpuser."
    fi

    # ── 3. Harden sshd_config ─────────────────────────────────────────────────
    header "Hardening /etc/ssh/sshd_config"
    local sshd_conf="/etc/ssh/sshd_config"
    local backup="${sshd_conf}.bak.$(date +%Y%m%d%H%M%S)"
    cp "$sshd_conf" "$backup"
    info "Backed up sshd_config → $backup"

    # Ubuntu 22.04 ships /etc/ssh/sshd_config.d/50-cloud-init.conf which sets
    # PasswordAuthentication yes and silently overrides the main config file.
    local cloud_init_conf="/etc/ssh/sshd_config.d/50-cloud-init.conf"
    if [[ -f $cloud_init_conf ]]; then
        local cloud_backup="${cloud_init_conf}.bak.$(date +%Y%m%d%H%M%S)"
        cp "$cloud_init_conf" "$cloud_backup"
        echo "PasswordAuthentication no" > "$cloud_init_conf"
        success "Neutralised $cloud_init_conf (backed up → $cloud_backup)."
    fi

    # Neutralise any other drop-in that enables password auth
    local dropin
    for dropin in /etc/ssh/sshd_config.d/*.conf; do
        [[ -f $dropin ]] || continue
        [[ $dropin == "$cloud_init_conf" ]] && continue
        if grep -qiE "^[[:space:]]*PasswordAuthentication[[:space:]]+yes" "$dropin"; then
            warn "Found PasswordAuthentication yes in $dropin — overriding."
            sed -i -E \
                "s|^[[:space:]]*PasswordAuthentication[[:space:]]+yes|PasswordAuthentication no|Ig" \
                "$dropin"
            success "  Fixed $dropin"
        fi
    done

    # Helper: set or replace a top-level sshd directive (idempotent)
    _sshd_set() {
        local key=$1 val=$2
        if grep -qE "^#?[[:space:]]*${key}[[:space:]]" "$sshd_conf"; then
            sed -i -E "s|^#?[[:space:]]*${key}[[:space:]].*|${key} ${val}|" "$sshd_conf"
        else
            echo "${key} ${val}" >> "$sshd_conf"
        fi
    }

    _sshd_set PasswordAuthentication           no
    _sshd_set PermitRootLogin                  no
    _sshd_set ChallengeResponseAuthentication  no
    _sshd_set UsePAM                           yes
    success "Global sshd hardening applied."

    # jumpuser Match block — idempotent via marker
    # PermitListen (not PermitOpen) restricts inbound -R reverse tunnels.
    # PermitOpen restricts outbound -L tunnels and has no effect on -R.
    # ForceCommand /bin/false cleanly rejects any shell/exec request post-auth.
    local marker="# BEGIN jumpuser-tunnel-block"
    if grep -qF "$marker" "$sshd_conf"; then
        success "jumpuser Match block already present — skipping."
    else
        cat >> "$sshd_conf" <<'EOF'

# BEGIN jumpuser-tunnel-block
Match User jumpuser
    PasswordAuthentication no
    PubkeyAuthentication yes
    AllowAgentForwarding no
    AllowTcpForwarding remote
    X11Forwarding no
    PermitTTY no
    ForceCommand /bin/false
    PermitListen localhost:2222
# END jumpuser-tunnel-block
EOF
        success "jumpuser Match block written to sshd_config."
    fi

    # Validate config before restarting
    sshd -t || die "sshd_config validation failed — review $sshd_conf"

    # ── 4. fail2ban ───────────────────────────────────────────────────────────
    header "Installing and Configuring fail2ban"
    if ! dpkg -s fail2ban &>/dev/null; then
        apt-get update -qq
        apt-get install -y -qq fail2ban
        success "fail2ban installed."
    else
        success "fail2ban already installed — skipping."
    fi

    local f2b_local="/etc/fail2ban/jail.local"
    if [[ ! -f $f2b_local ]]; then
        cat > "$f2b_local" <<'EOF'
[DEFAULT]
bantime  = 3600
findtime = 600
maxretry = 5
backend  = systemd

[sshd]
enabled  = true
port     = ssh
logpath  = %(sshd_log)s
EOF
        success "fail2ban jail.local written."
    else
        success "jail.local already exists — not overwriting."
    fi

    systemctl enable --now fail2ban &>/dev/null
    success "fail2ban enabled and running."

    # ── 5. Restart sshd ───────────────────────────────────────────────────────
    systemctl restart sshd
    success "sshd restarted."

    # ── Summary ───────────────────────────────────────────────────────────────
    header "VPS Setup Complete — Summary"
    echo -e "${GREEN}Done:${RESET}"
    echo "  • jumpuser created with /usr/sbin/nologin shell"
    echo "  • Laptop public key installed in /home/jumpuser/.ssh/authorized_keys"
    echo "  • sshd hardened: no passwords, no root, jumpuser restricted to localhost:2222 reverse tunnel"
    echo "  • fail2ban active (ban 1 h after 5 failures in 10 min)"
    echo ""
    echo -e "${YELLOW}Next steps:${RESET}"
    echo "  1. Run this script with --homeserver on the home server."
    echo "  2. When prompted, install the tunnel service key into"
    echo "     /home/jumpuser/.ssh/authorized_keys on THIS VPS."
    echo "  3. Ensure port 22 (or your SSH port) is open in your firewall/security group."
}

# =============================================================================
# ROLE: HOMESERVER
# =============================================================================
setup_homeserver() {
    require_root
    header "Homeserver Setup — Persistent Reverse Tunnel"

    # ── 1. Detect if already configured; recover params if so ────────────────
    local service_file="/etc/systemd/system/reverse-tunnel.service"
    local key_file="/home/tunneluser/.ssh/tunnel_ed25519"
    local vps_host="" vps_user="" ssh_daemon="" openssh_port=""

    if [[ -f $service_file ]]; then
        info "Existing reverse-tunnel.service detected — reading parameters."
        # Parse ExecStart: autossh … -R 2222:localhost:22 jumpuser@vps_host
        local execstart
        execstart=$(grep "ExecStart=" "$service_file" | head -1 || true)
        vps_host=$(echo "$execstart" | grep -oP '(?<=@)\S+' | head -1 || true)
        vps_user=$(echo "$execstart" | grep -oP '\S+(?=@)' | tail -1 || true)
        if [[ -n $vps_host && -n $vps_user ]]; then
            success "Recovered VPS host: ${vps_user}@${vps_host}"
        else
            warn "Could not parse existing service file — will prompt for values."
            vps_host=""; vps_user=""
        fi
    fi

    # Detect active SSH daemon (OpenSSH vs Dropbear) and current listen port
    _detect_ssh_daemon() {
        if systemctl is-active --quiet dropbear-ssh 2>/dev/null; then
            ssh_daemon="dropbear"
            openssh_port=$(systemctl show dropbear-ssh --property=ExecStart \
                | grep -oP '(?<=-p )\d+' | head -1 || echo "22")
        elif systemctl is-active --quiet ssh 2>/dev/null \
          || systemctl is-active --quiet sshd 2>/dev/null \
          || systemctl is-active --quiet openssh-server 2>/dev/null; then
            ssh_daemon="openssh"
            openssh_port=$(ss -tlnp | grep -oP '(?<=:)\d+(?=.*sshd)' | head -1 || echo "22")
        else
            ssh_daemon="unknown"
            openssh_port="22"
        fi
    }
    _detect_ssh_daemon

    # ── 2. Gather parameters (skip if already recovered) ─────────────────────
    [[ -n $vps_host ]] || prompt vps_host "VPS hostname or IP"
    [[ -n $vps_user ]] || prompt vps_user "jumpuser username on VPS" "jumpuser"

    # ── 3. Create tunneluser ──────────────────────────────────────────────────
    info "Checking for tunneluser..."
    if id tunneluser &>/dev/null; then
        success "tunneluser already exists — skipping creation."
    else
        useradd --system --shell /usr/sbin/nologin --create-home tunneluser
        success "Created tunneluser."
    fi

    # ── 4. Generate ed25519 keypair ───────────────────────────────────────────
    local key_dir="/home/tunneluser/.ssh"
    install -d -m 0700 -o tunneluser -g tunneluser "$key_dir"

    if [[ -f $key_file ]]; then
        success "SSH keypair already exists at $key_file — skipping generation."
    else
        sudo -u tunneluser ssh-keygen -t ed25519 -f "$key_file" -N "" -C "tunneluser@homeserver"
        success "ed25519 keypair generated at $key_file"
    fi

    # ── 5. Install client's public key on homeserver ──────────────────────────
    # The laptop needs to be able to ssh into this machine to use the tunnel.
    # We ask for the user account that will be reached from the laptop.
    header "Client Public Key Installation"

    local client_user client_home
    prompt client_user "Username on this homeserver that the laptop will log in as" "$(logname 2>/dev/null || echo '')"
    client_home=$(getent passwd "$client_user" | cut -d: -f6) \
        || die "User '$client_user' not found on this system."

    local client_ak_dir="${client_home}/.ssh"
    local client_ak_file="${client_ak_dir}/authorized_keys"
    install -d -m 0700 -o "$client_user" -g "$client_user" "$client_ak_dir"

    local existing_client_keys=0
    if [[ -f $client_ak_file ]]; then
        existing_client_keys=$(grep -c "^ssh-" "$client_ak_file" 2>/dev/null || true)
    fi

    if [[ $existing_client_keys -gt 0 ]]; then
        success "${existing_client_keys} key(s) already in ${client_ak_file} — skipping prompt."
        info "Existing keys:"
        grep "^ssh-" "$client_ak_file" | while IFS= read -r k; do
            echo -e "    ${CYAN}$(echo "$k" | awk '{print $NF}')${RESET}"
        done
        echo ""
        read -rp "$(echo -e "${YELLOW}  >> ${RESET}Add another laptop key? [y/N]: ")" _add_more
        if [[ "${_add_more,,}" == "y" ]]; then
            local client_pubkey
            read -rp "$(echo -e "${YELLOW}  >> ${RESET}Laptop public key: ")" client_pubkey
            [[ -n $client_pubkey ]] || die "No public key provided."
            if grep -qF "$client_pubkey" "$client_ak_file" 2>/dev/null; then
                success "That key is already present — skipping."
            else
                echo "$client_pubkey" >> "$client_ak_file"
                chown "${client_user}:${client_user}" "$client_ak_file"
                chmod 0600 "$client_ak_file"
                success "Laptop public key added for ${client_user}."
            fi
        else
            info "Skipping key installation."
        fi
    else
        info "Paste the laptop's SSH public key for ${client_user} (single line, then Enter):"
        local client_pubkey
        read -rp "$(echo -e "${YELLOW}  >> ${RESET}Laptop public key: ")" client_pubkey
        [[ -n $client_pubkey ]] || die "No public key provided."
        echo "$client_pubkey" >> "$client_ak_file"
        chown "${client_user}:${client_user}" "$client_ak_file"
        chmod 0600 "$client_ak_file"
        success "Laptop public key installed for ${client_user}."
    fi

    # ── 6. Install tunnel public key on VPS ───────────────────────────────────
    header "Tunnel Public Key — Install on VPS"
    echo -e "${BOLD}This key must be in /home/${vps_user}/.ssh/authorized_keys on the VPS:${RESET}"
    echo ""
    echo -e "${GREEN}$(cat "${key_file}.pub")${RESET}"
    echo ""

    # Test auth using -N (no shell, no command request whatsoever) wrapped in
    # `timeout`. -N + SessionType=none means we never open a session channel,
    # so ForceCommand and PermitTTY are never reached. The connection
    # authenticates and then sits alive indefinitely — which is exactly what
    # `timeout 5` exploits: exit code 124 means the connection was alive and
    # well (auth succeeded), any other non-zero exit with "Permission denied"
    # in stderr means auth failed.
    _test_jumpuser_auth() {
        local _rc=0
        local _stderr
        _stderr=$(sudo -u tunneluser timeout 5 ssh \
            -N \
            -i "$key_file" \
            -o BatchMode=yes \
            -o ConnectTimeout=8 \
            -o StrictHostKeyChecking=accept-new \
            -o SessionType=none \
            "${vps_user}@${vps_host}" 2>&1) || _rc=$?

        if [[ $_rc -eq 124 ]]; then
            # timeout killed a live connection — auth succeeded
            return 0
        elif echo "$_stderr" | grep -qiE \
                "Permission denied|authentication failed|no supported auth"; then
            return 1
        elif echo "$_stderr" | grep -qiE \
                "Connection refused|No route|IDENTIFICATION HAS CHANGED|REMOTE HOST IDENTIFICATION"; then
            echo "$_stderr" >&2
            return 2
        else
            echo "$_stderr" >&2
            return 1
        fi
    }

    info "Testing key authentication to ${vps_user}@${vps_host}..."
    local _auth_result=0
    _test_jumpuser_auth || _auth_result=$?

    if [[ $_auth_result -eq 0 ]]; then
        success "Key authentication to VPS succeeded — ForceCommand fired correctly."
    elif [[ $_auth_result -eq 2 ]]; then
        die "Network or host-key error reaching ${vps_host}. Check connectivity and known_hosts."
    else
        warn "Key authentication failed — the tunnel key is NOT yet on the VPS."
        echo ""
        echo -e "  Copy the key above, then on the VPS run:"
        echo -e "  ${CYAN}echo '<pubkey>' >> /home/${vps_user}/.ssh/authorized_keys${RESET}"
        echo -e "  or re-run:  ${CYAN}sudo ./setup-tunnel.sh --vps${RESET}  and paste it when prompted."
        echo ""
        warn "Script paused. Install the key on the VPS, then press Enter to continue."
        read -rp "$(echo -e "${YELLOW}  >> ${RESET}Press Enter when the key is installed on the VPS...")"

        info "Re-testing key authentication..."
        _auth_result=0
        _test_jumpuser_auth || _auth_result=$?
        if [[ $_auth_result -eq 0 ]]; then
            success "Key authentication to VPS succeeded."
        else
            error "Key authentication still failing."
            error "Common causes:"
            error "  • Key not appended correctly (check for line breaks or extra spaces)"
            error "  • Wrong user — confirm jumpuser is '${vps_user}' on the VPS"
            error "  • authorized_keys permissions wrong on VPS (must be 0600, owned by ${vps_user})"
            error "  • VPS firewall blocking outbound connections from homeserver"
            die "Cannot proceed without working key auth. Fix the above and re-run --homeserver."
        fi
    fi

    # ── 7. Choose SSH daemon ──────────────────────────────────────────────────
    # Choose the SSH daemon for inbound connections on this homeserver.
    # The outbound tunnel always uses /usr/bin/ssh regardless of this choice.
    header "SSH Server Selection"
    echo "  Choose the SSH server to run on this homeserver:"
    echo "  ${BOLD}1) OpenSSH${RESET}  — standard, full-featured, larger codebase"
    echo "  ${BOLD}2) Dropbear${RESET} — minimal codebase (~10x smaller), separate zero-day pool"
    echo ""

    local use_dropbear=false


    # If Dropbear is already active, default to keeping it
    if [[ $ssh_daemon == "dropbear" ]]; then
        info "Dropbear is currently active on this system."
        read -rp "$(echo -e "${YELLOW}  >> ${RESET}Keep Dropbear? [Y/n]: ")" _keep_db
        if [[ "${_keep_db,,}" != "n" ]]; then
            use_dropbear=true
        fi
    else
        read -rp "$(echo -e "${YELLOW}  >> ${RESET}Choice [1/2, default 1]: ")" _daemon_choice
        if [[ "${_daemon_choice}" == "2" ]]; then
            use_dropbear=true
        fi
    fi

    if [[ $use_dropbear == true ]]; then

        info "Dropbear selected (inbound daemon only — tunnel uses OpenSSH client)."
    else
        info "OpenSSH selected."
    fi

    # ── 8. Install autossh ────────────────────────────────────────────────────
    header "Installing autossh"
    if ! command -v autossh &>/dev/null; then
        apt-get update -qq
        apt-get install -y -qq autossh
        success "autossh installed."
    else
        success "autossh already installed."
    fi

    # ── 9. Install / configure chosen SSH server ──────────────────────────────
    # Must complete before writing reverse-tunnel.service so that dbclient is
    # guaranteed present on disk before autossh tries to exec it. For Dropbear
    # this also performs the OpenSSH→Dropbear cutover — the listening daemon is
    # replaced but the already-established outbound tunnel connection is an
    # ESTAB socket and is unaffected by the daemon swap.
    if [[ $use_dropbear == true ]]; then
        _setup_dropbear_server
    else
        _ensure_openssh_server
    fi

    # ── 10. Pre-populate known_hosts for tunneluser ───────────────────────────
    info "Pre-populating known_hosts for ${vps_host}..."
    local known_hosts="${key_dir}/known_hosts"
    local scanned_key=""
    scanned_key=$(ssh-keyscan -T 10 "$vps_host" 2>/dev/null) || true
    if [[ -n $scanned_key ]]; then
        if ! grep -qF "$vps_host" "$known_hosts" 2>/dev/null; then
            echo "$scanned_key" >> "$known_hosts"
            chown tunneluser:tunneluser "$known_hosts"
            chmod 0644 "$known_hosts"
            success "VPS host key added to tunneluser's known_hosts."
        else
            success "VPS host key already in known_hosts."
        fi
    else
        warn "Could not reach $vps_host to scan host key. Add it manually."
    fi

    # ── 11. Write systemd service ─────────────────────────────────────────────
    # The tunnel client is always /usr/bin/ssh (OpenSSH) regardless of which
    # daemon listens on the homeserver. The listening daemon (Dropbear or
    # OpenSSH) is an inbound attack surface; the outbound tunnel client is not.
    # Using OpenSSH here avoids dbclient flag incompatibilities entirely.
    header "Writing reverse-tunnel.service"

    local new_service_content="[Unit]
Description=Persistent Reverse SSH Tunnel to VPS
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
User=tunneluser
Environment=AUTOSSH_GATETIME=0
Environment=AUTOSSH_PATH=/usr/bin/ssh
ExecStart=/usr/bin/autossh -M 0 -N -o ServerAliveInterval=30 -o ServerAliveCountMax=3 -o ExitOnForwardFailure=yes -o BatchMode=yes -o StrictHostKeyChecking=yes -i ${key_file} -R 2222:localhost:22 ${vps_user}@${vps_host}
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
"

    if [[ -f $service_file ]]; then
        local existing
        existing=$(cat "$service_file")
        if [[ "$existing" == "$new_service_content" ]]; then
            success "reverse-tunnel.service already up to date — skipping."
        else
            warn "Updating existing reverse-tunnel.service."
            printf '%s' "$new_service_content" > "$service_file"
            success "reverse-tunnel.service updated."
        fi
    else
        printf '%s' "$new_service_content" > "$service_file"
        success "reverse-tunnel.service written."
    fi

    # ── 12. Enable and start ──────────────────────────────────────────────────
    systemctl daemon-reload
    systemctl enable reverse-tunnel.service
    systemctl restart reverse-tunnel.service
    success "reverse-tunnel.service enabled and started."

    # ── 13. Verify ────────────────────────────────────────────────────────────
    header "Verifying Tunnel Service"
    sleep 3
    if systemctl is-active --quiet reverse-tunnel.service; then
        success "reverse-tunnel.service is ACTIVE."
    else
        error "reverse-tunnel.service is NOT active."
        journalctl -u reverse-tunnel.service -n 20 --no-pager || true
        die "Tunnel service failed to start. Check the logs above."
    fi

    # ── Summary ───────────────────────────────────────────────────────────────
    header "Homeserver Setup Complete — Summary"
    echo -e "${GREEN}Done:${RESET}"
    echo "  • tunneluser created (unprivileged, nologin shell)"
    echo "  • ed25519 keypair: ${key_file}"
    echo "  • Laptop public key installed for ${client_user}"
    echo "  • autossh installed"
    echo "  • SSH client in tunnel: /usr/bin/ssh (OpenSSH)"
    echo "  • reverse-tunnel.service: active, enabled at boot"
    echo "  • Tunnel: homeserver:22 → ${vps_host}:localhost:2222"
    echo ""
    echo -e "${YELLOW}Next steps:${RESET}"
    echo "  1. Run this script with --client on your laptop."
    echo "  2. Verify from the VPS: ssh -p 2222 ${client_user}@localhost"
    echo "  3. Monitor tunnel health: journalctl -u reverse-tunnel.service -f"
}

# =============================================================================
# HOMESERVER HELPER: Ensure OpenSSH is installed and running
# =============================================================================
_ensure_openssh_server() {
    if ! dpkg -s openssh-server &>/dev/null; then
        info "OpenSSH server not installed — installing..."
        apt-get update -qq
        apt-get install -y -qq openssh-server
        success "openssh-server installed."
    else
        success "openssh-server already installed."
    fi

    # Unmask in case a previous Dropbear run masked it
    systemctl unmask ssh openssh-server 2>/dev/null || true
    systemctl enable --now ssh 2>/dev/null \
        || systemctl enable --now openssh-server 2>/dev/null \
        || true
    success "OpenSSH server is active."
}

# =============================================================================
# HOMESERVER HELPER: Install Dropbear, convert keys, write service, test & switch
# =============================================================================
_setup_dropbear_server() {
    header "Installing Dropbear SSH Server"

    # ── Install ───────────────────────────────────────────────────────────────
    if dpkg -s dropbear-bin &>/dev/null; then
        success "Dropbear already installed."
    else
        apt-get update -qq
        apt-get install -y -qq dropbear-bin
        success "Dropbear installed (dropbear-bin)."
    fi

    # Confirm dbclient is available
    command -v dbclient &>/dev/null \
        || die "dbclient not found after installing dropbear-bin. Aborting."

    # ── Verify authorized_keys permissions ────────────────────────────────────
    info "Verifying authorized_keys permissions for all users..."
    local migrated=0
    while IFS=: read -r uname _ uid _ _ homedir _; do
        [[ $uid -ge 1000 || $uname == "tunneluser" ]] || continue
        local ak="${homedir}/.ssh/authorized_keys"
        if [[ -f $ak ]]; then
            chmod 0600 "$ak"
            chown "${uname}:${uname}" "$ak" 2>/dev/null || true
            success "  ${uname}: $ak permissions verified."
            (( migrated++ )) || true
        fi
    done < /etc/passwd
    [[ $migrated -gt 0 ]] || warn "No authorized_keys files found — ensure keys are installed."

    # ── Convert host key ──────────────────────────────────────────────────────
    header "Converting Host Key to Dropbear Format"
    local db_key_dir="/etc/dropbear"
    install -d -m 0755 "$db_key_dir"

    local openssh_ed25519="/etc/ssh/ssh_host_ed25519_key"
    local db_ed25519="${db_key_dir}/dropbear_ed25519_host_key"

    if [[ -f $db_ed25519 ]]; then
        success "Dropbear ed25519 host key already exists — skipping conversion."
    elif [[ -f $openssh_ed25519 ]]; then
        dropbearconvert openssh dropbear "$openssh_ed25519" "$db_ed25519"
        chmod 0600 "$db_ed25519"
        success "Converted OpenSSH ed25519 host key → Dropbear format."
        info "Host key fingerprint unchanged — clients will not see a TOFU warning."
    else
        dropbearkey -t ed25519 -f "$db_ed25519"
        chmod 0600 "$db_ed25519"
        warn "No OpenSSH ed25519 key found — generated a fresh Dropbear host key."
        warn "Clients will see a host-key-changed warning. Run: ssh-keygen -R <homeserver-ip>"
    fi

    # Determine which port to bind (use current OpenSSH port if known)
    local db_port=${openssh_port:-22}
    prompt db_port "Port for Dropbear to listen on" "$db_port"
    [[ $db_port =~ ^[0-9]+$ ]] || die "Port must be a number."

    # ── Write Dropbear systemd unit ───────────────────────────────────────────
    header "Writing dropbear-ssh.service"
    local db_service="/etc/systemd/system/dropbear-ssh.service"
    local db_service_content="[Unit]
Description=Dropbear SSH daemon (homeserver)
After=network.target
Documentation=man:dropbear(8)

[Service]
ExecStart=/usr/sbin/dropbear -F -E -p ${db_port} -r ${db_ed25519}
Restart=on-failure
RestartSec=5
KillMode=process

[Install]
WantedBy=multi-user.target
"
    # -F : foreground (systemd manages the process)
    # -E : log to stderr (journald captures it)
    # -p : listen port
    # -r : explicit host key (ed25519 only — no legacy RSA/DSA)

    if [[ -f $db_service ]]; then
        local existing_db
        existing_db=$(cat "$db_service")
        if [[ "$existing_db" == "$db_service_content" ]]; then
            success "dropbear-ssh.service already up to date — skipping."
        else
            warn "Updating existing dropbear-ssh.service."
            echo "$db_service_content" > "$db_service"
        fi
    else
        echo "$db_service_content" > "$db_service"
        success "dropbear-ssh.service written."
    fi
    systemctl daemon-reload

    # If Dropbear is already the active daemon on the correct port, we're done
    if systemctl is-active --quiet dropbear-ssh 2>/dev/null; then
        success "dropbear-ssh.service already active — reloading."
        systemctl restart dropbear-ssh
        return 0
    fi

    # ── Test Dropbear on a temporary port before cutting over ─────────────────
    local test_port=2223
    header "Testing Dropbear on temporary port ${test_port}"
    info "Starting Dropbear on port ${test_port} alongside OpenSSH for verification..."

    pkill -f "dropbear.*-p ${test_port}" 2>/dev/null || true
    sleep 1

    /usr/sbin/dropbear -p "$test_port" -r "$db_ed25519" -F -E &
    local db_test_pid=$!
    sleep 2

    if ! kill -0 "$db_test_pid" 2>/dev/null; then
        die "Dropbear failed to start on port ${test_port}. Check: journalctl -xe"
    fi
    success "Dropbear test instance running on port ${test_port} (PID ${db_test_pid})."

    echo ""
    echo -e "${BOLD}${YELLOW}ACTION REQUIRED — verify Dropbear before continuing:${RESET}"
    echo ""
    echo "  From your LAPTOP (in a separate terminal), run:"
    echo -e "  ${CYAN}ssh -p ${test_port} <your-user>@<homeserver-ip>${RESET}"
    echo ""
    echo "  If login succeeds, return here and press Enter."
    echo "  If login fails, press Ctrl+C to abort — OpenSSH will remain untouched."
    echo ""
    read -rp "$(echo -e "${YELLOW}  >> ${RESET}Press Enter ONLY after successful Dropbear login on port ${test_port}...")"

    kill "$db_test_pid" 2>/dev/null || true
    wait "$db_test_pid" 2>/dev/null || true
    success "Test instance stopped."

    # ── Cut over: stop OpenSSH, start Dropbear on real port ──────────────────
    header "Switching from OpenSSH to Dropbear on port ${db_port}"

    local openssh_unit=""
    for _unit in ssh openssh openssh-server sshd; do
        if systemctl is-active --quiet "$_unit" 2>/dev/null; then
            openssh_unit="$_unit"
            break
        fi
    done
    [[ -n $openssh_unit ]] || warn "Could not identify active OpenSSH unit — attempting common names."

    info "Stopping and disabling OpenSSH (${openssh_unit:-ssh/openssh-server})..."
    # Ubuntu 22.04+ ships ssh.socket for systemd socket activation — it holds
    # the listening socket independently of ssh.service and must be stopped
    # explicitly, otherwise port ${db_port} never releases even after the
    # service unit stops.
    systemctl disable --now ssh.socket      2>/dev/null || true
    systemctl disable --now openssh.socket  2>/dev/null || true
    systemctl disable --now "${openssh_unit:-ssh}"  2>/dev/null \
        || systemctl disable --now openssh-server   2>/dev/null \
        || systemctl disable --now sshd             2>/dev/null \
        || true

    # Wait for the listening socket to be released. We use `ss -tlnp` which
    # only shows LISTEN state — this deliberately ignores already-established
    # outbound connections (e.g. the reverse tunnel) which share the same port
    # number but are not listening and do not prevent Dropbear from binding.
    local port_wait=0
    while ss -tlnp | awk '{print $4}' | grep -qE ":${db_port}$" \
            && [[ $port_wait -lt 10 ]]; do
        sleep 1; (( port_wait++ )) || true
    done
    if ss -tlnp | awk '{print $4}' | grep -qE ":${db_port}$"; then
        warn "Port ${db_port} still in use (LISTEN) after stopping OpenSSH — rolling back."
        systemctl enable --now "${openssh_unit:-ssh}" 2>/dev/null || true
        die "Could not free port ${db_port}. OpenSSH restarted. No changes made."
    fi
    success "OpenSSH stopped — port ${db_port} is free."

    info "Starting dropbear-ssh.service on port ${db_port}..."
    systemctl enable dropbear-ssh.service
    systemctl start  dropbear-ssh.service
    sleep 2

    if systemctl is-active --quiet dropbear-ssh.service; then
        success "dropbear-ssh.service is ACTIVE on port ${db_port}."
    else
        error "Dropbear failed to start on port ${db_port}!"
        journalctl -u dropbear-ssh.service -n 30 --no-pager || true
        warn "Rolling back: restarting OpenSSH..."
        systemctl disable dropbear-ssh.service 2>/dev/null || true
        systemctl enable --now "${openssh_unit:-ssh}"    2>/dev/null \
            || systemctl enable --now openssh-server     2>/dev/null \
            || true
        die "Dropbear failed. OpenSSH has been restarted. No changes made."
    fi

    # Mask OpenSSH service and socket units so neither can be accidentally started
    systemctl mask ssh ssh.socket openssh-server openssh.socket sshd 2>/dev/null || true
    success "OpenSSH units masked — cannot be accidentally re-enabled."

    echo ""
    echo -e "${BOLD}Security diversity achieved:${RESET}"
    echo "  VPS        → OpenSSH  (needs Match block expressiveness)"
    echo "  Homeserver → Dropbear (separate codebase, ~10x smaller attack surface)"
    echo "  A zero-day in OpenSSH does not automatically compromise the homeserver."
    echo ""
    echo -e "${YELLOW}Rollback (if ever needed):${RESET}"
    echo "  systemctl unmask ssh ssh.socket"
    echo "  systemctl enable --now ssh"
    echo "  systemctl disable --now dropbear-ssh"
}

# =============================================================================
# ROLE: CLIENT (LAPTOP)
# =============================================================================
setup_client() {
    # Does NOT require root
    header "Client (Laptop) Setup — SSH Config and Tunnel Wrappers"

    # ── 1. Require SSH agent ──────────────────────────────────────────────────
    header "SSH Agent Check"
    if [[ -z "${SSH_AUTH_SOCK:-}" ]]; then
        die "SSH_AUTH_SOCK is not set — no SSH agent is running.\n\n" \
            "  Start one and load your key first:\n" \
            "    eval \"\$(ssh-agent -s)\"\n" \
            "    ssh-add ~/.ssh/id_ed25519\n" \
            "  Then re-run this script."
    fi
    local loaded_keys
    loaded_keys=$(ssh-add -l 2>/dev/null | grep -c "^[0-9]" || true)
    if [[ $loaded_keys -eq 0 ]]; then
        die "SSH agent is running but has no keys loaded.\n\n" \
            "  Load your key first:\n" \
            "    ssh-add ~/.ssh/id_ed25519\n" \
            "  Then re-run this script."
    fi
    success "SSH agent running with ${loaded_keys} key(s) loaded."

    # ── 2. Gather parameters ──────────────────────────────────────────────────
    local vps_host vps_user homeserver_user forward_port
    local ssh_dir="$HOME/.ssh"
    local ssh_config="$ssh_dir/config"

    # If config already exists, try to recover params from it
    local marker_begin="# BEGIN reverse-tunnel-config"
    if grep -qF "$marker_begin" "$ssh_config" 2>/dev/null; then
        info "Existing reverse-tunnel-config block detected — reading parameters."
        vps_host=$(awk '/^Host vps$/,/^Host /' "$ssh_config" \
            | grep -i "HostName" | awk '{print $2}' | head -1 || true)
        vps_user=$(awk '/^Host vps$/,/^Host /' "$ssh_config" \
            | grep -i "^ *User " | awk '{print $2}' | head -1 || true)
        homeserver_user=$(awk '/^Host homeserver$/,/^Host /' "$ssh_config" \
            | grep -i "^ *User " | awk '{print $2}' | head -1 || true)
        forward_port=$(grep -A2 "opencode-tunnel" "$HOME/bin/opencode-tunnel" 2>/dev/null \
            | grep "^PORT=" | cut -d= -f2 || true)
        if [[ -n $vps_host && -n $vps_user && -n $homeserver_user && -n $forward_port ]]; then
            success "Recovered: ${vps_user}@${vps_host}, homeserver user: ${homeserver_user}, port: ${forward_port}"
            echo ""
            read -rp "$(echo -e "${YELLOW}  >> ${RESET}Re-use these settings? [Y/n]: ")" _reuse
            if [[ "${_reuse,,}" == "n" ]]; then
                vps_host=""; vps_user=""; homeserver_user=""; forward_port=""
            fi
        else
            warn "Could not fully parse existing config — will prompt for values."
            vps_host=""; vps_user=""; homeserver_user=""; forward_port=""
        fi
    fi

    [[ -n $vps_host        ]] || prompt vps_host        "VPS hostname or IP"
    [[ -n $vps_user        ]] || prompt vps_user        "jumpuser username on VPS" "jumpuser"
    [[ -n $homeserver_user ]] || prompt homeserver_user "Your username on the homeserver"
    [[ -n $forward_port    ]] || prompt forward_port    "Local port to forward for OpenCode" "3000"
    [[ $forward_port =~ ^[0-9]+$ ]] || die "Port must be a number."

    # ── 3. Back up and write ~/.ssh/config ────────────────────────────────────
    header "Writing ~/.ssh/config"
    install -d -m 0700 "$ssh_dir"

    if [[ -f $ssh_config ]]; then
        local backup="${ssh_config}.bak.$(date +%Y%m%d%H%M%S)"
        cp "$ssh_config" "$backup"
        info "Existing config backed up → $backup"
    fi

    local marker_end="# END reverse-tunnel-config"
    if grep -qF "$marker_begin" "$ssh_config" 2>/dev/null; then
        sed -i "/${marker_begin}/,/${marker_end}/d" "$ssh_config"
        info "Replaced existing reverse-tunnel-config block."
    fi

    local new_block="${marker_begin}
Host vps
    HostName ${vps_host}
    User ${vps_user}
    ForwardAgent no
    ServerAliveInterval 30
    ServerAliveCountMax 3

Host homeserver
    HostName localhost
    Port 2222
    User ${homeserver_user}
    ProxyJump vps
    ForwardAgent no
    ServerAliveInterval 30
    ServerAliveCountMax 3
${marker_end}"

    echo "" >> "$ssh_config"
    echo "$new_block" >> "$ssh_config"
    chmod 0600 "$ssh_config"
    success "~/.ssh/config updated."

    # ── 4. Write ~/bin/opencode-tunnel ────────────────────────────────────────
    header "Writing ~/bin/opencode-tunnel"
    local bin_dir="$HOME/bin"
    local wrapper="$bin_dir/opencode-tunnel"
    mkdir -p "$bin_dir"

    cat > "$wrapper" <<WRAPPER_EOF
#!/usr/bin/env bash
# opencode-tunnel — Forward homeserver:${forward_port} → localhost:${forward_port}
# Generated by setup-tunnel.sh

set -euo pipefail

PORT=${forward_port}
BOLD='\033[1m'; GREEN='\033[0;32m'; RESET='\033[0m'
_cleanup() { echo -e "\n\${BOLD}Tunnel closed.\${RESET}"; exit 0; }
trap _cleanup INT TERM

echo -e "\${GREEN}\${BOLD}Starting tunnel: homeserver:\${PORT} → localhost:\${PORT}\${RESET}"
echo -e "\${BOLD}OpenCode will be available at: http://localhost:\${PORT}\${RESET}"
echo    "(Press Ctrl+C to close the tunnel)"
echo

ssh -N -L "\${PORT}:localhost:\${PORT}" homeserver &
SSH_PID=$!

verify_tunnel() {
    local port=$1
    local max_attempts=${2:-15}
    for i in $(seq 1 $max_attempts); do
        if curl -s -o /dev/null -m 2 "http://localhost:${port}" 2>/dev/null; then
            return 0
        fi
        sleep 1
    done
    return 1
}

if verify_tunnel "${PORT}"; then
    echo -e "${GREEN}✓ Tunnel is up — OpenCode available at http://localhost:${PORT}${RESET}"
else
    echo -e "${YELLOW}✗ Tunnel verification failed — SSH may have failed. Check with: ssh -v -L ${PORT}:localhost:${PORT} homeserver${RESET}"
    exit 1
fi

wait "\$SSH_PID"
WRAPPER_EOF

    chmod +x "$wrapper"
    success "~/bin/opencode-tunnel written and made executable."

    # ── 5. Write ~/bin/homelab-tunnel (SOCKS5 only — no browser management) ──
    header "Writing ~/bin/homelab-tunnel"
    local homelab_wrapper="$bin_dir/homelab-tunnel"
    local socks_port=1080

    cat > "$homelab_wrapper" <<HOMELAB_EOF
#!/usr/bin/env bash
# homelab-tunnel — SOCKS5 tunnel via homeserver.
#
# Security model:
#   laptop ──[ed25519]──▶ VPS:22 (dumb relay) ──▶ homeserver:2222
#   VPS sees only opaque ciphertext. No sudo. No routing table changes.
#
# Generated by setup-tunnel.sh

set -euo pipefail

SOCKS_PORT=${socks_port}
BOLD='\033[1m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; RESET='\033[0m'

SSH_PID=""

_cleanup() {
    echo -e "\n\${BOLD}Closing homelab tunnel...\${RESET}"
    [[ -n \$SSH_PID ]] && kill "\$SSH_PID" 2>/dev/null || true
    echo -e "\${BOLD}SOCKS5 tunnel closed.\${RESET}"
    exit 0
}
trap _cleanup INT TERM

echo -e "\${GREEN}\${BOLD}Opening SOCKS5 tunnel → homeserver (localhost:\${SOCKS_PORT})\${RESET}"
ssh -N -D "\${SOCKS_PORT}" homeserver &
SSH_PID=$!

verify_socks() {
    local socks_port=$1
    local max_attempts=${2:-20}
    for i in $(seq 1 $max_attempts); do
        if curl --socks5-hostname 127.0.0.1:${socks_port} -m 2 -s -o /dev/null http://example.com 2>/dev/null; then
            return 0
        fi
        echo -n "."
        sleep 1
    done
    return 1
}

echo -n "  Waiting for tunnel"
if verify_socks "${SOCKS_PORT}"; then
    echo -e " \${GREEN}✓\${RESET}"
else
    echo ""
    echo -e "${YELLOW}[WARN]\${RESET}  Tunnel did not become ready — SSH may have failed." >&2
    echo -e "${YELLOW}[HINT]\${RESET}  Check with: ssh -v -D ${SOCKS_PORT} homeserver" >&2
    exit 1
fi

echo -e "\${GREEN}✓ SOCKS5 tunnel up on localhost:\${SOCKS_PORT}\${RESET}"
echo ""
echo -e "\${CYAN}DNS resolves on homeserver — .home/.lan/.internal domains work.\${RESET}"
echo -e "\${CYAN}Only configure the browser you want to proxy — all other apps are unaffected.\${RESET}"
echo    "(Press Ctrl+C to close tunnel)"
echo

wait "\$SSH_PID"
HOMELAB_EOF

    chmod +x "$homelab_wrapper"
    success "~/bin/homelab-tunnel written and made executable."

    # ── 6. Print browser configuration instructions ───────────────────────────
    header "Browser SOCKS5 Configuration"
    echo -e "After running ${BOLD}homelab-tunnel${RESET}, configure your browser to use the SOCKS5 proxy."
    echo ""
    echo -e "${BOLD}${CYAN}Firefox / Firefox-based browsers (Librewolf, Zen, Floorp, etc.)${RESET}"
    echo "  Option A — Dedicated profile (recommended: keeps homelab traffic isolated):"
    echo "    1. Create a new profile:"
    echo -e "       ${CYAN}firefox --new-instance -P${RESET}  → click 'Create Profile', name it 'homelab'"
    echo "    2. Open that profile and go to:"
    echo -e "       ${CYAN}about:preferences#general${RESET}  → scroll to 'Network Settings' → Settings…"
    echo "    3. Select 'Manual proxy configuration':"
    echo "       SOCKS Host: 127.0.0.1    Port: ${socks_port}"
    echo "       SOCKS v5 ✓    Proxy DNS over SOCKS5 ✓"
    echo "       No proxy for: (leave blank)"
    echo "    4. Click OK. DNS now resolves on the homeserver."
    echo "       .home / .lan / .internal reverse-proxy domains will work."
    echo ""
    echo "  Option B — Temporary (current profile, current session only):"
    echo -e "    Open ${CYAN}about:config${RESET} and set:"
    echo "      network.proxy.type             → 1"
    echo "      network.proxy.socks            → 127.0.0.1"
    echo "      network.proxy.socks_port       → ${socks_port}"
    echo "      network.proxy.socks_version    → 5"
    echo "      network.proxy.socks_remote_dns → true"
    echo ""
    echo -e "${BOLD}${CYAN}Chromium / Chrome / Edge / Brave${RESET}"
    echo "  Chromium-based browsers only support proxy via system settings or launch flags."
    echo "  Launch with flags (creates an isolated instance, does not affect your normal browser):"
    echo -e "  ${CYAN}chromium --proxy-server='socks5://127.0.0.1:${socks_port}' \\"
    echo -e "           --host-resolver-rules='MAP * ~NOTFOUND, EXCLUDE 127.0.0.1' \\"
    echo -e "           --user-data-dir=/tmp/homelab-chrome${RESET}"
    echo ""
    echo "  Replace 'chromium' with 'google-chrome', 'brave-browser', or 'msedge' as appropriate."
    echo "  --user-data-dir keeps this instance's data separate from your normal profile."
    echo ""
    echo -e "${BOLD}${CYAN}curl / wget (for testing)${RESET}"
    echo -e "  ${CYAN}curl --socks5-hostname 127.0.0.1:${socks_port} http://myservice.home${RESET}"
    echo ""

    # Ensure ~/bin is on PATH
    if ! echo "$PATH" | tr ':' '\n' | grep -qx "$HOME/bin"; then
        warn "~/bin is not in your PATH."
        warn "Add this to your ~/.bashrc or ~/.zshrc:"
        echo -e "    ${CYAN}export PATH=\"\$HOME/bin:\$PATH\"${RESET}"
    fi

    # ── 7. Test VPS connectivity ──────────────────────────────────────────────
    header "Testing VPS Connectivity"
    info "Attempting: ssh -o BatchMode=yes -o ConnectTimeout=5 ${vps_user}@${vps_host} true"
    if ssh -o BatchMode=yes -o ConnectTimeout=5 "${vps_user}@${vps_host}" true 2>/dev/null; then
        success "VPS hop successful — ${vps_user}@${vps_host} is reachable."
    else
        warn "VPS hop FAILED. Possible causes:"
        warn "  • Public key not yet installed on the VPS for jumpuser"
        warn "  • Firewall blocking port 22 to the VPS"
        warn "  • SSH agent key does not match any key on the VPS"
        warn "  You can retry manually: ssh ${vps_user}@${vps_host}"
    fi

    # ── Summary ───────────────────────────────────────────────────────────────
    header "Client Setup Complete — Summary"
    echo -e "${GREEN}Done:${RESET}"
    echo "  • ~/.ssh/config updated with 'vps' and 'homeserver' blocks (ForwardAgent no)"
    echo "  • ~/bin/opencode-tunnel  — forwards port ${forward_port} only"
    echo "  • ~/bin/homelab-tunnel   — SOCKS5 proxy on localhost:${socks_port}"
    echo ""
    echo -e "${BOLD}SSH config written:${RESET}"
    echo -e "${CYAN}────────────────────────────────────────────${RESET}"
    echo "$new_block"
    echo -e "${CYAN}────────────────────────────────────────────${RESET}"
    echo ""
    echo -e "${YELLOW}Usage:${RESET}"
    echo "  Shell access to homeserver:  ssh homeserver"
    echo "  OpenCode port forward:       opencode-tunnel    → http://localhost:${forward_port}"
    echo "  Full homelab SOCKS5 proxy:   homelab-tunnel     → configure browser as shown above"
    echo ""
    echo -e "  ${BOLD}Architecture:${RESET}"
    echo "  laptop → (E2E encrypted) → VPS:22 (dumb relay) → homeserver:2222"
    echo "  VPS sees only opaque ciphertext. Keys never leave your devices. No sudo ever."
}

# =============================================================================
# Dispatch
# =============================================================================
case "$ROLE" in
    vps)        setup_vps        ;;
    homeserver) setup_homeserver ;;
    client)     setup_client     ;;
esac
