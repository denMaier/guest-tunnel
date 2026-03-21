package home

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yourusername/guest-tunnel/internal/config"
	"github.com/yourusername/guest-tunnel/internal/sysutil"
	"github.com/yourusername/guest-tunnel/internal/ui"
)

const (
	keyFile   = "/home/tunneluser/.ssh/tunnel_ed25519"
	keyDir    = "/home/tunneluser/.ssh"
	dbKeyDir  = "/etc/dropbear"
	dbKeyFile = "/etc/dropbear/dropbear_ed25519_host_key"
	dbService = "/etc/systemd/system/dropbear-ssh.service"
	rvService = "/etc/systemd/system/reverse-tunnel.service"
	testPort  = "2223"
)

type sshDaemon struct {
	kind string
	port string
}

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

	if err := cfg.Validate("home"); err != nil {
		ui.Fatal("Config error:\n  %v", err)
	}

	setupHome(cfg)
}

func writeExampleConfig() {
	if err := config.WriteExample("/etc/guest-tunnel/config.yml", "home"); err != nil {
		ui.Fatal("Could not write example config: %v", err)
	}
}

func setupHome(cfg *config.Config) {
	ui.Header("Homeserver Setup — Persistent Reverse Tunnel")

	daemon := detectSSHDaemon()

	setupTunnelUser()
	generateSSHKey()
	installClientPublicKey(cfg)
	testVPSConnection(cfg)
	installAutossh()
	populateKnownHosts(cfg)

	useDropbear := chooseDaemon(daemon, cfg)
	if useDropbear {
		setupDropbear(daemon.port, cfg)
	} else {
		ensureOpenSSH()
	}

	setupSystemdService(cfg)
	enableAndStart()

	ui.Header("Homeserver Setup Complete — Summary")
	ui.Print("  • tunneluser created (unprivileged, nologin shell)")
	ui.Print("  • ed25519 keypair generated")
	ui.Print("  • reverse-tunnel.service installed and running")
	ui.Print("  • Tunnel: homeserver:22 → VPS:localhost:%s", cfg.TunnelPort)
	if useDropbear {
		ui.Print("  • Inbound daemon: Dropbear (password auth disabled: -s -g)")
	} else {
		ui.Print("  • Inbound daemon: OpenSSH")
	}
	ui.Print("")
	ui.Print("Next: Run this binary with --mode=client on your laptop.")
}

func setupTunnelUser() {
	ui.Step(1, "Creating tunneluser...")

	if _, err := user.Lookup("tunneluser"); err == nil {
		ui.OK("tunneluser already exists")
		sysutil.EnsureServiceUserState("tunneluser", "/usr/sbin/nologin")
		return
	}

	cmd := exec.Command("useradd", "--system", "--shell", "/usr/sbin/nologin", "--create-home", "tunneluser")
	if out, err := cmd.CombinedOutput(); err != nil {
		ui.Fatal("Failed to create tunneluser: %v\n%s", err, out)
	}

	sysutil.EnsureServiceUserState("tunneluser", "/usr/sbin/nologin")
	ui.OK("Created tunneluser")
}

func generateSSHKey() {
	ui.Step(2, "Generating SSH keypair...")

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

	akDir := "/home/tunneluser/.ssh"
	akFile := akDir + "/authorized_keys"

	os.MkdirAll(akDir, 0700)
	// sshd drops privileges to tunneluser before reading authorized_keys,
	// so .ssh/ and authorized_keys must be owned by tunneluser.
	if u, err := user.Lookup("tunneluser"); err == nil {
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		os.Chown(akDir, uid, gid)
	}
	os.Chmod(akDir, 0700)

	existingKeys := sysutil.ReadPublicKeys(akFile)
	if len(existingKeys) > 0 {
		ui.OK("%d client key(s) already installed — preserving existing access", len(existingKeys))
		for _, k := range existingKeys {
			fields := strings.Fields(k)
			comment := ""
			if len(fields) >= 3 {
				comment = fields[2]
			}
			ui.Hint("  %s", comment)
		}
		ui.OK("Skipping client key installation on rerun")
		return
	}

	var key string
	if cfg.LaptopPubKey != "" {
		key = strings.TrimSpace(cfg.LaptopPubKey)
		ui.OK("Using laptop_pubkey from config")
	} else {
		fmt.Printf("\n  Paste the laptop's SSH public key (single line):\n  ")
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			ui.Warn("No key provided - skipping")
			return
		}
		key = strings.TrimSpace(scanner.Text())
		if key != "" {
			cfg.LaptopPubKey = key
			if err := cfg.Save("/etc/guest-tunnel/config.yml"); err != nil {
				ui.Warn("Could not save config: %v", err)
			} else {
				ui.OK("Saved laptop_pubkey to config for future non-interactive runs")
			}
		}
	}

	if key == "" {
		ui.Warn("No key provided - skipping")
		return
	}

	f, err := os.OpenFile(akFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		ui.Fatal("Failed to write authorized_keys: %v", err)
	}
	f.WriteString(key + "\n")
	f.Close()

	if u, err := user.Lookup("tunneluser"); err == nil {
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		os.Chown(akFile, uid, gid)
	}
	os.Chmod(akFile, 0600)

	ui.OK("Client public key installed")
}

func testVPSConnection(cfg *config.Config) {
	ui.Step(4, "Testing VPS connection...")

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
	case err := <-done:
		if err != nil {
			ui.Warn("VPS connection failed: %v", err)
			ui.Warn("Verify the tunnel key is installed on the VPS")
		} else {
			ui.OK("VPS connection successful")
		}
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		ui.Warn("VPS connection timed out — check firewall/network")
		ui.Hint("Verify the tunnel key is installed on the VPS and port 22 is open")
	}
}

func installAutossh() {
	ui.Step(5, "Installing autossh...")

	if _, err := exec.LookPath("autossh"); err == nil {
		ui.OK("autossh already installed")
		return
	}

	exec.Command("apt-get", "update", "-qq").Run()

	cmd := exec.Command("apt-get", "install", "-y", "-qq", "autossh")
	if out, err := cmd.CombinedOutput(); err != nil {
		ui.Warn("Failed to install autossh: %v\n%s", err, out)
		return
	}

	ui.OK("autossh installed")
}

func populateKnownHosts(cfg *config.Config) {
	ui.Step(6, "Pre-populating known_hosts for tunneluser...")

	knownHosts := keyDir + "/known_hosts"

	if data, err := os.ReadFile(knownHosts); err == nil {
		if strings.Contains(string(data), cfg.VPSHost) {
			ui.OK("VPS host key already in known_hosts")
			return
		}
	}

	out, err := exec.Command("ssh-keyscan", "-T", "10", cfg.VPSHost).Output()
	if err != nil || len(out) == 0 {
		ui.Warn("Could not reach %s to scan host key — add it manually", cfg.VPSHost)
		return
	}

	f, err := os.OpenFile(knownHosts, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		ui.Warn("Failed to write known_hosts: %v", err)
		return
	}
	f.Write(out)
	f.Close()

	os.Chown(knownHosts, tunnelUID(), tunnelGID())
	ui.OK("VPS host key added to tunneluser's known_hosts")
}

func detectSSHDaemon() sshDaemon {
	if isUnitActive("dropbear-ssh") {
		port := unitListenPort("dropbear-ssh", "22")
		return sshDaemon{kind: "dropbear", port: port}
	}
	for _, unit := range []string{"ssh", "sshd", "openssh-server"} {
		if isUnitActive(unit) {
			port := sshdListenPort("22")
			return sshDaemon{kind: "openssh", port: port}
		}
	}
	return sshDaemon{kind: "unknown", port: "22"}
}

func isUnitActive(unit string) bool {
	err := exec.Command("systemctl", "is-active", "--quiet", unit).Run()
	return err == nil
}

func unitListenPort(unit, fallback string) string {
	out, err := exec.Command("systemctl", "show", unit, "--property=ExecStart").Output()
	if err != nil {
		return fallback
	}
	s := string(out)
	idx := strings.Index(s, " -p ")
	if idx < 0 {
		return fallback
	}
	rest := strings.TrimSpace(s[idx+4:])
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return fallback
	}
	return fields[0]
}

func sshdListenPort(fallback string) string {
	out, err := exec.Command("ss", "-tlnp").Output()
	if err != nil {
		return fallback
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "sshd") {
			fields := strings.Fields(line)
			for _, f := range fields {
				if colon := strings.LastIndex(f, ":"); colon >= 0 {
					port := f[colon+1:]
					if port != "" && port != "*" {
						return port
					}
				}
			}
		}
	}
	return fallback
}

func chooseDaemon(current sshDaemon, cfg *config.Config) bool {
	if current.kind == "dropbear" {
		ui.Step(7, "Selecting SSH daemon...")
		ui.OK("Dropbear already active on port %s — preserving current daemon", current.port)
		return true
	}

	if current.kind == "openssh" {
		ui.Step(7, "Selecting SSH daemon...")
		ui.OK("OpenSSH already active on port %s — preserving current daemon", current.port)
		return false
	}

	// Non-interactive: use config value
	if cfg.SSHDaemon != "" {
		ui.Step(7, "Selecting SSH daemon...")
		d := strings.ToLower(cfg.SSHDaemon)
		if d == "openssh" {
			ui.OK("Using ssh_daemon from config: openssh")
			return false
		}
		ui.OK("Using ssh_daemon from config: dropbear")
		return true
	}

	ui.Header("SSH Server Selection")
	fmt.Println("  Choose the SSH server to run on this homeserver:")
	fmt.Printf("  %s1) OpenSSH%s  — standard, full-featured, larger codebase\n", ui.BOLD, ui.RESET)
	fmt.Printf("  %s2) Dropbear%s — minimal codebase (~10x smaller), separate zero-day pool\n", ui.BOLD, ui.RESET)
	fmt.Println()
	fmt.Print("  Choice [1/2, default 2 (Dropbear recommended)]: ")
	scanner := bufio.NewScanner(os.Stdin)
	chooseOpenSSH := scanner.Scan() && strings.TrimSpace(scanner.Text()) == "1"

	if chooseOpenSSH {
		cfg.SSHDaemon = "openssh"
	} else {
		cfg.SSHDaemon = "dropbear"
	}
	if err := cfg.Save("/etc/guest-tunnel/config.yml"); err != nil {
		ui.Warn("Could not save config: %v", err)
	}

	return !chooseOpenSSH
}

func ensureOpenSSH() {
	ui.Step(7, "Ensuring OpenSSH server is installed and running...")

	exec.Command("systemctl", "unmask", "ssh", "openssh-server").Run()

	if _, err := exec.LookPath("sshd"); err != nil {
		exec.Command("apt-get", "update", "-qq").Run()
		if out, err := exec.Command("apt-get", "install", "-y", "-qq", "openssh-server").CombinedOutput(); err != nil {
			ui.Fatal("Failed to install openssh-server: %v\n%s", err, out)
		}
		ui.OK("openssh-server installed")
	}

	for _, unit := range []string{"ssh", "openssh-server"} {
		if err := exec.Command("systemctl", "enable", "--now", unit).Run(); err == nil {
			ui.OK("OpenSSH server is active")
			return
		}
	}
	ui.Warn("Could not enable OpenSSH — check manually")
}

func setupDropbear(currentPort string, cfg *config.Config) {
	ui.Header("Installing Dropbear SSH Server")

	installDropbear()
	convertHostKey()

	dbPort := currentPort
	if isUnitActive("dropbear-ssh") {
		ui.OK("Keeping existing Dropbear listen port %s", dbPort)
	} else {
		dbPort = promptPort(currentPort, cfg)
	}

	writeDropbearService(dbPort)

	if isUnitActive("dropbear-ssh") {
		ui.OK("dropbear-ssh.service already active — restarting with updated config")
		exec.Command("systemctl", "daemon-reload").Run()
		exec.Command("systemctl", "restart", "dropbear-ssh").Run()
		return
	}

	testDropbear(dbPort, cfg)
	cutoverToDropbear(dbPort)
}

func installDropbear() {
	if out, err := exec.Command("dpkg", "-s", "dropbear-bin").CombinedOutput(); err == nil {
		_ = out
		ui.OK("Dropbear already installed")
		return
	}

	exec.Command("apt-get", "update", "-qq").Run()
	if out, err := exec.Command("apt-get", "install", "-y", "-qq", "dropbear-bin").CombinedOutput(); err != nil {
		ui.Fatal("Failed to install dropbear-bin: %v\n%s", err, out)
	}

	if _, err := exec.LookPath("dbclient"); err != nil {
		ui.Fatal("dbclient not found after installing dropbear-bin")
	}

	ui.OK("Dropbear installed")
}

func convertHostKey() {
	ui.Step(7, "Converting host key to Dropbear format...")

	os.MkdirAll(dbKeyDir, 0755)

	if _, err := os.Stat(dbKeyFile); err == nil {
		ui.OK("Dropbear ed25519 host key already exists — skipping conversion")
		return
	}

	opensshKey := "/etc/ssh/ssh_host_ed25519_key"
	if _, err := os.Stat(opensshKey); err == nil {
		if out, err := exec.Command("dropbearconvert", "openssh", "dropbear", opensshKey, dbKeyFile).CombinedOutput(); err != nil {
			ui.Fatal("dropbearconvert failed: %v\n%s", err, out)
		}
		os.Chmod(dbKeyFile, 0600)
		ui.OK("Converted OpenSSH ed25519 host key → Dropbear format (fingerprint unchanged)")
	} else {
		if out, err := exec.Command("dropbearkey", "-t", "ed25519", "-f", dbKeyFile).CombinedOutput(); err != nil {
			ui.Fatal("dropbearkey failed: %v\n%s", err, out)
		}
		os.Chmod(dbKeyFile, 0600)
		ui.Warn("No OpenSSH ed25519 key found — generated a fresh Dropbear host key")
		ui.Warn("Clients will see a host-key-changed warning. Run: ssh-keygen -R <homeserver-ip>")
	}
}

func promptPort(current string, cfg *config.Config) string {
	if cfg.DropbearPort != "" {
		ui.OK("Using dropbear_port from config: %s", cfg.DropbearPort)
		return cfg.DropbearPort
	}
	fmt.Printf("  Port for Dropbear to listen on [%s]: ", current)
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		p := strings.TrimSpace(scanner.Text())
		if p != "" {
			cfg.DropbearPort = p
			if err := cfg.Save("/etc/guest-tunnel/config.yml"); err != nil {
				ui.Warn("Could not save config: %v", err)
			}
			return p
		}
	}
	return current
}

func writeDropbearService(port string) {
	content := fmt.Sprintf(`[Unit]
Description=Dropbear SSH daemon (homeserver)
After=network.target
Documentation=man:dropbear(8)

[Service]
ExecStart=/usr/sbin/dropbear -F -E -s -g -p %s -r %s
Restart=on-failure
RestartSec=5
KillMode=process

[Install]
WantedBy=multi-user.target
`,
		port, dbKeyFile)

	existing, _ := os.ReadFile(dbService)
	if string(existing) == content {
		ui.OK("dropbear-ssh.service already up to date")
		return
	}

	if err := os.WriteFile(dbService, []byte(content), 0644); err != nil {
		ui.Fatal("Failed to write dropbear-ssh.service: %v", err)
	}
	exec.Command("systemctl", "daemon-reload").Run()
	ui.OK("dropbear-ssh.service written (password auth disabled)")
}

func testDropbear(realPort string, cfg *config.Config) {
	if cfg.SkipTest {
		ui.Step(8, "Skipping Dropbear interactive test (skip_test: true)")
		return
	}

	ui.Header(fmt.Sprintf("Testing Dropbear on temporary port %s", testPort))
	ui.Print("  Starting Dropbear alongside OpenSSH for verification...")

	exec.Command("pkill", "-f", "dropbear.*-p "+testPort).Run()
	time.Sleep(time.Second)

	cmd := exec.Command("/usr/sbin/dropbear", "-p", testPort, "-r", dbKeyFile, "-F", "-E", "-s", "-g")
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		ui.Fatal("Failed to start Dropbear test instance: %v", err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
		ui.OK("Test instance stopped")
	}()

	if !waitForPort("127.0.0.1:"+testPort, 5*time.Second) {
		ui.Fatal("Dropbear did not start on port %s — check: journalctl -xe", testPort)
	}
	ui.OK("Dropbear test instance running on port %s", testPort)

	fmt.Println()
	fmt.Printf("  %s%sACTION REQUIRED — verify Dropbear before continuing:%s\n", ui.BOLD, ui.YELLOW, ui.RESET)
	fmt.Println()
	fmt.Printf("  From your LAPTOP (in a separate terminal), run:\n")
	fmt.Printf("  %s  ssh -p %s <your-user>@<homeserver-ip>%s\n", ui.CYAN, testPort, ui.RESET)
	fmt.Println()
	fmt.Println("  If login succeeds, return here and press Enter.")
	fmt.Println("  If login fails, press Ctrl+C to abort — OpenSSH will remain untouched.")
	fmt.Println()
	fmt.Print("  >> Press Enter ONLY after successful Dropbear login on port " + testPort + "...")
	bufio.NewReader(os.Stdin).ReadString('\n')
}

func cutoverToDropbear(port string) {
	ui.Header(fmt.Sprintf("Switching from OpenSSH to Dropbear on port %s", port))

	for _, unit := range []string{"ssh.socket", "openssh.socket", "ssh", "openssh-server", "sshd"} {
		exec.Command("systemctl", "disable", "--now", unit).Run()
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !portInUse(port) {
			break
		}
		time.Sleep(time.Second)
	}
	if portInUse(port) {
		exec.Command("systemctl", "enable", "--now", "ssh").Run()
		ui.Fatal("Port %s still in use after stopping OpenSSH — rolled back. No changes made.", port)
	}
	ui.OK("OpenSSH stopped — port %s is free", port)

	exec.Command("systemctl", "enable", "dropbear-ssh.service").Run()
	if out, err := exec.Command("systemctl", "start", "dropbear-ssh.service").CombinedOutput(); err != nil {
		exec.Command("systemctl", "disable", "dropbear-ssh.service").Run()
		exec.Command("systemctl", "enable", "--now", "ssh").Run()
		ui.Fatal("Dropbear failed to start: %v\n%s\nOpenSSH has been restarted. No changes made.", err, out)
	}

	time.Sleep(2 * time.Second)
	if !isUnitActive("dropbear-ssh") {
		exec.Command("systemctl", "disable", "dropbear-ssh.service").Run()
		exec.Command("systemctl", "enable", "--now", "ssh").Run()
		ui.Fatal("Dropbear is not active after start — rolled back to OpenSSH")
	}
	ui.OK("dropbear-ssh.service is ACTIVE on port %s", port)

	for _, unit := range []string{"ssh", "ssh.socket", "openssh-server", "openssh.socket", "sshd"} {
		exec.Command("systemctl", "mask", unit).Run()
	}
	ui.OK("OpenSSH units masked — cannot be accidentally re-enabled")

	fmt.Println()
	ui.Print("  Security diversity achieved:")
	ui.Print("    VPS        → OpenSSH  (needs Match block expressiveness)")
	ui.Print("    Homeserver → Dropbear (separate codebase, ~10x smaller attack surface)")
	ui.Print("    A zero-day in OpenSSH does not automatically compromise the homeserver.")
	fmt.Println()
	ui.Print("  Rollback (if ever needed):")
	ui.Print("    systemctl unmask ssh ssh.socket")
	ui.Print("    systemctl enable --now ssh")
	ui.Print("    systemctl disable --now dropbear-ssh")
}

func setupSystemdService(cfg *config.Config) {
	ui.Step(8, "Writing reverse-tunnel.service...")

	vpsAddr := fmt.Sprintf("%s@%s", cfg.VPSUser, cfg.VPSHost)

	content := fmt.Sprintf(`[Unit]
Description=Persistent Reverse SSH Tunnel to VPS
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
User=tunneluser
Environment=AUTOSSH_GATETIME=0
Environment=AUTOSSH_PATH=/usr/bin/ssh
ExecStart=/usr/bin/autossh -M 0 -N -o ServerAliveInterval=30 -o ServerAliveCountMax=3 -o ExitOnForwardFailure=yes -o BatchMode=yes -o StrictHostKeyChecking=yes -i %s -R %s:localhost:22 %s
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
`, keyFile, cfg.TunnelPort, vpsAddr)

	existing, _ := os.ReadFile(rvService)
	if string(existing) == content {
		ui.OK("reverse-tunnel.service already up to date")
		return
	}

	if err := os.WriteFile(rvService, []byte(content), 0644); err != nil {
		ui.Fatal("Failed to write reverse-tunnel.service: %v", err)
	}
	exec.Command("systemctl", "daemon-reload").Run()
	ui.OK("reverse-tunnel.service written")
}

func enableAndStart() {
	ui.Step(9, "Enabling and starting reverse-tunnel.service...")

	exec.Command("systemctl", "enable", "reverse-tunnel.service").Run()

	if out, err := exec.Command("systemctl", "restart", "reverse-tunnel.service").CombinedOutput(); err != nil {
		ui.Fatal("Failed to start reverse-tunnel.service: %v\n%s", err, out)
	}

	time.Sleep(3 * time.Second)

	if isUnitActive("reverse-tunnel.service") {
		ui.OK("reverse-tunnel.service is ACTIVE")
	} else {
		ui.Fatal("reverse-tunnel.service failed to start — check: journalctl -u reverse-tunnel.service -n 30")
	}
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

	if _, err := os.Stat(rvService); err == nil {
		os.Remove(rvService)
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

	for _, f := range []string{keyFile, keyFile + ".pub"} {
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

func portInUse(port string) bool {
	out, err := exec.Command("ss", "-tlnp").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, ":"+port) {
			return true
		}
	}
	return false
}

func waitForPort(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

func tunnelUID() int {
	u, err := user.Lookup("tunneluser")
	if err != nil {
		return 0
	}
	var uid int
	fmt.Sscan(u.Uid, &uid)
	return uid
}

func tunnelGID() int {
	u, err := user.Lookup("tunneluser")
	if err != nil {
		return 0
	}
	var gid int
	fmt.Sscan(u.Gid, &gid)
	return gid
}
