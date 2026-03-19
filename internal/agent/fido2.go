package agent

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	fido2AgentName    = "fido2-agent"
	fido2SocketName   = "agent.sock"
	fido2StartupDelay = 5 * time.Second
)

// SpawnFido2Agent launches the local helper that loads resident keys into an
// SSH agent-compatible socket for tunnel authentication.
func SpawnFido2Agent() (*Auth, error) {
	helper, err := fido2AgentBin()
	if err != nil {
		return nil, err
	}

	tmpDir, err := os.MkdirTemp("", "guest-tunnel-fido2-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temporary fido2-agent directory: %w", err)
	}

	socketPath := filepath.Join(tmpDir, fido2SocketName)
	cmd := exec.Command(helper, "--socket", socketPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("failed to start %s: %w", helper, err)
	}

	dead := make(chan error, 1)
	go func() {
		dead <- cmd.Wait()
	}()

	cleanup := func() error {
		var cleanupErr error
		if cmd.Process != nil {
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				cleanupErr = err
			}
			<-dead
		}
		if err := os.RemoveAll(tmpDir); err != nil && cleanupErr == nil {
			cleanupErr = err
		}
		return cleanupErr
	}

	if err := waitForSocket(socketPath, fido2StartupDelay); err != nil {
		_ = cleanup()
		return nil, err
	}

	return &Auth{
		Fido2Sock: socketPath,
		cleanup:   cleanup,
	}, nil
}

func fido2AgentBin() (string, error) {
	if override := os.Getenv("GUEST_TUNNEL_FIDO2_AGENT"); override != "" {
		if _, err := os.Stat(override); err != nil {
			return "", fmt.Errorf("GUEST_TUNNEL_FIDO2_AGENT %q is not usable: %w", override, err)
		}
		return override, nil
	}

	if exe, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(exe), fido2AgentName)
		if _, err := os.Stat(sibling); err == nil {
			return sibling, nil
		}
	}

	bin, err := exec.LookPath(fido2AgentName)
	if err != nil {
		return "", fmt.Errorf(
			"%s not found; build it alongside guest-tunnel or set GUEST_TUNNEL_FIDO2_AGENT",
			fido2AgentName,
		)
	}
	return bin, nil
}

func waitForSocket(socketPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if fi, err := os.Stat(socketPath); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for fido2-agent socket %s", socketPath)
		}

		time.Sleep(100 * time.Millisecond)
	}
}
