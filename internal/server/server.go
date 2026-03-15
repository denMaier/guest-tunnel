package server

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/yourusername/guest-tunnel/internal/config"
	"github.com/yourusername/guest-tunnel/internal/ui"
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
	hardenSSH()
	installFail2ban()
	restartSSH()

	ui.Header("VPS Setup Complete — Summary")
	ui.Print("  • jumpuser created with /usr/sbin/nologin shell")
	ui.Print("  • SSH hardening applied: no passwords, no root")
	ui.Print("  • fail2ban active")
	ui.Print("")
	ui.Print("Next: Run this binary with --mode=home on the homeserver.")
}

func setupJumpUser(username string) {
	ui.Step(1, "Creating tunnel user...")

	if _, err := user.Lookup(username); err == nil {
		ui.OK("User %s already exists", username)
		return
	}

	cmd := exec.Command("useradd", "--system", "--shell", "/usr/sbin/nologin", "--create-home", username)
	if out, err := cmd.CombinedOutput(); err != nil {
		ui.Fatal("Failed to create user: %v\n%s", err, out)
	}

	ui.OK("Created user %s", username)
}

func setupSSHKeys(username string) {
	ui.Step(2, "Setting up SSH authorized_keys...")

	sshDir := filepath.Join("/home", username, ".ssh")
	authKeys := filepath.Join(sshDir, "authorized_keys")

	os.MkdirAll(sshDir, 0700)
	os.Chown(sshDir, 0, 0)
	os.Chmod(sshDir, 0700)

	if _, err := os.Stat(authKeys); err == nil {
		ui.OK("Authorized keys file exists")
		printExistingKeys(authKeys)
		return
	}

	fmt.Printf("\n  Paste the laptop's SSH public key (single line):\n  ")
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		key := strings.TrimSpace(scanner.Text())
		if key != "" {
			os.WriteFile(authKeys, []byte(key+"\n"), 0600)
			os.Chown(authKeys, 0, 0)
			os.Chmod(authKeys, 0600)
			ui.OK("Public key installed")
		}
	}
}

func printExistingKeys(authKeys string) {
	file, err := os.Open(authKeys)
	if err != nil {
		return
	}
	defer file.Close()

	count := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "ssh-") {
			count++
		}
	}
	ui.OK("%d key(s) already installed", count)
}

func hardenSSH() {
	ui.Step(3, "Hardening SSH configuration...")

	conf := "/etc/ssh/sshd_config"
	backup := conf + ".bak"
	if _, err := os.Stat(backup); err != nil {
		data, _ := os.ReadFile(conf)
		os.WriteFile(backup, data, 0644)
	}

	data, _ := os.ReadFile(conf)
	lines := strings.Split(string(data), "\n")
	var out []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			out = append(out, line)
			continue
		}

		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "passwordauthentication") ||
			strings.HasPrefix(lower, "permitrootlogin") ||
			strings.HasPrefix(lower, "challengeresponseauthentication") {
			continue
		}
		out = append(out, line)
	}

	out = append(out, "", "# guest-tunnel hardening")
	out = append(out, "PasswordAuthentication no")
	out = append(out, "PermitRootLogin no")
	out = append(out, "ChallengeResponseAuthentication no")

	matchBlock := `
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
# END jumpuser-tunnel-block`
	out = append(out, matchBlock)

	os.WriteFile(conf, []byte(strings.Join(out, "\n")), 0644)
	ui.OK("SSH hardened")
}

func installFail2ban() {
	ui.Step(4, "Installing fail2ban...")

	if _, err := exec.LookPath("fail2ban-server"); err == nil {
		ui.OK("fail2ban already installed")
		return
	}

	cmd := exec.Command("apt-get", "update", "-qq")
	cmd.Run()

	cmd = exec.Command("apt-get", "install", "-y", "-qq", "fail2ban")
	if out, err := cmd.CombinedOutput(); err != nil {
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
logpath  = %(sshd_log)s`
		os.WriteFile(jailLocal, []byte(content), 0644)
	}

	exec.Command("systemctl", "enable", "--now", "fail2ban").Run()
	ui.OK("fail2ban installed and running")
}

func restartSSH() {
	ui.Step(5, "Restarting SSH daemon...")

	if err := exec.Command("sshd", "-t").Run(); err != nil {
		ui.Fatal("SSH config validation failed: %v", err)
	}

	exec.Command("systemctl", "restart", "sshd").Run()
	ui.OK("SSH daemon restarted")
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

	confirmOrForce(forceFlag, "This will PERMANENTLY DELETE:")
	ui.Print("  - jumpuser and all files in /home/%s", username)
	ui.Print("  - SSH hardening (sshd_config will be restored)")
	confirmOrForce(forceFlag, "")

	restoreSSHConfig()
	removeUser(username)

	ui.Header("VPS Uninstall Complete")
	ui.Print("  • jumpuser removed")
	ui.Print("  • sshd_config restored")
	ui.Print("  • fail2ban left intact (it's useful)")
}

func confirmOrForce(forceFlag *bool, msg string) {
	if *forceFlag {
		return
	}
	if msg != "" {
		fmt.Println(msg)
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

func restoreSSHConfig() {
	ui.Step(1, "Restoring sshd_config...")

	conf := "/etc/ssh/sshd_config"
	backup := conf + ".bak.guest-tunnel"

	if _, err := os.Stat(backup); err != nil {
		ui.Warn("No backup found at %s", backup)
		ui.Warn("Attempting to remove guest-tunnel additions manually...")
		removeSSHHardening()
		return
	}

	data, _ := os.ReadFile(backup)
	os.WriteFile(conf, data, 0644)
	os.Rename(backup, conf+".bak")

	ui.OK("sshd_config restored from backup")
}

func removeSSHHardening() {
	conf := "/etc/ssh/sshd_config"
	data, _ := os.ReadFile(conf)
	lines := strings.Split(string(data), "\n")
	var out []string
	skip := false

	for _, line := range lines {
		if strings.Contains(line, "# BEGIN jumpuser-tunnel-block") {
			skip = true
			continue
		}
		if strings.Contains(line, "# END jumpuser-tunnel-block") {
			skip = false
			continue
		}
		if skip {
			continue
		}
		if strings.Contains(line, "# guest-tunnel hardening") {
			continue
		}
		if strings.TrimSpace(line) == "PasswordAuthentication no" {
			continue
		}
		if strings.TrimSpace(line) == "PermitRootLogin no" {
			continue
		}
		if strings.TrimSpace(line) == "ChallengeResponseAuthentication no" {
			continue
		}
		out = append(out, line)
	}

	os.WriteFile(conf, []byte(strings.Join(out, "\n")), 0644)
	ui.OK("Removed guest-tunnel hardening additions")
}

func removeUser(username string) {
	ui.Step(2, "Removing user "+username+"...")

	if _, err := user.Lookup(username); err != nil {
		ui.Warn("User %s does not exist", username)
		return
	}

	cmd := exec.Command("userdel", "-r", username)
	if out, err := cmd.CombinedOutput(); err != nil {
		ui.Warn("Failed to remove user: %v\n%s", err, out)
		return
	}

	ui.OK("User %s removed", username)
}
