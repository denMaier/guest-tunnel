// Package agent resolves how SSH authentication is provided to the tunnel.
//
// Two modes are supported:
//
//   - Agent socket: an existing SSH agent (ssh-agent, KeepassXC, StrongBox,
//     1Password, gpg-agent …) is used via SSH_AUTH_SOCK or an explicit path.
//
//   - Identity file: a plain private key on disk (id_ed25519, id_ecdsa, etc.)
//     is passed directly to ssh with -i. FIDO2 resident-key files work here
//     too if the system ssh supports them.
//
// There is no FIDO2 binary management, no downloading, no building.
package agent

import (
	"fmt"
	"os"
	"os/exec"
)

// Auth describes how the tunnel should authenticate.
type Auth struct {
	// Exactly one of these is set.
	AgentSock    string // path to SSH_AUTH_SOCK; ssh uses -o IdentityAgent=…
	IdentityFile string // path to private key file; ssh uses -i …
}

// Resolve determines the Auth to use.
//
// Priority:
//  1. explicitSock if non-empty (--agent-sock flag)
//  2. explicitKey  if non-empty (--identity flag)
//  3. SSH_AUTH_SOCK environment variable if set and the socket exists
//  4. error — the user must specify one of the above
func Resolve(explicitSock, explicitKey string) (*Auth, error) {
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
