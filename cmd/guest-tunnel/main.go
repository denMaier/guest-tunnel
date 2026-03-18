package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/yourusername/guest-tunnel/internal/agent"
	"github.com/yourusername/guest-tunnel/internal/client"
	"github.com/yourusername/guest-tunnel/internal/config"
	homeserver "github.com/yourusername/guest-tunnel/internal/home"
	"github.com/yourusername/guest-tunnel/internal/proxy"
	"github.com/yourusername/guest-tunnel/internal/server"
	"github.com/yourusername/guest-tunnel/internal/tunnel"
	"github.com/yourusername/guest-tunnel/internal/ui"
)

var Version = "dev"

func main() {
	var (
		mode        = flag.String("mode", "client", "mode: client, client-setup, home, home-uninstall, server, or server-uninstall")
		configPath  = flag.String("config", "", "path to config.yml (optional)")
		initFlag    = flag.Bool("init", false, "write an example config.yml and exit")
		versionFlag = flag.Bool("version", false, "print version and exit")
		forceFlag   = flag.Bool("force", false, "skip confirmation prompts (for uninstall)")
		noReconnect = flag.Bool("no-reconnect", false, "exit immediately on tunnel failure (default: auto-reconnect)")
		agentSock   = flag.String("agent-sock", "", "SSH agent socket path (overrides SSH_AUTH_SOCK)")
		identity    = flag.String("identity", "", "SSH private key file (e.g. ~/.ssh/id_ed25519)")
	)

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: guest-tunnel [flags]\n\nFlags:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nModes:\n")
		fmt.Fprintf(os.Stderr, "  client           — open the two-gate SOCKS5 tunnel (default)\n")
		fmt.Fprintf(os.Stderr, "  client-setup     — write ~/.ssh/config and ~/bin helper scripts (run once)\n")
		fmt.Fprintf(os.Stderr, "  home             — set up homeserver: tunneluser, Dropbear/OpenSSH, autossh service\n")
		fmt.Fprintf(os.Stderr, "  home-uninstall   — remove homeserver tunnel setup\n")
		fmt.Fprintf(os.Stderr, "  server           — set up VPS: jumpuser, sshd hardening, fail2ban\n")
		fmt.Fprintf(os.Stderr, "  server-uninstall — remove VPS jump host setup\n\n")
		fmt.Fprintf(os.Stderr, "Authentication (client mode — pick one):\n")
		fmt.Fprintf(os.Stderr, "  SSH_AUTH_SOCK env var   existing agent (used automatically if set)\n")
		fmt.Fprintf(os.Stderr, "  --agent-sock <path>     explicit agent socket\n")
		fmt.Fprintf(os.Stderr, "  --identity <path>       SSH private key file\n")
		fmt.Fprintf(os.Stderr, "  --no-reconnect          exit immediately on tunnel failure (default: auto-reconnect)\n\n")
		fmt.Fprintf(os.Stderr, "Run with --init to generate a starter config.\n")
	}
	flag.Parse()

	ui.Banner(Version)

	switch *mode {
	case "client":
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
		runClient(*configPath, *agentSock, *identity, *noReconnect)
	case "client-setup":
		client.Setup(configPath, initFlag)
	case "home":
		homeserver.Run(configPath, initFlag)
	case "home-uninstall":
		homeserver.Uninstall(configPath, forceFlag)
	case "server":
		server.Run(configPath, initFlag)
	case "server-uninstall":
		server.Uninstall(configPath, forceFlag)
	default:
		ui.Fatal("Invalid mode: %s", *mode)
	}
}

func runClient(configPath, agentSock, identity string, noReconnect bool) {
	// ── Load config ───────────────────────────────────────────────────────────
	ui.Step(1, "Loading configuration...")
	ui.Hint("Config: %s", config.ConfigPath(configPath))

	cfg, err := config.Load(configPath)
	if err != nil {
		ui.Fatal("%v\n\nRun with --init to create a starter config.", err)
	}
	ui.OK("VPS:        %s@%s:%s", cfg.VPSUser, cfg.VPSHost, cfg.VPSPort)
	ui.OK("Homeserver: %s@localhost:%s (via reverse tunnel)", cfg.HomeUser, cfg.TunnelPort)
	ui.OK("SOCKS5:     %s:%s", cfg.SOCKSBind, cfg.SOCKSPort)

	// ── Resolve authentication ────────────────────────────────────────────────
	ui.Step(2, "Resolving SSH authentication...")

	auth, err := agent.Resolve(agentSock, identity)
	if err != nil {
		ui.Fatal("%v", err)
	}

	switch {
	case auth.AgentSock != "":
		ui.OK("Auth: agent socket %s", auth.AgentSock)
	case auth.IdentityFile != "":
		ui.OK("Auth: identity file %s", auth.IdentityFile)
	}

	// ── Establish the two-gate tunnel ─────────────────────────────────────────
	ui.Step(3, "Establishing two-gate SSH tunnel...")
	ui.Hint("Gate 1 → VPS        (%s@%s:%s)", cfg.VPSUser, cfg.VPSHost, cfg.VPSPort)
	ui.Hint("Gate 2 → Homeserver (%s@localhost:%s via jump)", cfg.HomeUser, cfg.TunnelPort)

	tcfg := tunnel.Config{
		Auth:       auth,
		VPSUser:    cfg.VPSUser,
		VPSHost:    cfg.VPSHost,
		VPSPort:    cfg.VPSPort,
		HomeUser:   cfg.HomeUser,
		TunnelPort: cfg.TunnelPort,
		SOCKSPort:  cfg.SOCKSPort,
		SOCKSBind:  cfg.SOCKSBind,
		Reconnect:  !noReconnect,
	}

	t, err := tunnel.Establish(tcfg)
	if err != nil {
		ui.Fatal("Failed to establish tunnel: %v", err)
	}
	defer t.Close()
	ui.OK("Both gates cleared — tunnel is up")

	// ── Configure proxy ───────────────────────────────────────────────────────
	ui.Step(4, "Configuring system proxy...")
	proxyCleanup, err := proxy.Configure(cfg.SOCKSPort)
	if err != nil {
		ui.Warn("Could not auto-configure proxy: %v", err)
		ui.Hint("Set manually: SOCKS5 proxy → localhost:%s", cfg.SOCKSPort)
	} else {
		defer proxyCleanup()
	}

	// ── Live ──────────────────────────────────────────────────────────────────
	ui.Step(5, "Tunnel is live.")
	ui.PrintConnectionInfo(cfg.SOCKSBind, cfg.SOCKSPort)

	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGINT, syscall.SIGTERM)

	if noReconnect {
		select {
		case <-sigC:
			fmt.Println()
			ui.Step(6, "Shutting down...")
		case err := <-t.Dead():
			ui.Warn("Tunnel exited unexpectedly: %v", err)
			ui.Step(6, "Cleaning up...")
		}
	} else {
		ui.Hint("Tunnel will auto-reconnect on failure. Press Ctrl+C to shut down.")
		<-sigC
		fmt.Println()
		ui.Step(6, "Shutting down...")
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
