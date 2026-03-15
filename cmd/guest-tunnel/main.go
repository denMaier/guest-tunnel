package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/yourusername/guest-tunnel/internal/agent"
	"github.com/yourusername/guest-tunnel/internal/config"
	homeserver "github.com/yourusername/guest-tunnel/internal/home"
	"github.com/yourusername/guest-tunnel/internal/proxy"
	"github.com/yourusername/guest-tunnel/internal/server"
	"github.com/yourusername/guest-tunnel/internal/tunnel"
	"github.com/yourusername/guest-tunnel/internal/ui"
)

// Build-time only — server config is now in config.yml, not ldflags.
//
// Set via:  -ldflags "-X main.Version=v1.2.3"
var (
	Version      = "dev"
	SSHBinaryURL = ""
)

func main() {
	var (
		mode          = flag.String("mode", "client", "mode: client, home, home-uninstall, server, or server-uninstall")
		configPath    = flag.String("config", "", "path to config.yml (optional — see search order in README)")
		initFlag      = flag.Bool("init", false, "write an example config.yml to ~/.config/guest-tunnel/config.yml and exit")
		versionFlag   = flag.Bool("version", false, "print version and exit")
		externalAgent = flag.Bool("external-agent", false, "use existing SSH agent (KeepassXC, StrongBox, etc.) instead of YubiKey")
		forceFlag     = flag.Bool("force", false, "skip confirmation prompts (for uninstall)")
	)

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: guest-tunnel [flags]\n\nFlags:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nModes:\n")
		fmt.Fprintf(os.Stderr, "  client           — connect to homeserver via VPS using YubiKey (default)\n")
		fmt.Fprintf(os.Stderr, "  home             — run reverse tunnel on homeserver as systemd service\n")
		fmt.Fprintf(os.Stderr, "  home-uninstall   — remove homeserver tunnel setup\n")
		fmt.Fprintf(os.Stderr, "  server           — setup VPS (jump host) configuration\n")
		fmt.Fprintf(os.Stderr, "  server-uninstall — remove VPS jump host setup\n\n")
		fmt.Fprintf(os.Stderr, "Config file search order:\n")
		fmt.Fprintf(os.Stderr, "  1. --config flag\n")
		fmt.Fprintf(os.Stderr, "  2. $GUEST_TUNNEL_CONFIG env var\n")
		fmt.Fprintf(os.Stderr, "  3. ./config.yml\n")
		fmt.Fprintf(os.Stderr, "  4. ~/.config/guest-tunnel/config.yml\n")
		fmt.Fprintf(os.Stderr, "  5. ~/.guest-tunnel.yml\n\n")
		fmt.Fprintf(os.Stderr, "Run with --init to generate a starter config.\n")
	}
	flag.Parse()

	ui.Banner(Version)

	switch *mode {
	case "client":
		runClientMode(configPath, initFlag, versionFlag, externalAgent)
	case "home":
		homeserver.Run(configPath, initFlag)
	case "home-uninstall":
		homeserver.Uninstall(configPath, forceFlag)
	case "server":
		server.Run(configPath, initFlag)
	case "server-uninstall":
		server.Uninstall(configPath, forceFlag)
	default:
		ui.Fatal("Invalid mode: %s (use client, home, home-uninstall, server, or server-uninstall)", *mode)
	}
}

func runClientMode(configPath *string, initFlag *bool, versionFlag *bool, externalAgent *bool) {
	if *versionFlag {
		fmt.Println(Version)
		os.Exit(0)
	}

	if *initFlag {
		if err := writeExampleConfig(); err != nil {
			ui.Fatal("Could not write example config: %v", err)
		}
		os.Exit(0)
	}

	// ------------------------------------------------------------------ //
	// Step 2 — Load config                                                 //
	// ------------------------------------------------------------------ //
	ui.Step(1, "Loading configuration...")
	ui.Hint("Config: %s", config.ConfigPath(*configPath))

	cfg, err := config.Load(*configPath)
	if err != nil {
		ui.Fatal("%v\n\nRun with --init to create a starter config.", err)
	}
	ui.OK("VPS:        %s@%s:%s", cfg.VPSUser, cfg.VPSHost, cfg.VPSPort)
	ui.OK("Homeserver: %s@localhost:%s (via reverse tunnel)", cfg.HomeUser, cfg.TunnelPort)
	ui.OK("SOCKS5:     %s:%s", cfg.SOCKSBind, cfg.SOCKSPort)

	// ------------------------------------------------------------------ //
	// Step 3 — Locate FIDO2-capable SSH binary                            //
	// ------------------------------------------------------------------ //
	ui.Step(2, "Locating FIDO2-capable SSH binary...")
	sshBin, sshCleanup, err := agent.FindOrFetchSSH(SSHBinaryURL)
	if err != nil {
		ui.Fatal("Could not find a usable SSH binary: %v", err)
	}
	defer sshCleanup()
	ui.OK("Using: %s", sshBin)

	// ------------------------------------------------------------------ //
	// Step 4 — Start ssh-agent and load keys                               //
	// ------------------------------------------------------------------ //
	if *externalAgent {
		ui.Step(3, "Using external SSH agent (KeepassXC, StrongBox, etc.)...")
	} else {
		ui.Step(3, "Starting private ssh-agent and loading YubiKey resident key...")
		ui.Hint("Touch your YubiKey when the light flashes.")
	}

	a, err := agent.Start(sshBin, *externalAgent)
	if err != nil {
		ui.Fatal("Failed to start ssh-agent: %v", err)
	}
	defer a.Stop()

	if *externalAgent {
		ui.OK("Using external agent: %s", a.SocketPath())
	} else {
		if err := a.LoadResidentKey(); err != nil {
			ui.Fatal(
				"Failed to load resident key from YubiKey: %v\n\n"+
					"  • Make sure your YubiKey is inserted\n"+
					"  • Make sure it has a resident FIDO2 credential (see README: Enrollment)\n"+
					"  • On Linux, check that udev rules allow FIDO HID access without sudo",
				err,
			)
		}
		ui.OK("Resident key loaded into agent")
	}

	// ------------------------------------------------------------------ //
	// Step 5 — Establish the two-gate tunnel                              //
	// ------------------------------------------------------------------ //
	ui.Step(4, "Establishing two-gate SSH tunnel...")
	ui.Hint("Gate 1 → VPS        (%s@%s:%s)", cfg.VPSUser, cfg.VPSHost, cfg.VPSPort)
	ui.Hint("Gate 2 → Homeserver (%s@localhost:%s via jump)", cfg.HomeUser, cfg.TunnelPort)
	ui.Hint("Touch your YubiKey when prompted.")

	tcfg := tunnel.Config{
		SSHBin:     sshBin,
		AgentSock:  a.SocketPath(),
		VPSUser:    cfg.VPSUser,
		VPSHost:    cfg.VPSHost,
		VPSPort:    cfg.VPSPort,
		HomeUser:   cfg.HomeUser,
		TunnelPort: cfg.TunnelPort,
		SOCKSPort:  cfg.SOCKSPort,
		SOCKSBind:  cfg.SOCKSBind,
	}

	t, err := tunnel.Establish(tcfg)
	if err != nil {
		ui.Fatal("Failed to establish tunnel: %v", err)
	}
	defer t.Close()
	ui.OK("Both gates cleared — tunnel is up")

	// ------------------------------------------------------------------ //
	// Step 6 — Configure proxy                                            //
	// ------------------------------------------------------------------ //
	ui.Step(5, "Configuring system proxy...")
	proxyCleanup, err := proxy.Configure(cfg.SOCKSPort)
	if err != nil {
		ui.Warn("Could not auto-configure proxy: %v", err)
		ui.Hint("Set manually: SOCKS5 proxy → localhost:%s", cfg.SOCKSPort)
	} else {
		defer proxyCleanup()
	}

	// ------------------------------------------------------------------ //
	// Step 7 — Live                                                        //
	// ------------------------------------------------------------------ //
	ui.Step(6, "Tunnel is live.")
	ui.PrintConnectionInfo(cfg.SOCKSBind, cfg.SOCKSPort)

	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sigC:
		fmt.Println()
		ui.Step(7, "Shutting down and cleaning up...")
	case err := <-t.Dead():
		ui.Warn("Tunnel exited unexpectedly: %v", err)
		ui.Step(7, "Cleaning up...")
	}

	ui.OK("Done. Goodbye.")
}

func writeExampleConfig() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot determine home directory: %w", err)
	}
	dir := homeDir + "/.config/guest-tunnel"
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("cannot create %s: %w", dir, err)
	}
	dest := dir + "/config.yml"
	if _, err := os.Stat(dest); err == nil {
		fmt.Printf("Config already exists at %s\n", dest)
		fmt.Println("Delete it first if you want to regenerate.")
		return nil
	}
	if err := os.WriteFile(dest, []byte(config.Example()), 0600); err != nil {
		return fmt.Errorf("cannot write %s: %w", dest, err)
	}
	fmt.Printf("Example config written to: %s\n\n", dest)
	fmt.Println("Edit it with your VPS and homeserver details, then run guest-tunnel again.")
	return nil
}
