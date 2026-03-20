# guest-tunnel

Authenticated SOCKS5 tunnel to your homelab from any borrowed machine — no sudo, no pre-installed libraries, just a YubiKey.

## How it works

1. You `curl` a single pre-compiled binary (no install, runs from `/tmp`)
2. The binary starts a private `ssh-agent` in memory
3. A bundled `fido2-agent` helper starts a private `ssh-agent` and uses `ssh-add -K` to load your YubiKey's resident FIDO2 key into it (never written to disk)
4. SSH authenticates through two independent gates — VPS jump host, then homeserver — each requiring the FIDO2 key
5. A SOCKS5 proxy comes up on `localhost:1080`
6. On exit, the agent is killed and all temp files are wiped

A compromised VPS cannot reach the homeserver: the homeserver gate verifies the FIDO2 key independently, and the public key alone is insufficient to authenticate (the YubiKey must sign the challenge).

## Architecture

The same binary runs in three modes:

| Mode | Command | Where to run |
|------|---------|--------------|
| Server | `sudo guest-tunnel --mode=server` | VPS (jump host) |
| Home | `sudo guest-tunnel --mode=home` | Homeserver |
| Client | `guest-tunnel --mode=client` | Borrowed machine |

## Quick start

### 1. Setup VPS (run once)

```bash
sudo guest-tunnel --mode=server
```

This creates the `jumpuser`, hardens SSH, and installs fail2ban.

### 2. Setup Homeserver (run once)

```bash
sudo guest-tunnel --mode=home
```

This creates `tunneluser`, generates an SSH key, installs autossh, and runs the reverse tunnel as a systemd service.

### 3. Connect from borrowed machine

```bash
# Detect platform and download the right binary
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
VERSION=v1.0.0   # or check releases page

curl -fsSL \
  https://github.com/yourusername/guest-tunnel/releases/download/${VERSION}/guest-tunnel-${OS}-${ARCH} \
  -o /tmp/guest-tunnel

chmod +x /tmp/guest-tunnel
/tmp/guest-tunnel --mode=client --yubikey
```

Before you run it, open the GitHub release page in your browser, copy the SHA256 shown for the exact binary you downloaded, and compare it locally:

```bash
shasum -a 256 /tmp/guest-tunnel
```

If the hash in your browser does not match the hash of the file you downloaded, do not run it.

Insert your YubiKey, let the helper load resident keys, and touch it when prompted. Done.

## Requirements on the borrowed machine

| Requirement | Notes |
|---|---|
| `ssh`, `ssh-agent`, `ssh-add` | Used for the tunnel and loaded by the bundled helper |
| YubiKey (FIDO2, resident key enrolled) | See enrollment below |
| No sudo needed | Everything runs in userspace |

## fido2-agent helper

### What it does

`fido2-agent` is a small Go binary that:

1. Starts a private `ssh-agent` with a known socket path
2. Runs `ssh-add -K` to load resident FIDO2 keys from your YubiKey into that agent
3. Keeps the agent alive until you exit (Ctrl+C or when guest-tunnel shuts down)

It uses whatever `ssh-agent` and `ssh-add` are available on the system — it does not bundle or require a custom SSH build.

### How guest-tunnel finds it

When `--yubikey` is set, guest-tunnel looks for `fido2-agent` in this order:

1. `GUEST_TUNNEL_FIDO2_AGENT` env var
2. Next to the `guest-tunnel` binary
3. On `$PATH`

If no `fido2-agent` can be found, guest-tunnel exits with a clear error.

### Build it locally

```bash
make build
# Output: dist/fido2-agent
```

## Server setup

### VPS (`/etc/ssh/sshd_config` additions)

```
Match User jumpuser
    AuthenticationMethods publickey
    PubkeyAuthentication yes
    # One user handles both flows:
    # - homeserver reverse tunnel (-R <tunnel_port>:localhost:22)
    # - client jump hop (-W localhost:<tunnel_port>)
    AllowTcpForwarding yes
    PermitOpen localhost:2222
    PermitListen localhost:2222
    X11Forwarding no
    PermitTTY no
```

Add the YubiKey's FIDO2 public key to both `~jumpuser/.ssh/authorized_keys` and `~tunneluser/.ssh/authorized_keys`:

```
sk-ssh-ed25519@openssh.com AAAA... resident-key
```

### Homeserver (`/etc/ssh/sshd_config` additions)

```
Match User tunneluser
    AuthenticationMethods publickey
    PubkeyAuthentication yes
    AllowTcpForwarding yes     # needed for SOCKS5 -D
    X11Forwarding no
    PermitTTY no
    ForceCommand /bin/false
```

### YubiKey enrollment (run once, on a trusted machine with libfido2)

Generate a single resident FIDO2 key that will work for both gates:

```bash
ssh-keygen -t ed25519-sk -O resident -O application=ssh:tunnel \
  -C "guest-tunnel" -f /tmp/guest-tunnel

# Add to VPS (jump host)
cat /tmp/guest-tunnel.pub >> ~jumpuser/.ssh/authorized_keys

# Add to homeserver
cat /tmp/guest-tunnel.pub >> ~tunneluser/.ssh/authorized_keys

rm /tmp/guest-tunnel /tmp/guest-tunnel.pub
```

The same resident key lives on the YubiKey. When `ssh-add -K` is called on the borrowed machine, it's loaded into the agent and both gates use it automatically.

## Building from source

```bash
git clone https://github.com/yourusername/guest-tunnel
cd guest-tunnel

# Build for current platform
make build

# Cross-compile all platforms
make cross
```

### Configuration

Create a `config.yml` (or use `--init` to generate an example):

```yaml
vps_host: vps.example.com
vps_user: jumpuser
vps_port: 22

home_user: tunneluser       # SSH user on homeserver
tunnel_port: 2222           # reverse tunnel port on VPS (homeserver uses -R 2222:localhost:22)

socks_port: 1080
socks_bind: 127.0.0.1
```

Config file search order:
1. `--config` flag
2. `$GUEST_TUNNEL_CONFIG` env var
3. `./config.yml`
4. `~/.config/guest-tunnel/config.yml`
5. `~/.guest-tunnel.yml`

## Local Apple Container Test Harness

On macOS 26 with Apple silicon, you can exercise the tunnel locally with Apple's `container` CLI.

The harness starts three Linux containers:

- `vps`: SSH jump host
- `home`: homeserver SSH daemon plus a reverse tunnel back to `vps`
- `client`: runs `guest-tunnel` and validation checks

All three now run from the same test image. Role-specific startup is selected by entrypoint environment (`GT_ROLE=vps|home|client`).
The `vps` and `home` containers execute the real setup code paths (`--mode=server` and `--mode=home`) during boot, then start their runtime daemons.

The homeserver container also exposes a loopback-only HTTP page on `127.0.0.1:8080`. The smoke test reaches that page through the SOCKS proxy, which proves Gate 2 is really working instead of just opening a local port.

Run the happy-path check:

```bash
./scripts/apple-container-integration.sh smoke
```

Run the broader regression set:

```bash
./scripts/apple-container-integration.sh test
```

That test set currently covers:

- happy path: tunnel comes up and can fetch the homeserver-only page
- wrong tunnel port: startup must fail and must not print `tunnel is up`
- wrong VPS user: startup must fail and must not print `tunnel is up`
- local SOCKS port conflict: startup must fail and must not print `tunnel is up`

Useful helper commands:

```bash
./scripts/apple-container-integration.sh up
./scripts/apple-container-integration.sh logs
./scripts/apple-container-integration.sh logs client
./scripts/apple-container-integration.sh shell-client
./scripts/apple-container-integration.sh down
```

The single harness image is built from `test/docker/Dockerfile`.

## Security notes

- The VPS is a blind TCP relay for tunnel data — it cannot read or modify the SSH-encrypted traffic
- All temp files and the agent process are cleaned up on exit (including `SIGINT`)

## License

MIT
