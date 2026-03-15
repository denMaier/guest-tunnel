package tunnel

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"
)

// Config holds everything needed to build the two-gate tunnel.
type Config struct {
	SSHBin     string
	AgentSock  string
	VPSUser    string
	VPSHost    string
	VPSPort    string
	HomeUser   string
	TunnelPort string // reverse tunnel port on VPS (e.g., 2222)
	SOCKSPort  string
	SOCKSBind  string
}

// Tunnel represents a live SSH tunnel process.
type Tunnel struct {
	cmd  *exec.Cmd
	dead chan error
}

// Dead returns a channel that receives when the tunnel process exits.
func (t *Tunnel) Dead() <-chan error { return t.dead }

// Close terminates the tunnel process.
func (t *Tunnel) Close() {
	if t.cmd != nil && t.cmd.Process != nil {
		t.cmd.Process.Kill()
		t.cmd.Wait()
	}
}

// Establish brings up:
//
//	ssh -o ProxyJump=<VPSUser>@<VPSHost> \
//	    -o StrictHostKeyChecking=accept-new \
//	    -o IdentityAgent=<AgentSock> \
//	    -D <SOCKSPort> \
//	    -N \
//	    -p <TunnelPort> \
//	    <HomeUser>@localhost
//
// Gate 1 (VPS) and Gate 2 (homeserver) are each individually authenticated
// using the resident FIDO2 key held in our private agent. The user may be
// prompted to touch the YubiKey twice — once per gate.
func Establish(cfg Config) (*Tunnel, error) {
	// Verify the SOCKS port is free before we start
	if err := portFree(cfg.SOCKSBind + ":" + cfg.SOCKSPort); err != nil {
		return nil, fmt.Errorf("SOCKS port %s:%s is already in use: %w", cfg.SOCKSBind, cfg.SOCKSPort, err)
	}

	// Build the ProxyJump string with optional non-standard port
	proxyJump := fmt.Sprintf("%s@%s", cfg.VPSUser, cfg.VPSHost)
	if cfg.VPSPort != "" && cfg.VPSPort != "22" {
		proxyJump = fmt.Sprintf("%s@%s:%s", cfg.VPSUser, cfg.VPSHost, cfg.VPSPort)
	}

	// Connect via reverse tunnel: localhost:<tunnelPort>
	// The VPS has a reverse tunnel bound to localhost:<TunnelPort> -> homeserver:22
	homeAddr := fmt.Sprintf("%s@localhost", cfg.HomeUser)
	homePortArgs := []string{"-p", cfg.TunnelPort}

	socksAddr := fmt.Sprintf("%s:%s", cfg.SOCKSBind, cfg.SOCKSPort)

	args := []string{
		"-o", fmt.Sprintf("ProxyJump=%s", proxyJump),
		"-o", fmt.Sprintf("IdentityAgent=%s", cfg.AgentSock),
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=/dev/null",
		"-N",
		"-T",
		"-D", socksAddr,
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "LogLevel=ERROR",
	}
	args = append(args, homePortArgs...)
	args = append(args, homeAddr)

	cmd := exec.Command(cfg.SSHBin, args...)
	cmd.Env = append(os.Environ(),
		"SSH_AUTH_SOCK="+cfg.AgentSock,
		// Forward the touch prompt to the terminal
		"SSH_ASKPASS_REQUIRE=never",
	)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start SSH: %w", err)
	}

	// Wait for the SOCKS5 port to become available — this confirms the tunnel
	// has cleared both gates and is ready to forward traffic.
	if err := waitForPort(socksAddr, 30*time.Second); err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("tunnel did not come up within 30s (both gates must accept the FIDO2 key): %w", err)
	}

	dead := make(chan error, 1)
	go func() {
		dead <- cmd.Wait()
	}()

	return &Tunnel{cmd: cmd, dead: dead}, nil
}

// -------------------------------------------------------------------------- //
// Helpers                                                                     //
// -------------------------------------------------------------------------- //

func portFree(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	ln.Close()
	return nil
}

func waitForPort(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", addr)
}
