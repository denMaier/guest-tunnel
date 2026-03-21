package tunnel

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yourusername/guest-tunnel/internal/agent"
)

var (
	lookupSSHBin      = agent.SSHBin
	startVerifiedConn = startVerifiedTunnelCommand
	errSSHEarlyExit   = errors.New("ssh exited before SOCKS port became ready")

	// defaultSSHOptions are the common -o flags shared by every SSH invocation.
	defaultSSHOptions = []string{
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-o", "LogLevel=ERROR",
	}
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
		if t.cmd != nil && t.cmd.Process != nil {
			_ = t.cmd.Process.Kill()
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
			select {
			case <-tunnel.stop:
				return
			case <-time.After(2 * time.Second):
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
	stderrTail := newTailBuffer(8192)
	setCmdEnv(cmd, cfg, stderrTail)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start ssh: %w", err)
	}

	if err := waitForPortOrProcessExit(socksAddr, cmd, 10*time.Second); err != nil {
		terminateTunnelCommand(cmd)
		if errors.Is(err, errSSHEarlyExit) {
			return nil, fmt.Errorf("%w%s", errSSHEarlyExit, formatCapturedStderr(stderrTail.String()))
		}
		return nil, fmt.Errorf("SOCKS port did not become ready: %w%s", err, formatCapturedStderr(stderrTail.String()))
	}

	if err := verifyProxyWorks(socksAddr, 20*time.Second); err != nil {
		terminateTunnelCommand(cmd)
		return nil, fmt.Errorf("tunnel verification failed: %w%s", err, formatCapturedStderr(stderrTail.String()))
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
	args := append([]string(nil), defaultSSHOptions...)
	args = append(args,
		"-o", fmt.Sprintf("ProxyCommand=%s", buildProxyCommand(cfg)),
		"-N", "-T",
		"-D", socksAddr,
		"-p", cfg.TunnelPort,
	)

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
	args := append([]string(nil), defaultSSHOptions...)
	args = append(args,
		"-o", fmt.Sprintf("ProxyCommand=%s", buildProxyCommand(cfg)),
		"-o", "ControlMaster=auto",
		"-o", fmt.Sprintf("ControlPath=%s", controlPath),
		"-o", "ControlPersist=600",
		"-N", "-T",
		"-D", socksAddr,
		"-p", cfg.TunnelPort,
	)

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

	proxy := append([]string{"ssh"}, defaultSSHOptions...)

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

func setCmdEnv(cmd *exec.Cmd, cfg Config, stderrCapture io.Writer) {
	env := os.Environ()
	if sock := cfg.Auth.AgentSocket(); sock != "" {
		env = append(env, "SSH_AUTH_SOCK="+sock)
	}
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	if stderrCapture != nil {
		cmd.Stderr = io.MultiWriter(os.Stderr, stderrCapture)
	} else {
		cmd.Stderr = os.Stderr
	}
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
	matches, _ := filepath.Glob(filepath.Join(socketDir, "*"))
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && !fi.IsDir() {
			os.Remove(m)
		}
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

func waitForPortOrProcessExit(addr string, cmd *exec.Cmd, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processRunning(cmd) {
			return errSSHEarlyExit
		}
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", addr)
}

func processRunning(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.Process == nil {
		return false
	}
	err := cmd.Process.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrProcessDone) {
		return false
	}
	return true
}

func formatCapturedStderr(stderr string) string {
	trimmed := strings.TrimSpace(stderr)
	if trimmed == "" {
		return ""
	}
	return "\nssh stderr:\n" + trimmed
}

type tailBuffer struct {
	mu   sync.Mutex
	max  int
	data []byte
}

func newTailBuffer(max int) *tailBuffer {
	return &tailBuffer{max: max}
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > b.max {
		b.data = append([]byte(nil), b.data[len(b.data)-b.max:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(bytes.Clone(b.data))
}

func verifyProxyWorks(proxyAddr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		err := verifySOCKS5Connect(proxyAddr, "localhost", 22, 3*time.Second)
		if err == nil {
			return nil
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timed out")
	}
	return fmt.Errorf("proxy verification failed for localhost:22 through %s: %w", proxyAddr, lastErr)
}

func verifySOCKS5Connect(proxyAddr, targetHost string, targetPort int, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", proxyAddr, timeout)
	if err != nil {
		return err
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(timeout))

	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return fmt.Errorf("SOCKS5 greeting write failed: %w", err)
	}

	greetResp := make([]byte, 2)
	if _, err := io.ReadFull(conn, greetResp); err != nil {
		return fmt.Errorf("SOCKS5 greeting read failed: %w", err)
	}
	if greetResp[0] != 0x05 || greetResp[1] != 0x00 {
		return fmt.Errorf("SOCKS5 auth negotiation failed: version=%d method=%d", greetResp[0], greetResp[1])
	}

	host := []byte(targetHost)
	if len(host) > 255 {
		return fmt.Errorf("target host too long: %s", targetHost)
	}

	req := make([]byte, 0, 7+len(host))
	req = append(req, 0x05, 0x01, 0x00, 0x03, byte(len(host)))
	req = append(req, host...)
	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, uint16(targetPort))
	req = append(req, portBuf...)

	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("SOCKS5 connect write failed: %w", err)
	}

	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("SOCKS5 connect read failed: %w", err)
	}
	if header[0] != 0x05 {
		return fmt.Errorf("unexpected SOCKS version: %d", header[0])
	}
	if header[1] != 0x00 {
		return fmt.Errorf("SOCKS5 connect failed with code=%d", header[1])
	}

	var skip int
	switch header[3] {
	case 0x01:
		skip = 4 + 2
	case 0x03:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return fmt.Errorf("SOCKS5 addr length read failed: %w", err)
		}
		skip = int(lenBuf[0]) + 2
	case 0x04:
		skip = 16 + 2
	default:
		return fmt.Errorf("SOCKS5 response has unknown atyp=%d", header[3])
	}

	if skip > 0 {
		discard := make([]byte, skip)
		if _, err := io.ReadFull(conn, discard); err != nil {
			return fmt.Errorf("SOCKS5 response read failed: %w", err)
		}
	}

	return nil
}
