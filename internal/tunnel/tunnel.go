package tunnel

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yourusername/guest-tunnel/internal/agent"
)

var (
	lookupSSHBin      = agent.SSHBin
	startVerifiedConn = startVerifiedTunnelCommand
)

type Config struct {
	Auth       *agent.Auth
	VPSUser    string
	VPSHost    string
	VPSPort    string
	HomeUser   string
	TunnelPort string
	SOCKSPort  string
	SOCKSBind  string
	Reconnect  bool
}

type Tunnel struct {
	cmd       *exec.Cmd
	dead      chan error
	stop      chan struct{}
	closeOnce sync.Once
}

func (t *Tunnel) Dead() <-chan error { return t.dead }

func (t *Tunnel) Close() {
	if t == nil {
		return
	}
	t.closeOnce.Do(func() {
		if t.stop != nil {
			close(t.stop)
		}
		if t.cmd != nil {
			terminateTunnelCommand(t.cmd)
		}
	})
}

func Establish(cfg Config) (*Tunnel, error) {
	if cfg.Reconnect {
		return establishWithReconnect(cfg)
	}
	return establishOnce(cfg)
}

func establishOnce(cfg Config) (*Tunnel, error) {
	sshBin, err := lookupSSHBin()
	if err != nil {
		return nil, err
	}

	socksAddr := fmt.Sprintf("%s:%s", cfg.SOCKSBind, cfg.SOCKSPort)
	cmd, err := startVerifiedConn(sshBin, cfg, socksAddr, "", false)
	if err != nil {
		return nil, err
	}

	dead := make(chan error, 1)
	go func() { dead <- cmd.Wait() }()

	return &Tunnel{cmd: cmd, dead: dead, stop: make(chan struct{})}, nil
}

func establishWithReconnect(cfg Config) (*Tunnel, error) {
	sshBin, err := lookupSSHBin()
	if err != nil {
		return nil, err
	}

	socksAddr := fmt.Sprintf("%s:%s", cfg.SOCKSBind, cfg.SOCKSPort)

	useControlMaster := cfg.Auth.AgentSocket() == "" && cfg.Auth.IdentityFile != ""

	var controlPath string
	if useControlMaster {
		controlPath, err = ensureControlSocketDir()
		if err != nil {
			return nil, fmt.Errorf("failed to create control socket directory: %w", err)
		}
	}

	tunnel := &Tunnel{
		cmd:  nil,
		dead: make(chan error, 1),
		stop: make(chan struct{}),
	}
	ready := make(chan error, 1)

	go func() {
		firstAttempt := true
		for {
			if tunnel.shouldStop() {
				return
			}

			cmd, err := startVerifiedConn(sshBin, cfg, socksAddr, controlPath, useControlMaster)
			if err != nil {
				if tunnel.shouldStop() {
					return
				}
				if firstAttempt {
					ready <- err
				}
				tunnel.dead <- err
				return
			}

			tunnel.cmd = cmd
			if firstAttempt {
				ready <- nil
				firstAttempt = false
			}

			err = cmd.Wait()
			if tunnel.shouldStop() {
				return
			}
			fmt.Printf("Tunnel exited: %v. Reconnecting...\n", err)
			if useControlMaster {
				cleanupControlSocket(controlPath, cfg)
			}
		}
	}()

	if err := <-ready; err != nil {
		return nil, err
	}

	return tunnel, nil
}

func (t *Tunnel) shouldStop() bool {
	if t == nil || t.stop == nil {
		return false
	}
	select {
	case <-t.stop:
		return true
	default:
		return false
	}
}

func startVerifiedTunnelCommand(sshBin string, cfg Config, socksAddr, controlPath string, useControlMaster bool) (*exec.Cmd, error) {
	if err := portFree(cfg.SOCKSBind + ":" + cfg.SOCKSPort); err != nil {
		return nil, fmt.Errorf("SOCKS port %s:%s already in use: %w", cfg.SOCKSBind, cfg.SOCKSPort, err)
	}

	var args []string
	if useControlMaster {
		args = buildSSHArgsWithControlMaster(cfg, socksAddr, controlPath)
	} else {
		args = buildSSHArgs(cfg, socksAddr)
	}

	cmd := exec.Command(sshBin, args...)
	setCmdEnv(cmd, cfg)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start ssh: %w", err)
	}

	if err := waitForPort(socksAddr, 10*time.Second); err != nil {
		terminateTunnelCommand(cmd)
		return nil, fmt.Errorf("SOCKS port did not become ready: %w", err)
	}

	if err := verifyProxyWorks(socksAddr, "http://example.com", 20*time.Second); err != nil {
		terminateTunnelCommand(cmd)
		return nil, fmt.Errorf("tunnel verification failed: %w", err)
	}

	return cmd, nil
}

func terminateTunnelCommand(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return
	}
	_ = cmd.Wait()
}

func buildSSHArgs(cfg Config, socksAddr string) []string {
	args := []string{
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "LogLevel=ERROR",
		"-o", fmt.Sprintf("ProxyCommand=%s", buildProxyCommand(cfg)),
		"-N", "-T",
		"-D", socksAddr,
		"-p", cfg.TunnelPort,
	}

	switch {
	case cfg.Auth.AgentSocket() != "":
		args = append(args, "-o", fmt.Sprintf("IdentityAgent=%s", cfg.Auth.AgentSocket()))
	case cfg.Auth.IdentityFile != "":
		args = append(args, "-i", cfg.Auth.IdentityFile)
	}

	args = append(args, fmt.Sprintf("%s@localhost", cfg.HomeUser))
	return args
}

func buildSSHArgsWithControlMaster(cfg Config, socksAddr, controlPath string) []string {
	args := []string{
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "LogLevel=ERROR",
		"-o", fmt.Sprintf("ProxyCommand=%s", buildProxyCommand(cfg)),
		"-o", "ControlMaster=auto",
		"-o", fmt.Sprintf("ControlPath=%s", controlPath),
		"-o", "ControlPersist=600",
		"-N", "-T",
		"-D", socksAddr,
		"-p", cfg.TunnelPort,
	}

	switch {
	case cfg.Auth.AgentSocket() != "":
		args = append(args, "-o", fmt.Sprintf("IdentityAgent=%s", cfg.Auth.AgentSocket()))
	case cfg.Auth.IdentityFile != "":
		args = append(args, "-i", cfg.Auth.IdentityFile)
	}

	args = append(args, fmt.Sprintf("%s@localhost", cfg.HomeUser))
	return args
}

func buildProxyCommand(cfg Config) string {
	target := fmt.Sprintf("%s@%s", cfg.VPSUser, cfg.VPSHost)

	proxy := []string{
		"ssh",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "LogLevel=ERROR",
	}

	switch {
	case cfg.Auth.AgentSocket() != "":
		proxy = append(proxy, "-o", fmt.Sprintf("IdentityAgent=%s", cfg.Auth.AgentSocket()))
	case cfg.Auth.IdentityFile != "":
		proxy = append(proxy, "-i", cfg.Auth.IdentityFile)
	}

	if cfg.VPSPort != "" && cfg.VPSPort != "22" {
		proxy = append(proxy, "-p", cfg.VPSPort)
	}

	proxy = append(proxy, target, "-W", "%h:%p")
	return strings.Join(proxy, " ")
}

func setCmdEnv(cmd *exec.Cmd, cfg Config) {
	env := os.Environ()
	if sock := cfg.Auth.AgentSocket(); sock != "" {
		env = append(env, "SSH_AUTH_SOCK="+sock)
	}
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
}

func ensureControlSocketDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	socketDir := filepath.Join(home, ".ssh", "sockets")
	if err := os.MkdirAll(socketDir, 0700); err != nil {
		return "", err
	}
	return filepath.Join(socketDir, "%r@%h-%p"), nil
}

func cleanupControlSocket(controlPath string, cfg Config) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	socketDir := filepath.Join(home, ".ssh", "sockets")
	pattern := fmt.Sprintf("%s@%%h-%%p", cfg.VPSUser)
	matches, _ := filepath.Glob(filepath.Join(socketDir, pattern))
	for _, m := range matches {
		os.Remove(m)
	}
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

func verifyProxyWorks(proxyAddr, testHost string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd := exec.Command("curl",
			"--socks5-hostname", proxyAddr,
			"-m", "3",
			"-s", "-o", "/dev/null", "-w", "%{http_code}",
			testHost,
		)
		out, err := cmd.CombinedOutput()
		if err == nil {
			code := strings.TrimSpace(string(out))
			if code == "200" || code == "301" || code == "302" || code == "400" || code == "401" || code == "403" || code == "404" || code == "407" || code == "502" || code == "503" {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("proxy verification failed for %s through %s", testHost, proxyAddr)
}
