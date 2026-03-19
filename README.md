# guest-tunnel

Authenticated SOCKS5 tunnel to your homelab from any borrowed machine — no sudo, no pre-installed libraries, just a YubiKey.

## How it works

1. You `curl` a single pre-compiled binary (no install, runs from `/tmp`)
2. The binary starts a private `ssh-agent` in memory
3. Your YubiKey's resident FIDO2 key is loaded into that agent (never written to disk)
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

# Verify hash BEFORE running (download the sha256sums.txt from the release page)
curl -fsSL \
  https://github.com/yourusername/guest-tunnel/releases/download/${VERSION}/sha256sums.txt \
  | grep "guest-tunnel-${OS}-${ARCH}" | sha256sum -c -

chmod +x /tmp/guest-tunnel
/tmp/guest-tunnel --mode=client --yubikey
```

Insert your YubiKey, let the helper load resident keys, and touch it when prompted. Done.

## Requirements on the borrowed machine

| Requirement | Notes |
|---|---|
| `ssh` | Used for the tunnel |
| `ssh-agent`, `ssh-add` | Ships with OpenSSH, present everywhere |
| YubiKey (FIDO2, resident key enrolled) | See enrollment below |
| No sudo needed | Everything runs in userspace |

## Embedded SSH Client

### Why a bundled SSH binary?

macOS ships OpenSSH without FIDO2 (`sk-*`) key type support — it is compiled against the system's Security framework rather than libfido2, and `sk-ssh-ed25519` is simply absent from `ssh -Q key`. This means resident YubiKey credentials cannot be loaded on macOS without a replacement binary.

guest-tunnel handles this automatically.

### How detection and fallback work

At startup, guest-tunnel runs:

```
ssh -Q key | grep sk-ssh-
```

- **If it matches**: system SSH supports FIDO2 — used as-is, nothing extra downloaded.
- **If it does not match**: guest-tunnel looks for a pre-built fallback in this order:
  1. `$GUEST_TUNNEL_SSH` environment variable (user override — always checked first)
  2. `./ssh-fido2-{os}-{arch}` alongside the guest-tunnel binary
  3. `$HOME/.local/bin/ssh-fido2`
  4. Downloads `ssh-fido2-{os}-{arch}` from the current release, verifies its SHA256 against `sha256sums.txt`, writes it to a temp directory, and deletes it on exit.

If no FIDO2-capable binary can be found or downloaded, guest-tunnel exits with a clear error rather than silently using a non-FIDO2 binary.

### About the bundled binary

- Built from upstream [OpenSSH V_9_8_P1](https://github.com/openssh/openssh-portable/tree/V_9_8_P1)
- Compiled with `--with-security-key-builtin=yes` — FIDO2 is embedded in the binary itself, no runtime library dependencies
- Does **not** use `--with-libfido2` (which would require libfido2 present at runtime)
- Verified: `ssh-fido2-{os}-{arch} -Q key` prints `sk-ssh-ed25519@openssh.com` on a clean system with no extra libraries installed

### Build it locally

```bash
make ssh-local
# Output: bin/ssh-fido2-{os}-{arch}
```

To use it as the override for local development:

```bash
GUEST_TUNNEL_SSH=./bin/ssh-fido2-$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/') ./dist/guest-tunnel --mode=client
```

Or place it alongside the binary so it is picked up automatically:

```bash
cp bin/ssh-fido2-darwin-arm64 dist/
```

## Server setup

### VPS (`/etc/ssh/sshd_config` additions)

```
Match User jumpuser
    AuthenticationMethods publickey
    PubkeyAuthentication yes
    # No port forwarding — this user is only a jump host
    AllowTcpForwarding no
    X11Forwarding no
    PermitTTY no
    ForceCommand /bin/false
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

# Cross-compile all platforms + generate sha256sums
make cross sha256sums
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

The underlying images are still built from the Dockerfiles in [test/docker/client/Dockerfile](/Users/denis/Projects/Tunnel/test/docker/client/Dockerfile) and [test/docker/sshd/Dockerfile](/Users/denis/Projects/Tunnel/test/docker/sshd/Dockerfile), because Apple's `container build` consumes Dockerfiles directly.

### Repository secrets for GitHub Actions release

Set these in `Settings → Secrets → Actions`:

| Secret | Value |
|---|---|
| `VPS_HOST` | Your VPS hostname |
| `VPS_USER` | Jump user on VPS |
| `HOME_USER` | Tunnel user on homeserver |
| `TUNNEL_PORT` | Reverse tunnel port on VPS (e.g., 2222) |

Tag a release (`git tag v1.0.0 && git push --tags`) to trigger the build.

## Security notes

- The VPS is a blind TCP relay for tunnel data — it cannot read or modify the SSH-encrypted traffic
- All temp files and the agent process are cleaned up on exit (including `SIGINT`)

## License

MIT
