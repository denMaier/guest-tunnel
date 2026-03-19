// Package agent resolves how SSH authentication is provided to the tunnel.
//
// Three modes are supported:
//
//   - Agent socket: an existing SSH agent (ssh-agent, KeepassXC, StrongBox,
//     1Password, gpg-agent ...) is used via SSH_AUTH_SOCK or an explicit path.
//
//   - Identity file: a plain private key on disk (id_ed25519, id_ecdsa, etc.)
//     is passed directly to ssh with -i. FIDO2 resident-key files work here
//     too if the system ssh supports them.
//
//   - FIDO2/YubiKey: a resident key on a YubiKey is used via a spawned
//     fido2-agent process that exposes a local SSH agent socket.
package agent

import (
	"fmt"
	"os"
	"os/exec"
)

// Auth describes how the tunnel should authenticate.
type Auth struct {
	// Exactly one of these is set.
	AgentSock    string
	IdentityFile string
	Fido2Sock    string

	cleanup func() error
}

// Close releases any resources owned by the auth mode.
func (a *Auth) Close() error {
	if a == nil || a.cleanup == nil {
		return nil
	}
	err := a.cleanup()
	a.cleanup = nil
	return err
}

// AgentSocket returns the agent socket to expose to ssh, if any.
func (a *Auth) AgentSocket() string {
	if a == nil {
		return ""
	}
	if a.Fido2Sock != "" {
		return a.Fido2Sock
	}
	return a.AgentSock
}

// Resolve determines the Auth to use.
//
// Priority:
//  1. explicitSock if non-empty (--agent-sock flag)
//  2. explicitKey  if non-empty (--identity flag)
//  3. yubikey      if --yubikey was requested
//  4. SSH_AUTH_SOCK environment variable if set and the socket exists
//  5. error
func Resolve(explicitSock, explicitKey string, yubikey bool) (*Auth, error) {
	if explicitSock != "" {
		if _, err := os.Stat(explicitSock); err != nil {
			return nil, fmt.Errorf("agent socket %q not found: %w", explicitSock, err)
		}
		return &Auth{AgentSock: explicitSock}, nil
	}

	if explicitKey != "" {
		if _, err := os.Stat(explicitKey); err != nil {
			return nil, fmt.Errorf("identity file %q not found: %w", explicitKey, err)
		}
		return &Auth{IdentityFile: explicitKey}, nil
	}

	if yubikey {
		auth, err := SpawnFido2Agent()
		if err != nil {
			return nil, err
		}
		return auth, nil
	}

	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if _, err := os.Stat(sock); err == nil {
			return &Auth{AgentSock: sock}, nil
		}
	}

	return nil, fmt.Errorf(
		"no SSH authentication source found.\n\n" +
			"Provide one of:\n" +
			"  --identity ~/.ssh/id_ed25519   (private key file)\n" +
			"  --agent-sock /path/to/agent    (SSH agent socket)\n" +
			"  --yubikey                      (YubiKey resident key)\n" +
			"  SSH_AUTH_SOCK env var          (existing agent)\n",
	)
}

// SSHBin returns the path to the system ssh binary, or an error if not found.
func SSHBin() (string, error) {
	bin, err := exec.LookPath("ssh")
	if err != nil {
		return "", fmt.Errorf("ssh not found in PATH: %w", err)
	}
	return bin, nil
}
