package agent

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Agent wraps a private ssh-agent process.
type Agent struct {
	cmd           *exec.Cmd
	socketPath    string
	sshBin        string
	tmpDir        string
	externalAgent bool // true if using an existing agent (KeepassXC, StrongBox, etc.)
}

// SocketPath returns the SSH_AUTH_SOCK value for this agent.
func (a *Agent) SocketPath() string { return a.socketPath }

// IsExternal returns true if we're using an existing agent (KeepassXC, StrongBox, etc.)
func (a *Agent) IsExternal() bool { return a.externalAgent }

// FindOrFetchSSH looks for an SSH binary that supports FIDO2 (ecdsa-sk / ed25519-sk).
// Search order:
//  1. $GUEST_TUNNEL_SSH env var (user override)
//  2. System ssh — tested with a probe key type query
//  3. Download a pre-built static binary from binaryURL (if set), with automatic platform detection
//
// Returns the path to use and a cleanup func (deletes any downloaded binary).
func FindOrFetchSSH(binaryURL string) (string, func(), error) {
	noop := func() {}

	// 1. User override
	if override := os.Getenv("GUEST_TUNNEL_SSH"); override != "" {
		if err := probeSSH(override); err != nil {
			return "", noop, fmt.Errorf("GUEST_TUNNEL_SSH=%q does not support FIDO2: %w", override, err)
		}
		return override, noop, nil
	}

	// 2. System ssh
	if sys, err := exec.LookPath("ssh"); err == nil {
		if probeSSH(sys) == nil {
			return sys, noop, nil
		}
		// System ssh exists but lacks FIDO2 — fall through to download
		_ = err
	}

	// 3. Download
	if binaryURL == "" {
		return "", noop, fmt.Errorf(
			"system SSH does not support FIDO2 (compiled without libfido2).\n" +
				"Set GUEST_TUNNEL_SSH to a FIDO2-capable ssh binary.",
		)
	}

	// Auto-detect platform for the SSH binary URL
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	if goarch == "x86_64" {
		goarch = "amd64"
	} else if goarch == "aarch64" {
		goarch = "arm64"
	}
	// Map Go os names to release artifact names
	osMap := map[string]string{
		"darwin": "darwin",
		"linux":  "linux",
	}
	osStr := osMap[goos]
	if osStr == "" {
		return "", noop, fmt.Errorf("unsupported platform: %s/%s", goos, goarch)
	}

	// Construct URL: baseURL/ssh-fido2-{os}-{arch}
	binaryURL = fmt.Sprintf("%s/ssh-fido2-%s-%s", strings.TrimSuffix(binaryURL, "/"), osStr, goarch)

	tmpDir, err := os.MkdirTemp("", "guest-tunnel-*")
	if err != nil {
		return "", noop, fmt.Errorf("cannot create temp dir: %w", err)
	}
	cleanup := func() { os.RemoveAll(tmpDir) }

	dest := filepath.Join(tmpDir, "ssh-fido2")
	if err := downloadFile(binaryURL, dest); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("failed to download SSH binary from %s: %w", binaryURL, err)
	}
	if err := os.Chmod(dest, 0700); err != nil {
		cleanup()
		return "", noop, err
	}
	if err := probeSSH(dest); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("downloaded SSH binary at %s does not support FIDO2: %w", dest, err)
	}

	return dest, cleanup, nil
}

// probeSSH checks whether the given ssh binary supports ed25519-sk key types
// by asking it to generate one into /dev/null — we only check the exit code
// and stderr for the telltale "unknown key type" message.
func probeSSH(bin string) error {
	// `ssh -Q key` lists supported key types; presence of ed25519-sk is the signal.
	out, err := exec.Command(bin, "-Q", "key").Output()
	if err != nil {
		return fmt.Errorf("ssh -Q key failed: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "ed25519-sk" {
			return nil
		}
	}
	return fmt.Errorf("ed25519-sk not listed in supported key types")
}

// Start launches a private ssh-agent by default. If externalAgent is true,
// it checks for an existing SSH agent (KeepassXC, StrongBox, etc.) and uses
// that instead.
func Start(sshBin string, externalAgent bool) (*Agent, error) {
	// Check for existing agents only if explicitly requested
	if externalAgent {
		if extSocket, extBin := findExistingAgent(); extSocket != "" {
			return &Agent{
				socketPath:    extSocket,
				sshBin:        extBin,
				externalAgent: true,
			}, nil
		}
		// No external agent found, fall through to private agent
	}

	// No existing agent found — start our own private agent
	tmpDir, err := os.MkdirTemp("", "gt-agent-*")
	if err != nil {
		return nil, fmt.Errorf("cannot create agent temp dir: %w", err)
	}

	socketPath := filepath.Join(tmpDir, "agent.sock")

	// Find ssh-agent alongside the ssh binary we are using
	agentBin := findAgentBin(sshBin)

	cmd := exec.Command(agentBin, "-a", socketPath, "-D")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("cannot start ssh-agent (%s): %w", agentBin, err)
	}

	// Wait for the socket to appear (agent needs a moment to bind)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(socketPath); err != nil {
		cmd.Process.Kill()
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("ssh-agent socket never appeared at %s", socketPath)
	}

	return &Agent{
		cmd:        cmd,
		socketPath: socketPath,
		sshBin:     sshBin,
		tmpDir:     tmpDir,
	}, nil
}

// LoadResidentKey calls `ssh-add -K` to load all FIDO2 resident keys
// from the connected YubiKey into our private agent. The user will be
// prompted to touch the YubiKey.
//
// If using an external agent (KeepassXC, StrongBox, etc.), this is a no-op
// since keys are already loaded in that agent.
func (a *Agent) LoadResidentKey() error {
	if a.externalAgent {
		return nil // keys already loaded in external agent
	}

	env := append(os.Environ(), "SSH_AUTH_SOCK="+a.socketPath)

	// Find ssh-add
	addBin := findBinNextTo(a.sshBin, "ssh-add")

	cmd := exec.Command(addBin, "-K")
	cmd.Env = env
	cmd.Stdin = os.Stdin   // needed for PIN prompt if set
	cmd.Stdout = os.Stdout // show touch prompt
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh-add -K failed: %w\n\nMake sure your YubiKey is plugged in and has resident FIDO2 credentials.\nIf your key requires a PIN you will be prompted above.", err)
	}
	return nil
}

// Stop kills the agent process and removes all temp files.
// If using an external agent, this is a no-op.
func (a *Agent) Stop() {
	if a.externalAgent {
		return // don't kill someone else's agent
	}
	if a.cmd != nil && a.cmd.Process != nil {
		a.cmd.Process.Kill()
		a.cmd.Wait()
	}
	if a.tmpDir != "" {
		os.RemoveAll(a.tmpDir)
	}
}

// -------------------------------------------------------------------------- //
// Helpers                                                                     //
// -------------------------------------------------------------------------- //

// findExistingAgent checks for known SSH agent sockets from external programs.
// Only used when externalAgent is enabled via flag.
// Returns (socketPath, sshBin) if found, ("", "") otherwise.
func findExistingAgent() (string, string) {
	// Check SSH_AUTH_SOCK env var first
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if _, err := os.Stat(sock); err == nil {
			// Verify the agent is responsive
			if out, err := exec.Command("ssh-add", "-l").Output(); err == nil || len(out) > 0 {
				// Find ssh binary to return
				sshBin, _ := exec.LookPath("ssh")
				return sock, sshBin
			}
		}
	}

	// Check for KeepassXC agent (common locations)
	home := os.Getenv("HOME")
	keepassxcSockets := []string{
		filepath.Join(home, ".cache", "keepassxc", "agent.sock"),
		"/run/user/1000/keepassxc/agent.sock",
		"/tmp/keepassxc_agent.sock",
	}
	for _, sock := range keepassxcSockets {
		if sock == "" {
			continue
		}
		if _, err := os.Stat(sock); err == nil {
			sshBin, _ := exec.LookPath("ssh")
			return sock, sshBin
		}
	}

	// Check for gpg-agent --use-standard-socket (sometimes has keys)
	gpgSock := filepath.Join(home, ".gnupg", "S.gpg-agent.ssh")
	if _, err := os.Stat(gpgSock); err == nil {
		sshBin, _ := exec.LookPath("ssh")
		return gpgSock, sshBin
	}

	return "", ""
}

func findAgentBin(sshBin string) string {
	return findBinNextTo(sshBin, "ssh-agent")
}

// findBinNextTo looks for `name` in the same directory as `ref`, falling back
// to PATH. This handles the case where we downloaded a custom ssh binary into
// a temp dir that also contains ssh-agent and ssh-add.
func findBinNextTo(ref, name string) string {
	dir := filepath.Dir(ref)
	candidate := filepath.Join(dir, name)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	if found, err := exec.LookPath(name); err == nil {
		return found
	}
	return name // let the OS error naturally
}

func downloadFile(url, dest string) error {
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	// Build a download URL for the correct platform automatically
	_ = runtime.GOOS // used below if we add platform selection
	_ = runtime.GOARCH

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}
