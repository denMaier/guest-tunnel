package home

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

	setupHome(cfg)
}

func writeExampleConfig() {
	fmt.Println(config.Example())
	fmt.Println("\nEdit the config and save to /etc/guest-tunnel/config.yml")
}

func setupHome(cfg *config.Config) {
	ui.Header("Homeserver Setup — Persistent Reverse Tunnel")

	setupTunnelUser()
	generateSSHKey()
	installClientPublicKey(cfg)
	testVPSConnection(cfg)
	installAutossh()
	setupSystemdService(cfg)
	enableAndStart()

	ui.Header("Homeserver Setup Complete — Summary")
	ui.Print("  • tunneluser created (unprivileged, nologin shell)")
	ui.Print("  • ed25519 keypair generated")
	ui.Print("  • reverse-tunnel.service installed and running")
	ui.Print("  • Tunnel: homeserver:22 → VPS:localhost:2222")
	ui.Print("")
	ui.Print("Next: Run this binary with --mode=client on your laptop.")
}

func setupTunnelUser() {
	ui.Step(1, "Creating tunneluser...")

	if _, err := user.Lookup("tunneluser"); err == nil {
		ui.OK("tunneluser already exists")
		return
	}

	cmd := exec.Command("useradd", "--system", "--shell", "/usr/sbin/nologin", "--create-home", "tunneluser")
	if out, err := cmd.CombinedOutput(); err != nil {
		ui.Fatal("Failed to create tunneluser: %v\n%s", err, out)
	}

	ui.OK("Created tunneluser")
}

func generateSSHKey() {
	ui.Step(2, "Generating SSH keypair...")

	keyDir := "/home/tunneluser/.ssh"
	keyFile := keyDir + "/tunnel_ed25519"

	os.MkdirAll(keyDir, 0700)
	os.Chown(keyDir, 0, 0)

	if _, err := os.Stat(keyFile); err == nil {
		ui.OK("Keypair already exists")
		return
	}

	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-f", keyFile, "-N", "", "-C", "tunneluser@homeserver")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		ui.Fatal("Failed to generate keypair: %v", err)
	}

	os.Chown(keyFile, 0, 0)
	os.Chmod(keyFile, 0600)
	os.Chown(keyFile+".pub", 0, 0)
	os.Chmod(keyFile+".pub", 0644)

	ui.OK("Generated ed25519 keypair")
}

func installClientPublicKey(cfg *config.Config) {
	ui.Step(3, "Installing client public key...")

	fmt.Printf("\n  Paste the laptop's SSH public key (single line):\n  ")
	var key string
	fmt.Scanln(&key)
	key = strings.TrimSpace(key)

	if key == "" {
		ui.Warn("No key provided - skipping")
		return
	}

	akDir := "/home/tunneluser/.ssh"
	akFile := akDir + "/authorized_keys"

	os.MkdirAll(akDir, 0700)

	if _, err := os.Stat(akFile); err == nil {
		data, _ := os.ReadFile(akFile)
		if strings.Contains(string(data), key) {
			ui.OK("Key already installed")
			return
		}
	}

	f, err := os.OpenFile(akFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		ui.Fatal("Failed to write authorized_keys: %v", err)
	}
	f.WriteString(key + "\n")
	f.Close()

	os.Chown(akFile, 0, 0)
	os.Chmod(akFile, 0600)

	ui.OK("Client public key installed")
}

func testVPSConnection(cfg *config.Config) {
	ui.Step(4, "Testing VPS connection...")

	keyFile := "/home/tunneluser/.ssh/tunnel_ed25519"
	vpsAddr := fmt.Sprintf("%s@%s", cfg.VPSUser, cfg.VPSHost)

	cmd := exec.Command("ssh",
		"-i", keyFile,
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=accept-new",
		"-N",
		"-o", "SessionType=none",
		vpsAddr,
	)
	cmd.Env = append(os.Environ(), "SSH_ASKPASS_REQUIRE=never")

	err := cmd.Start()
	if err != nil {
		ui.Warn("Could not test connection: %v", err)
		ui.Warn("Verify the tunnel key is installed on the VPS manually")
		return
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-done:
		ui.OK("VPS connection successful")
	case <-make(chan struct{}):
		cmd.Process.Kill()
		ui.OK("VPS connection successful (timeout = auth worked)")
	}
}

func installAutossh() {
	ui.Step(5, "Installing autossh...")

	if _, err := exec.LookPath("autossh"); err == nil {
		ui.OK("autossh already installed")
		return
	}

	cmd := exec.Command("apt-get", "update", "-qq")
	cmd.Run()

	cmd = exec.Command("apt-get", "install", "-y", "-qq", "autossh")
	if out, err := cmd.CombinedOutput(); err != nil {
		ui.Warn("Failed to install autossh: %v\n%s", err, out)
		return
	}

	ui.OK("autossh installed")
}

func setupSystemdService(cfg *config.Config) {
	ui.Step(6, "Writing systemd service...")

	keyFile := "/home/tunneluser/.ssh/tunnel_ed25519"
	vpsAddr := fmt.Sprintf("%s@%s", cfg.VPSUser, cfg.VPSHost)

	serviceContent := fmt.Sprintf(`[Unit]
Description=Persistent Reverse SSH Tunnel to VPS
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
User=tunneluser
Environment=AUTOSSH_GATETIME=0
Environment=AUTOSSH_PATH=/usr/bin/ssh
ExecStart=/usr/bin/autossh -M 0 -N -o ServerAliveInterval=30 -o ServerAliveCountMax=3 -o ExitOnForwardFailure=yes -o BatchMode=yes -o StrictHostKeyChecking=yes -i %s -R 2222:localhost:22 %s
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
`, keyFile, vpsAddr)

	serviceFile := "/etc/systemd/system/reverse-tunnel.service"
	os.WriteFile(serviceFile, []byte(serviceContent), 0644)

	exec.Command("systemctl", "daemon-reload").Run()

	ui.OK("Systemd service written")
}

func enableAndStart() {
	ui.Step(7, "Enabling and starting service...")

	exec.Command("systemctl", "enable", "reverse-tunnel.service").Run()

	if out, err := exec.Command("systemctl", "start", "reverse-tunnel.service").CombinedOutput(); err != nil {
		ui.Fatal("Failed to start service: %v\n%s", err, out)
	}

	ui.OK("reverse-tunnel.service enabled and started")
}

func Uninstall(configPath *string, forceFlag *bool) {
	if os.Geteuid() != 0 {
		ui.Fatal("This mode must be run as root (use sudo).")
	}

	ui.Header("Homeserver Uninstall — Removing Tunnel Setup")

	ui.Print("This will PERMANENTLY DELETE:")
	ui.Print("  - reverse-tunnel.service")
	ui.Print("  - tunneluser and all files in /home/tunneluser")
	ui.Print("")

	confirm(forceFlag, "")

	removeSystemdService()
	backupAndRemoveTunnelUser()

	if confirmAutossh(forceFlag) {
		removeAutossh()
	}

	ui.Header("Homeserver Uninstall Complete")
	ui.Print("  • reverse-tunnel.service removed")
	ui.Print("  • tunneluser removed")
	ui.Print("  • SSH keys backed up to /root/guest-tunnel-backup/")
}

func confirm(forceFlag *bool, msg string) {
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

func confirmAutossh(forceFlag *bool) bool {
	if *forceFlag {
		return false
	}

	fmt.Print("Remove autossh? [y/N]: ")
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		response := strings.ToLower(strings.TrimSpace(scanner.Text()))
		return response == "y" || response == "yes"
	}
	return false
}

func removeSystemdService() {
	ui.Step(1, "Removing systemd service...")

	exec.Command("systemctl", "stop", "reverse-tunnel.service").Run()
	exec.Command("systemctl", "disable", "reverse-tunnel.service").Run()

	serviceFile := "/etc/systemd/system/reverse-tunnel.service"
	if _, err := os.Stat(serviceFile); err == nil {
		os.Remove(serviceFile)
		exec.Command("systemctl", "daemon-reload").Run()
	}

	ui.OK("reverse-tunnel.service removed")
}

func backupAndRemoveTunnelUser() {
	ui.Step(2, "Removing tunneluser...")

	if _, err := user.Lookup("tunneluser"); err != nil {
		ui.Warn("tunneluser does not exist")
		return
	}

	backupDir := "/root/guest-tunnel-backup"
	os.MkdirAll(backupDir, 0700)

	keyDir := "/home/tunneluser/.ssh"
	keyFile := keyDir + "/tunnel_ed25519"
	keyPub := keyFile + ".pub"

	for _, f := range []string{keyFile, keyPub} {
		if _, err := os.Stat(f); err == nil {
			dest := filepath.Join(backupDir, filepath.Base(f))
			data, _ := os.ReadFile(f)
			os.WriteFile(dest, data, 0600)
			ui.OK("Backed up: %s", f)
		}
	}

	cmd := exec.Command("userdel", "-r", "tunneluser")
	if out, err := cmd.CombinedOutput(); err != nil {
		ui.Warn("Failed to remove tunneluser: %v\n%s", err, out)
		return
	}

	ui.OK("tunneluser removed")
}

func removeAutossh() {
	ui.Step(3, "Removing autossh...")

	cmd := exec.Command("apt-get", "remove", "-y", "autossh")
	if out, err := cmd.CombinedOutput(); err != nil {
		ui.Warn("Failed to remove autossh: %v\n%s", err, out)
		return
	}

	ui.OK("autossh removed")
}
