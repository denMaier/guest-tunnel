// Package client handles the --mode=client-setup: writing ~/.ssh/config,
// ~/bin/opencode-tunnel, and ~/bin/homelab-tunnel, then testing VPS
// connectivity.
package client

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/yourusername/guest-tunnel/internal/config"
	"github.com/yourusername/guest-tunnel/internal/ui"
)

const (
	markerBegin = "# BEGIN reverse-tunnel-config"
	markerEnd   = "# END reverse-tunnel-config"
	socksPort   = "1080"
)

// Setup writes the SSH config block and helper scripts for the client (laptop).
// It does not require root.
func Setup(configPath *string, initFlag *bool) {
	if *initFlag {
		fmt.Println(config.Example())
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		ui.Fatal("Failed to load config: %v", err)
	}

	ui.Header("Client (Laptop) Setup — SSH Config and Tunnel Scripts")

	forwardPort := promptForwardPort()

	writeSSHConfig(cfg)
	writeOpencodeScript(cfg, forwardPort)
	writeHomelabScript(cfg)
	testVPSConnectivity(cfg)
	checkBinOnPath()

	ui.Header("Client Setup Complete — Summary")
	ui.Print("  • ~/.ssh/config updated with 'vps' and 'homeserver' blocks (ForwardAgent no)")
	ui.Print("  • ~/bin/opencode-tunnel  — forwards port %s only", forwardPort)
	ui.Print("  • ~/bin/homelab-tunnel   — SOCKS5 proxy on localhost:%s", socksPort)
	ui.Print("")
	ui.Print("Usage:")
	ui.Print("  Shell access to homeserver:  ssh homeserver")
	ui.Print("  OpenCode port forward:       opencode-tunnel    → http://localhost:%s", forwardPort)
	ui.Print("  Full homelab SOCKS5 proxy:   homelab-tunnel     → configure browser as shown")
	ui.Print("")
	ui.Print("Architecture:")
	ui.Print("  laptop → (E2E encrypted) → VPS:22 (dumb relay) → homeserver:2222")
	ui.Print("  VPS sees only opaque ciphertext. Keys never leave your devices.")
}

// ── Parameter helpers ─────────────────────────────────────────────────────────

func promptForwardPort() string {
	// Try to recover from an existing opencode-tunnel script
	if home, err := os.UserHomeDir(); err == nil {
		script := filepath.Join(home, "bin", "opencode-tunnel")
		if data, err := os.ReadFile(script); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "PORT=") {
					port := strings.TrimPrefix(line, "PORT=")
					port = strings.TrimSpace(port)
					if port != "" {
						fmt.Printf("  Detected existing forward port: %s\n", port)
						fmt.Print("  Keep it? [Y/n]: ")
						scanner := bufio.NewScanner(os.Stdin)
						if scanner.Scan() && strings.ToLower(strings.TrimSpace(scanner.Text())) != "n" {
							return port
						}
					}
				}
			}
		}
	}

	fmt.Print("  Local port to forward for OpenCode [3000]: ")
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		p := strings.TrimSpace(scanner.Text())
		if p != "" {
			return p
		}
	}
	return "3000"
}

// ── SSH config ────────────────────────────────────────────────────────────────

func writeSSHConfig(cfg *config.Config) {
	ui.Step(1, "Writing ~/.ssh/config...")

	home, err := os.UserHomeDir()
	if err != nil {
		ui.Fatal("Cannot determine home directory: %v", err)
	}
	sshDir := filepath.Join(home, ".ssh")
	sshConfig := filepath.Join(sshDir, "config")

	os.MkdirAll(sshDir, 0700)

	// Backup existing config
	if _, err := os.Stat(sshConfig); err == nil {
		backup := sshConfig + ".bak." + timestamp()
		if data, err := os.ReadFile(sshConfig); err == nil {
			os.WriteFile(backup, data, 0600)
			ui.OK("Existing config backed up → %s", backup)
		}
	}

	block := buildSSHConfigBlock(cfg)

	// Read existing content, strip old block if present, append new one
	existing := ""
	if data, err := os.ReadFile(sshConfig); err == nil {
		existing = string(data)
	}
	existing = removeMarkerBlock(existing, markerBegin, markerEnd)
	existing = strings.TrimRight(existing, "\n")

	content := existing + "\n" + block + "\n"

	if err := os.WriteFile(sshConfig, []byte(content), 0600); err != nil {
		ui.Fatal("Failed to write %s: %v", sshConfig, err)
	}
	ui.OK("~/.ssh/config updated")
	fmt.Println()
	fmt.Printf("  Written block:\n")
	for _, line := range strings.Split(block, "\n") {
		fmt.Printf("    %s\n", line)
	}
	fmt.Println()
}

func buildSSHConfigBlock(cfg *config.Config) string {
	vpsPort := cfg.VPSPort
	if vpsPort == "" || vpsPort == "22" {
		vpsPort = "" // omit Port line when default
	}

	var vpsPortLine string
	if vpsPort != "" {
		vpsPortLine = "\n    Port " + vpsPort
	}

	return fmt.Sprintf(`%s
Host vps
    HostName %s
    User %s%s
    ForwardAgent no
    ServerAliveInterval 30
    ServerAliveCountMax 3

Host homeserver
    HostName localhost
    Port %s
    User %s
    ProxyCommand ssh -W %%h:%%p vps
    ForwardAgent no
    ServerAliveInterval 30
    ServerAliveCountMax 3
%s`, markerBegin, cfg.VPSHost, cfg.VPSUser, vpsPortLine, cfg.TunnelPort, cfg.HomeUser, markerEnd)
}

// removeMarkerBlock strips everything between markerBegin and markerEnd
// (inclusive) from s, returning the cleaned string.
func removeMarkerBlock(s, begin, end string) string {
	startIdx := strings.Index(s, begin)
	if startIdx < 0 {
		return s
	}
	endIdx := strings.Index(s, end)
	if endIdx < 0 {
		return s
	}
	endIdx += len(end)
	// Also consume a leading newline before the block
	trim := startIdx
	if trim > 0 && s[trim-1] == '\n' {
		trim--
	}
	return s[:trim] + s[endIdx:]
}

// ── Helper scripts ────────────────────────────────────────────────────────────

func writeOpencodeScript(cfg *config.Config, forwardPort string) {
	ui.Step(2, "Writing ~/bin/opencode-tunnel...")

	path := binPath("opencode-tunnel")
	content := fmt.Sprintf(`#!/usr/bin/env bash
# opencode-tunnel — Forward homeserver:%s → localhost:%s
# Generated by guest-tunnel --mode=client-setup

set -euo pipefail

PORT=%s
BOLD='\033[1m'; GREEN='\033[0;32m'; RESET='\033[0m'

_cleanup() { echo -e "\n${BOLD}Tunnel closed.${RESET}"; exit 0; }
trap _cleanup INT TERM

echo -e "${GREEN}${BOLD}Starting tunnel: homeserver:${PORT} → localhost:${PORT}${RESET}"
echo -e "${BOLD}OpenCode will be available at: http://localhost:${PORT}${RESET}"
echo    "(Press Ctrl+C to close the tunnel)"
echo

ssh -N -L "${PORT}:localhost:${PORT}" homeserver &
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

wait "$SSH_PID"
`, forwardPort, forwardPort, forwardPort)

	writeScript(path, content)
	ui.OK("~/bin/opencode-tunnel written")
}

func writeHomelabScript(cfg *config.Config) {
	ui.Step(3, "Writing ~/bin/homelab-tunnel...")

	path := binPath("homelab-tunnel")
	content := fmt.Sprintf(`#!/usr/bin/env bash
# homelab-tunnel — SOCKS5 tunnel via homeserver.
#
# Security model:
#   laptop ──[ed25519]──▶ VPS:22 (dumb relay) ──▶ homeserver:%s
#   VPS sees only opaque ciphertext. No sudo. No routing table changes.
#
# Generated by guest-tunnel --mode=client-setup

set -euo pipefail

SOCKS_PORT=%s
BOLD='\033[1m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; RESET='\033[0m'

SSH_PID=""

_cleanup() {
    echo -e "\n${BOLD}Closing homelab tunnel...${RESET}"
    [[ -n $SSH_PID ]] && kill "$SSH_PID" 2>/dev/null || true
    echo -e "${BOLD}SOCKS5 tunnel closed.${RESET}"
    exit 0
}
trap _cleanup INT TERM

echo -e "${GREEN}${BOLD}Opening SOCKS5 tunnel → homeserver (localhost:${SOCKS_PORT})${RESET}"
ssh -N -D "${SOCKS_PORT}" homeserver &
SSH_PID=$!

verify_socks() {
    local socks_port=$1
    local max_attempts=${2:-20}
    for i in $(seq 1 $max_attempts); do
        if nc -z 127.0.0.1 "${socks_port}" >/dev/null 2>&1; then
            return 0
        fi
        if ! kill -0 "$SSH_PID" 2>/dev/null; then
            return 2
        fi
        echo -n "."
        sleep 1
    done
    return 1
}

echo -n "  Waiting for tunnel"
verify_status=0
if verify_socks "${SOCKS_PORT}"; then
    echo -e " ${GREEN}✓${RESET}"
else
    verify_status=$?
    echo ""
    if [[ $verify_status -eq 2 ]]; then
        exit_code=1
        if wait "$SSH_PID"; then
            exit_code=0
        else
            exit_code=$?
        fi
        echo -e "${YELLOW}[WARN]${RESET}  SSH exited before tunnel became ready (likely auth failure)." >&2
        exit "$exit_code"
    fi
    echo -e "${YELLOW}[WARN]${RESET}  Tunnel did not become ready — SSH may have failed." >&2
    echo -e "${YELLOW}[HINT]${RESET}  Check with: ssh -v -D ${SOCKS_PORT} homeserver" >&2
    exit 1
fi

echo -e "${GREEN}✓ SOCKS5 tunnel up on localhost:${SOCKS_PORT}${RESET}"
echo ""
echo -e "${CYAN}DNS resolves on homeserver — .home/.lan/.internal domains work.${RESET}"
echo -e "${CYAN}Only configure the browser you want to proxy — all other apps are unaffected.${RESET}"
echo    "(Press Ctrl+C to close tunnel)"
echo

wait "$SSH_PID"
`, cfg.TunnelPort, socksPort)

	writeScript(path, content)
	ui.OK("~/bin/homelab-tunnel written")
	printBrowserInstructions()
}

func printBrowserInstructions() {
	fmt.Println()
	ui.Header("Browser SOCKS5 Configuration")
	fmt.Printf("After running %shomelab-tunnel%s, configure your browser:\n\n", ui.BOLD, ui.RESET)

	fmt.Printf("%s%sFirefox / Firefox-based (Librewolf, Zen, Floorp…)%s\n", ui.BOLD, ui.CYAN, ui.RESET)
	fmt.Println("  Recommended: dedicated profile (keeps homelab traffic isolated)")
	fmt.Printf("    1. Create profile: %sfirefox --new-instance -P%s\n", ui.CYAN, ui.RESET)
	fmt.Println("    2. about:preferences#general → Network Settings → Settings…")
	fmt.Println("    3. Manual proxy configuration:")
	fmt.Printf("         SOCKS Host: 127.0.0.1   Port: %s\n", socksPort)
	fmt.Println("         SOCKS v5 ✓   Proxy DNS over SOCKS5 ✓")
	fmt.Println()

	fmt.Printf("%s%sChromium / Chrome / Edge / Brave%s\n", ui.BOLD, ui.CYAN, ui.RESET)
	fmt.Printf("  %schromium --proxy-server='socks5://127.0.0.1:%s' \\\n", ui.CYAN, socksPort)
	fmt.Println("           --host-resolver-rules='MAP * ~NOTFOUND, EXCLUDE 127.0.0.1' \\")
	fmt.Printf("           --user-data-dir=/tmp/homelab-chrome%s\n", ui.RESET)
	fmt.Println()

	fmt.Printf("%s%scurl (testing)%s\n", ui.BOLD, ui.CYAN, ui.RESET)
	fmt.Printf("  %scurl --socks5-hostname 127.0.0.1:%s http://myservice.home%s\n\n", ui.CYAN, socksPort, ui.RESET)
}

// ── VPS connectivity test ─────────────────────────────────────────────────────

func testVPSConnectivity(cfg *config.Config) {
	ui.Step(4, "Testing VPS connectivity...")

	target := fmt.Sprintf("%s@%s", cfg.VPSUser, cfg.VPSHost)
	cmd := exec.Command("ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=5",
		"-o", "StrictHostKeyChecking=accept-new",
		target, "true",
	)
	if err := cmd.Run(); err != nil {
		ui.Warn("VPS hop FAILED — possible causes:")
		ui.Hint("  • Public key not yet installed on the VPS for %s", cfg.VPSUser)
		ui.Hint("  • Firewall blocking port 22 to the VPS")
		ui.Hint("  • SSH agent key does not match any key on the VPS")
		ui.Hint("  Retry manually: ssh %s", target)
		return
	}
	ui.OK("VPS hop successful — %s is reachable", target)
}

// ── PATH check ────────────────────────────────────────────────────────────────

func checkBinOnPath() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	binDir := filepath.Join(home, "bin")
	pathEnv := os.Getenv("PATH")
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == binDir {
			return
		}
	}
	ui.Warn("~/bin is not in your PATH.")
	ui.Hint("Add this to your ~/.bashrc or ~/.zshrc:")
	fmt.Printf("    %sexport PATH=\"$HOME/bin:$PATH\"%s\n", ui.CYAN, ui.RESET)
}

// ── Utilities ─────────────────────────────────────────────────────────────────

func binPath(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		ui.Fatal("Cannot determine home directory: %v", err)
	}
	dir := filepath.Join(home, "bin")
	os.MkdirAll(dir, 0755)
	return filepath.Join(dir, name)
}

func writeScript(path, content string) {
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		ui.Fatal("Failed to write %s: %v", path, err)
	}
}

func timestamp() string {
	// Use time package would add an import; keep it simple with a proc read
	out, err := exec.Command("date", "+%Y%m%d%H%M%S").Output()
	if err != nil {
		return "backup"
	}
	return strings.TrimSpace(string(out))
}
