package sysutil

import (
	"os"
	"os/exec"
	"strings"

	"github.com/yourusername/guest-tunnel/internal/ui"
)

// ReadPublicKeys reads an authorized_keys-style file and returns the list of
// SSH public key lines it contains.
func ReadPublicKeys(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var keys []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ssh-") || strings.HasPrefix(line, "ecdsa-") || strings.HasPrefix(line, "sk-") {
			keys = append(keys, line)
		}
	}
	return keys
}

// EnsureServiceUserState keeps a service account non-interactive while ensuring
// OpenSSH does not reject it pre-auth as a fully locked account on stricter
// PAM setups.
func EnsureServiceUserState(username, shell string) {
	RunUsermodIfPossible(username, "--shell", shell)
	RunUsermodIfPossible(username, "--unlock")
	RunPasswdIfPossible(username, "--delete")
}

// RunUsermodIfPossible runs usermod with the given args. A warning is printed
// if usermod is not available or fails.
func RunUsermodIfPossible(username string, args ...string) {
	cmd := exec.Command("usermod", append(args, username)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		ui.Warn("Could not update %s with usermod %s: %v\n%s", username, strings.Join(args, " "), err, out)
	}
}

// RunPasswdIfPossible runs passwd with the given args. A warning is printed
// if passwd is not available or fails.
func RunPasswdIfPossible(username string, args ...string) {
	cmd := exec.Command("passwd", append(args, username)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		ui.Warn("Could not update %s with passwd %s: %v\n%s", username, strings.Join(args, " "), err, out)
	}
}
