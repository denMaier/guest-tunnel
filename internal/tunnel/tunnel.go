package tunnel

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/yourusername/guest-tunnel/internal/agent"
)

// Config holds everything needed to build the two-gate tunnel.
type Config struct {
	Auth       *agent.Auth
	VPSUser    string
	VPSHost    string
	VPSPort    string
	HomeUser   string
	TunnelPort string // reverse tunnel port on VPS (e.g. 2222)
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

// Establish brings up a two-gate SOCKS5 tunnel:
//
//	ssh -o ProxyJump=<VPSUser>@<VPSHost>[:<VPSPort>] \
//	    -o StrictHostKeyChecking=accept-new \
//	    [-o IdentityAgent=<sock> | -i <keyfile>] \
//	    -D <SOCKSBind>:<SOCKSPort> \
//	    -N -T \
//	    -p <TunnelPort> \
//	    <HomeUser>@localhost
func Establish(cfg Config) (*Tunnel, error) {
	sshBin, err := agent.SSHBin()
	if err != nil {
		return nil, err
	}

	if err := portFree(cfg.SOCKSBind + ":" + cfg.SOCKSPort); err != nil {
		return nil, fmt.Errorf("SOCKS port %s:%s already in use: %w", cfg.SOCKSBind, cfg.SOCKSPort, err)
	}

	proxyJump := fmt.Sprintf("%s@%s", cfg.VPSUser, cfg.VPSHost)
	if cfg.VPSPort != "" && cfg.VPSPort != "22" {
		proxyJump = fmt.Sprintf("%s@%s:%s", cfg.VPSUser, cfg.VPSHost, cfg.VPSPort)
	}

	socksAddr := fmt.Sprintf("%s:%s", cfg.SOCKSBind, cfg.SOCKSPort)

	args := []string{
		"-o", fmt.Sprintf("ProxyJump=%s", proxyJump),
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "LogLevel=ERROR",
		"-N", "-T",
		"-D", socksAddr,
		"-p", cfg.TunnelPort,
	}

	// Auth: agent socket or identity file
	switch {
	case cfg.Auth.AgentSock != "":
		args = append(args, "-o", fmt.Sprintf("IdentityAgent=%s", cfg.Auth.AgentSock))
	case cfg.Auth.IdentityFile != "":
		args = append(args, "-i", cfg.Auth.IdentityFile)
	}

	args = append(args, fmt.Sprintf("%s@localhost", cfg.HomeUser))

	cmd := exec.Command(sshBin, args...)
	env := os.Environ()
	if cfg.Auth.AgentSock != "" {
		env = append(env, "SSH_AUTH_SOCK="+cfg.Auth.AgentSock)
	}
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start ssh: %w", err)
	}

	if err := waitForPort(socksAddr, 30*time.Second); err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("tunnel did not come up within 30s: %w", err)
	}

	dead := make(chan error, 1)
	go func() { dead <- cmd.Wait() }()

	return &Tunnel{cmd: cmd, dead: dead}, nil
}

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
