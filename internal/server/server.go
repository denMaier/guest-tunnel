package server

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/yourusername/guest-tunnel/internal/config"
	"github.com/yourusername/guest-tunnel/internal/ui"
)

const (
	sshdConf       = "/etc/ssh/sshd_config"
	sshdConfDropIn = "/etc/ssh/sshd_config.d"
	cloudInitConf  = "/etc/ssh/sshd_config.d/50-cloud-init.conf"
	markerBegin    = "# BEGIN jumpuser-tunnel-block"
	markerEnd      = "# END jumpuser-tunnel-block"
)

func Run(configPath *string, initFlag *bool) {
	if *initFlag {
		writeExampleConfig()
		os.Exit(0)
	}

	if os.Geteuid() != 0 {
		ui.Fatal("This mode must be run as root (use sudo).")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		ui.Fatal("Failed to load config: %v", err)
	}

	setupVPS(cfg)
}

func writeExampleConfig() {
	fmt.Println(config.Example())
	fmt.Println("\nEdit the config and save to /etc/guest-tunnel/config.yml")
}

func setupVPS(cfg *config.Config) {
	ui.Header("VPS Setup — Relay Node Configuration")

	setupJumpUser(cfg.VPSUser)
	setupSSHKeys(cfg.VPSUser)
	hardenSSH(cfg)
	installFail2ban()
	restartSSH()

	ui.Header("VPS Setup Complete — Summary")
	ui.Print("  • %s created with /usr/sbin/nologin shell", cfg.VPSUser)
	ui.Print("  • Laptop public key installed in /home/%s/.ssh/authorized_keys", cfg.VPSUser)
	ui.Print("  • sshd hardened: no passwords, no root, %s restricted to localhost:%s reverse tunnel", cfg.VPSUser, cfg.TunnelPort)
	ui.Print("  • fail2ban active (ban 1h after 5 failures in 10 min)")
	ui.Print("")
	ui.Print("Next steps:")
	ui.Print("  1. Run this binary with --mode=home on the homeserver.")
	ui.Print("  2. When prompted, install the tunnel service key into")
	ui.Print("     /home/%s/.ssh/authorized_keys on THIS VPS.", cfg.VPSUser)
	ui.Print("  3. Ensure port 22 (or your SSH port) is open in your firewall.")
}

func setupJumpUser(username string) {
	ui.Step(1, fmt.Sprintf("Creating %s...", username))

	if _, err := user.Lookup(username); err == nil {
		ui.OK("%s already exists", username)
		ensureServiceUserState(username, "/usr/sbin/nologin")
		return
	}

	cmd := exec.Command("useradd", "--system", "--shell", "/usr/sbin/nologin", "--create-home", username)
	if out, err := cmd.CombinedOutput(); err != nil {
		ui.Fatal("Failed to create %s: %v\n%s", username, err, out)
	}

	ensureServiceUserState(username, "/usr/sbin/nologin")
	ui.OK("Created %s with /usr/sbin/nologin shell", username)
}

func ensureServiceUserState(username, shell string) {
	// Some OpenSSH/PAM combinations reject pubkey auth for accounts that are
	// present but password-locked. We explicitly keep the service user
	// shell-restricted while making the account state usable for pubkey-only SSH.
	runUsermodIfPossible(username, "--shell", shell)
	runUsermodIfPossible(username, "--unlock")
	runPasswdIfPossible(username, "--delete")
}

func runUsermodIfPossible(username string, args ...string) {
	cmd := exec.Command("usermod", append(args, username)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		ui.Warn("Could not update %s with usermod %s: %v\n%s", username, strings.Join(args, " "), err, out)
	}
}

func runPasswdIfPossible(username string, args ...string) {
	cmd := exec.Command("passwd", append(args, username)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		ui.Warn("Could not update %s with passwd %s: %v\n%s", username, strings.Join(args, " "), err, out)
	}
}

func setupSSHKeys(username string) {
	ui.Step(2, "Installing authorized SSH keys...")

	sshDir := filepath.Join("/home", username, ".ssh")
	authKeys := filepath.Join(sshDir, "authorized_keys")

	if err := os.MkdirAll(sshDir, 0700); err != nil {
		ui.Fatal("Failed to create %s: %v", sshDir, err)
	}
	os.Chown(sshDir, 0, 0)
	os.Chmod(sshDir, 0700)

	existingKeys := readPublicKeys(authKeys)

	if len(existingKeys) > 0 {
		ui.OK("%d key(s) already in authorized_keys — preserving existing access", len(existingKeys))
		for _, k := range existingKeys {
			fields := strings.Fields(k)
			comment := ""
			if len(fields) >= 3 {
				comment = fields[2]
			}
			ui.Hint("  %s", comment)
		}
		ui.OK("Skipping key installation on rerun")
		return
	}

	fmt.Printf("\n  Paste the laptop's SSH public key (single line):\n  ")
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		ui.Warn("No input — skipping key installation")
		return
	}
	key := strings.TrimSpace(scanner.Text())
	if key == "" {
		ui.Warn("Empty key — skipping")
		return
	}

	for _, existing := range existingKeys {
		if existing == key {
			ui.OK("That key is already present — skipping")
			return
		}
	}

	f, err := os.OpenFile(authKeys, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		ui.Fatal("Failed to open authorized_keys: %v", err)
	}
	f.WriteString(key + "\n")
	f.Close()

	os.Chown(authKeys, 0, 0)
	os.Chmod(authKeys, 0600)
	ui.OK("Public key installed for %s", username)
}

func readPublicKeys(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var keys []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ssh-") || strings.HasPrefix(line, "ecdsa-") || strings.HasPrefix(line, "sk-") {
			keys = append(keys, line)
		}
	}
	return keys
}

func hardenSSH(cfg *config.Config) {
	ui.Step(3, "Hardening /etc/ssh/sshd_config...")

	timestamp := time.Now().Format("20060102150405")
	backup := sshdConf + ".bak." + timestamp
	if data, err := os.ReadFile(sshdConf); err == nil {
		if err := os.WriteFile(backup, data, 0644); err != nil {
			ui.Warn("Could not write backup %s: %v", backup, err)
		} else {
			ui.OK("Backed up sshd_config → %s", backup)
		}
	}

	neutraliseCloudInit()
	neutraliseDropIns()

	setSSHDirective(sshdConf, "PasswordAuthentication", "no")
	setSSHDirective(sshdConf, "PermitRootLogin", "no")
	setSSHDirective(sshdConf, "ChallengeResponseAuthentication", "no")
	setSSHDirective(sshdConf, "UsePAM", "yes")
	ui.OK("Global sshd hardening applied")

	applyMatchBlock(cfg)
}

func neutraliseCloudInit() {
	if _, err := os.Stat(cloudInitConf); err != nil {
		return
	}
	timestamp := time.Now().Format("20060102150405")
	backup := cloudInitConf + ".bak." + timestamp
	if data, err := os.ReadFile(cloudInitConf); err == nil {
		os.WriteFile(backup, data, 0644)
	}
	os.WriteFile(cloudInitConf, []byte("PasswordAuthentication no\n"), 0644)
	ui.OK("Neutralised %s", cloudInitConf)
}

func neutraliseDropIns() {
	entries, err := os.ReadDir(sshdConfDropIn)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".conf") {
			continue
		}
		path := filepath.Join(sshdConfDropIn, e.Name())
		if path == cloudInitConf {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lower := strings.ToLower(string(data))
		if !strings.Contains(lower, "passwordauthentication yes") {
			continue
		}
		var out []string
		for _, line := range strings.Split(string(data), "\n") {
			if strings.EqualFold(strings.TrimSpace(line), "passwordauthentication yes") {
				out = append(out, "PasswordAuthentication no")
				ui.Warn("Overrode PasswordAuthentication yes in %s", path)
			} else {
				out = append(out, line)
			}
		}
		os.WriteFile(path, []byte(strings.Join(out, "\n")), 0644)
	}
}

func setSSHDirective(path, key, value string) {
	data, err := os.ReadFile(path)
	if err != nil {
		ui.Warn("Cannot read %s: %v", path, err)
		return
	}

	lines := strings.Split(string(data), "\n")
	replaced := false
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, "# \t")
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, strings.ToLower(key)+" ") ||
			strings.HasPrefix(lower, strings.ToLower(key)+"\t") {
			lines[i] = key + " " + value
			replaced = true
		}
	}

	if !replaced {
		lines = append(lines, key+" "+value)
	}

	os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644)
}

func applyMatchBlock(cfg *config.Config) {
	data, err := os.ReadFile(sshdConf)
	if err != nil {
		ui.Fatal("Cannot read %s: %v", sshdConf, err)
	}
	content := string(data)

	block := fmt.Sprintf(`
%s
Match User %s
    PasswordAuthentication no
    PubkeyAuthentication yes
    AllowAgentForwarding no
    AllowTcpForwarding yes
    X11Forwarding no
    PermitTTY no
    PermitOpen localhost:%s
    PermitListen localhost:%s
%s`, markerBegin, cfg.VPSUser, cfg.TunnelPort, cfg.TunnelPort, markerEnd)

	if strings.Contains(content, markerBegin) {
		start := strings.Index(content, markerBegin)
		end := strings.Index(content, markerEnd)
		if end >= 0 {
			end += len(markerEnd)
		}
		if start >= 0 && end > start {
			content = content[:start] + strings.TrimLeft(block, "\n") + content[end:]
		}
		ui.OK("jumpuser Match block updated in sshd_config")
	} else {
		content += block
		ui.OK("jumpuser Match block written to sshd_config")
	}

	os.WriteFile(sshdConf, []byte(content), 0644)
}

func installFail2ban() {
	ui.Step(4, "Installing fail2ban...")

	if _, err := exec.LookPath("fail2ban-server"); err == nil {
		ui.OK("fail2ban already installed")
		exec.Command("systemctl", "enable", "--now", "fail2ban").Run()
		return
	}

	exec.Command("apt-get", "update", "-qq").Run()
	if out, err := exec.Command("apt-get", "install", "-y", "-qq", "fail2ban").CombinedOutput(); err != nil {
		ui.Warn("Failed to install fail2ban: %v\n%s", err, out)
		return
	}

	jailLocal := "/etc/fail2ban/jail.local"
	if _, err := os.Stat(jailLocal); err != nil {
		content := `[DEFAULT]
bantime  = 3600
findtime = 600
maxretry = 5
backend  = systemd

[sshd]
enabled  = true
port     = ssh
logpath  = %(sshd_log)s
`
		os.WriteFile(jailLocal, []byte(content), 0644)
		ui.OK("fail2ban jail.local written")
	} else {
		ui.OK("jail.local already exists — not overwriting")
	}

	exec.Command("systemctl", "enable", "--now", "fail2ban").Run()
	ui.OK("fail2ban installed and running")
}

func restartSSH() {
	ui.Step(5, "Validating and restarting SSH daemon...")

	if out, err := exec.Command("sshd", "-t").CombinedOutput(); err != nil {
		ui.Fatal("sshd_config validation failed — NOT restarting sshd.\n%s\nReview %s", out, sshdConf)
	}
	ui.OK("sshd_config validation passed")

	restarted := false
	for _, unit := range []string{"ssh", "sshd", "openssh-server"} {
		if err := exec.Command("systemctl", "restart", unit).Run(); err == nil {
			ui.OK("sshd restarted (%s)", unit)
			restarted = true
			break
		}
	}
	if !restarted {
		ui.Warn("Could not restart sshd — do it manually: systemctl restart ssh")
	}
}

func Uninstall(configPath *string, forceFlag *bool) {
	if os.Geteuid() != 0 {
		ui.Fatal("This mode must be run as root (use sudo).")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		ui.Fatal("Failed to load config: %v", err)
	}

	username := cfg.VPSUser
	if username == "" {
		username = "jumpuser"
	}

	ui.Header("VPS Uninstall — Removing Jump Host Configuration")
	ui.Print("This will PERMANENTLY DELETE:")
	ui.Print("  - %s and all files in /home/%s", username, username)
	ui.Print("  - jumpuser Match block from sshd_config")
	ui.Print("  (fail2ban and global hardening directives are left in place)")
	ui.Print("")

	confirmOrForce(forceFlag)

	removeMatchBlock()
	removeUser(username)
	restartSSH()

	ui.Header("VPS Uninstall Complete")
	ui.Print("  • %s removed", username)
	ui.Print("  • jumpuser Match block removed from sshd_config")
	ui.Print("  • sshd restarted")
	ui.Print("  • fail2ban left intact")
}

func confirmOrForce(forceFlag *bool) {
	if *forceFlag {
		return
	}
	fmt.Print("Continue? [y/N]: ")
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		response := strings.ToLower(strings.TrimSpace(scanner.Text()))
		if response != "y" && response != "yes" {
			ui.Print("Aborted.")
			os.Exit(0)
		}
	}
}

func removeMatchBlock() {
	ui.Step(1, "Removing jumpuser Match block from sshd_config...")

	data, err := os.ReadFile(sshdConf)
	if err != nil {
		ui.Warn("Cannot read %s: %v", sshdConf, err)
		return
	}
	content := string(data)

	if !strings.Contains(content, markerBegin) {
		ui.OK("No jumpuser Match block found — nothing to remove")
		return
	}

	start := strings.Index(content, markerBegin)
	end := strings.Index(content, markerEnd)
	if end >= 0 {
		end += len(markerEnd)
	}
	if start < 0 || end <= start {
		ui.Warn("Malformed marker block — skipping removal")
		return
	}

	trimStart := start
	if trimStart > 0 && content[trimStart-1] == '\n' {
		trimStart--
	}

	content = content[:trimStart] + content[end:]
	os.WriteFile(sshdConf, []byte(content), 0644)
	ui.OK("jumpuser Match block removed")
}

func removeUser(username string) {
	ui.Step(2, fmt.Sprintf("Removing user %s...", username))

	if _, err := user.Lookup(username); err != nil {
		ui.Warn("User %s does not exist", username)
		return
	}

	if out, err := exec.Command("userdel", "-r", username).CombinedOutput(); err != nil {
		ui.Warn("Failed to remove user: %v\n%s", err, out)
		return
	}

	ui.OK("User %s removed", username)
}
