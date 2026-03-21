// Package config loads and validates guest-tunnel's configuration from a
// config.yml file. No third-party dependencies — parsed with stdlib only.
//
// Search order for config.yml:
//  1. Path given by --config flag (if provided)
//  2. $GUEST_TUNNEL_CONFIG env var
//  3. ./config.yml                          (same dir as binary / cwd)
//  4. ~/.config/guest-tunnel/config.yml
//  5. ~/.guest-tunnel.yml
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config holds the full runtime configuration for guest-tunnel.
type Config struct {
	sourcePath string // resolved path the config was loaded from (for Save)

	// Gate 1 — VPS jump host
	VPSHost string `yaml:"vps_host"` // e.g. vps.example.com
	VPSUser string `yaml:"vps_user"` // e.g. jumpuser
	VPSPort string `yaml:"vps_port"` // default: 22

	// Gate 2 — Homeserver (connected via reverse tunnel on VPS)
	HomeUser   string `yaml:"home_user"`   // e.g. tunneluser
	TunnelPort string `yaml:"tunnel_port"` // reverse tunnel port on VPS (e.g., 2222)

	// SOCKS5 proxy (client mode only)
	SOCKSPort string `yaml:"socks_port"` // default: 1080
	SOCKSBind string `yaml:"socks_bind"` // default: 127.0.0.1

	// Non-interactive setup (optional — if set, skips interactive prompts)
	LaptopPubKey string `yaml:"laptop_pubkey"` // SSH public key for the laptop (server + home)
	SSHDaemon    string `yaml:"ssh_daemon"`    // "openssh" or "dropbear" (home only)
	DropbearPort string `yaml:"dropbear_port"` // port for dropbear to listen on (home only)
	SkipTest     bool   `yaml:"skip_test"`     // skip interactive dropbear login test (home only)
}

// Load finds and parses the config file. See package doc for search order.
// configPath may be empty, in which case the search order above is used.
// Validation is NOT performed here — call Validate(mode) after Load.
func Load(configPath string) (*Config, error) {
	path, err := findConfig(configPath)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot open config file %s: %w", path, err)
	}
	defer f.Close()

	cfg, err := parse(f)
	if err != nil {
		return nil, fmt.Errorf("error in config file %s: %w", path, err)
	}

	cfg.sourcePath = path
	cfg.applyDefaults()

	return cfg, nil
}

// ConfigPath returns the resolved path that would be loaded, without
// actually loading it. Useful for printing "using config: ..." to the user.
func ConfigPath(override string) string {
	path, err := findConfig(override)
	if err != nil {
		return "(not found)"
	}
	return path
}

// Example returns a well-commented example config.yml as a string.
func Example() string {
	return `# guest-tunnel configuration
# Place this file at ~/.config/guest-tunnel/config.yml
# or pass --config /path/to/config.yml
#
# Each mode (server, home, client) only uses the fields it needs.
# Run --init with a mode flag for a minimal template.

# ── VPS jump host ────────────────────────────────────────────────────────────
vps_host: vps.example.com   # hostname or IP (needed by home + client)
vps_user: jumpuser           # SSH user on the VPS (all modes)
vps_port: 22                 # SSH port on the VPS (default: 22)

# ── Homeserver ───────────────────────────────────────────────────────────────
home_user: tunneluser        # SSH user on the homeserver (home + client)
tunnel_port: 2222            # reverse tunnel port on VPS (all modes)

# ── Client proxy ─────────────────────────────────────────────────────────────
socks_port: 1080             # local SOCKS5 proxy port (client only, default: 1080)
socks_bind: 127.0.0.1        # bind address (client only, default: 127.0.0.1)

# ── Non-interactive setup (optional) ─────────────────────────────────────────
# Skip interactive prompts during --mode=server or --mode=home.
# Values entered interactively are saved back to this file automatically.

# laptop_pubkey: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA... user@laptop
# ssh_daemon: dropbear       # "openssh" or "dropbear" (home only, default: dropbear)
# dropbear_port: 22          # listen port for Dropbear (home only)
# skip_test: false           # skip interactive Dropbear login test (home only)
`
}

// WriteExample writes a mode-specific example config to dest. It refuses to
// overwrite an existing file. Returns nil if the file already existed.
func WriteExample(dest, mode string) error {
	if _, err := os.Stat(dest); err == nil {
		fmt.Printf("Config already exists at %s\n", dest)
		fmt.Println("Delete it first if you want to regenerate.")
		return nil
	}

	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("cannot create %s: %w", dir, err)
	}

	if err := os.WriteFile(dest, []byte(ExampleForMode(mode)), 0600); err != nil {
		return fmt.Errorf("cannot write %s: %w", dest, err)
	}

	fmt.Printf("Example config written to: %s\n", dest)
	fmt.Println("Edit it with your details, then run guest-tunnel again.")
	return nil
}

// ExampleForMode returns a minimal config template for the given mode.
func ExampleForMode(mode string) string {
	switch mode {
	case "server":
		return `# guest-tunnel config — server mode (VPS)
# Save to /etc/guest-tunnel/config.yml

vps_user: jumpuser           # SSH user for the tunnel jump account
vps_port: 22                 # SSH port (default: 22)
tunnel_port: 2222            # reverse tunnel port — must match home config

# Optional: skip the interactive key prompt
# laptop_pubkey: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA... user@laptop
`
	case "home":
		return `# guest-tunnel config — home mode (homeserver)
# Save to /etc/guest-tunnel/config.yml

vps_host: vps.example.com   # VPS hostname or IP
vps_user: jumpuser           # SSH user on the VPS
vps_port: 22                 # SSH port on the VPS (default: 22)
home_user: tunneluser        # SSH user on this homeserver
tunnel_port: 2222            # reverse tunnel port — must match server config

# Optional: skip interactive prompts
# laptop_pubkey: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA... user@laptop
# ssh_daemon: dropbear       # "openssh" or "dropbear" (default: dropbear)
# dropbear_port: 22          # Dropbear listen port
# skip_test: false           # skip interactive Dropbear login test
`
	case "client":
		return `# guest-tunnel config — client mode (laptop)
# Save to ~/.config/guest-tunnel/config.yml

vps_host: vps.example.com   # VPS hostname or IP
vps_user: jumpuser           # SSH user on the VPS
vps_port: 22                 # SSH port on the VPS (default: 22)
home_user: tunneluser        # SSH user on the homeserver
tunnel_port: 2222            # reverse tunnel port

socks_port: 1080             # local SOCKS5 proxy port (default: 1080)
socks_bind: 127.0.0.1        # bind address (default: 127.0.0.1)
`
	default:
		return Example()
	}
}

// -------------------------------------------------------------------------- //
// Internal                                                                    //
// -------------------------------------------------------------------------- //

func findConfig(override string) (string, error) {
	candidates := []string{}

	if override != "" {
		candidates = append(candidates, override)
	} else {
		if env := os.Getenv("GUEST_TUNNEL_CONFIG"); env != "" {
			candidates = append(candidates, env)
		}
		// Executable directory / cwd
		if exe, err := os.Executable(); err == nil {
			candidates = append(candidates, filepath.Join(filepath.Dir(exe), "config.yml"))
		}
		candidates = append(candidates, "config.yml")

		// XDG / home
		if home, err := os.UserHomeDir(); err == nil {
			candidates = append(candidates,
				filepath.Join(home, ".config", "guest-tunnel", "config.yml"),
				filepath.Join(home, ".guest-tunnel.yml"),
			)
		}
	}

	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	// Nothing found — give the user a helpful error
	searched := make([]string, len(candidates))
	for i, c := range candidates {
		searched[i] = "  • " + c
	}
	return "", fmt.Errorf(
		"no config.yml found. Searched:\n%s\n\nCreate one with:\n  guest-tunnel --init\n\nOr pass --config /path/to/config.yml",
		strings.Join(searched, "\n"),
	)
}

// parse is a minimal YAML parser for the flat key: value structure we use.
// It handles:
//   - "key: value" lines
//   - "#" comments (whole-line and inline)
//   - blank lines
//   - quoted values ("..." or '...')
//
// It intentionally does NOT handle nested keys, lists, or multiline values —
// our config has none of those, and keeping this simple means zero deps and
// easy auditability.
func parse(f *os.File) (*Config, error) {
	cfg := &Config{}
	scanner := bufio.NewScanner(f)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		raw := scanner.Text()

		// Strip inline comments
		line := stripComment(raw)
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			return nil, fmt.Errorf("line %d: expected 'key: value', got: %q", lineNum, raw)
		}

		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		val = unquote(val)

		switch key {
		case "vps_host":
			cfg.VPSHost = val
		case "vps_user":
			cfg.VPSUser = val
		case "vps_port":
			cfg.VPSPort = val
		case "home_user":
			cfg.HomeUser = val
		case "tunnel_port":
			cfg.TunnelPort = val
		case "socks_port":
			cfg.SOCKSPort = val
		case "socks_bind":
			cfg.SOCKSBind = val
		case "laptop_pubkey":
			cfg.LaptopPubKey = val
		case "ssh_daemon":
			cfg.SSHDaemon = val
		case "dropbear_port":
			cfg.DropbearPort = val
		case "skip_test":
			lower := strings.ToLower(val)
			cfg.SkipTest = lower == "true" || lower == "yes" || lower == "1"
		default:
			// Unknown keys are ignored — forward-compatible and tolerates
			// comments like "# key: value" that slipped through
		}
	}

	return cfg, scanner.Err()
}

// stripComment removes everything from the first unquoted '#' onwards.
func stripComment(s string) string {
	inSingle, inDouble := false, false
	for i, c := range s {
		switch c {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble {
				return s[:i]
			}
		}
	}
	return s
}

// unquote strips a single layer of surrounding " or ' quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') ||
			(s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func (c *Config) applyDefaults() {
	if c.VPSPort == "" {
		c.VPSPort = "22"
	}
	if c.TunnelPort == "" {
		c.TunnelPort = "2222"
	}
	if c.SOCKSPort == "" {
		c.SOCKSPort = "1080"
	}
	if c.SOCKSBind == "" {
		c.SOCKSBind = "127.0.0.1"
	}
}

// Validate checks that the config has all fields required for the given mode.
// Modes: "server", "home", "client".
func (c *Config) Validate(mode string) error {
	var errs []string

	switch mode {
	case "server":
		if c.VPSUser == "" {
			errs = append(errs, "vps_user is required")
		}
		if c.TunnelPort == "" {
			errs = append(errs, "tunnel_port is required")
		}
	case "home":
		if c.VPSHost == "" {
			errs = append(errs, "vps_host is required")
		}
		if c.VPSUser == "" {
			errs = append(errs, "vps_user is required")
		}
		if c.HomeUser == "" {
			errs = append(errs, "home_user is required")
		}
		if c.TunnelPort == "" {
			errs = append(errs, "tunnel_port is required")
		}
	case "client":
		if c.VPSHost == "" {
			errs = append(errs, "vps_host is required")
		}
		if c.VPSUser == "" {
			errs = append(errs, "vps_user is required")
		}
		if c.HomeUser == "" {
			errs = append(errs, "home_user is required")
		}
	}

	if _, err := strconv.Atoi(c.TunnelPort); err != nil || c.TunnelPort == "0" {
		errs = append(errs, fmt.Sprintf("tunnel_port must be a valid port number, got %q", c.TunnelPort))
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "\n  "))
	}
	return nil
}

// Save writes updated config values back to the file. It replaces existing
// keys in-place and appends any new keys at the end. If no config file exists
// yet (e.g. all values came from interactive prompts), defaultPath is used.
func (c *Config) Save(defaultPath string) error {
	path := c.sourcePath
	if path == "" {
		path = defaultPath
	}

	// Ensure parent directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("cannot create config directory %s: %w", dir, err)
	}

	// Build the map of non-empty values to persist
	values := make(map[string]string)
	if c.VPSHost != "" {
		values["vps_host"] = c.VPSHost
	}
	if c.VPSUser != "" {
		values["vps_user"] = c.VPSUser
	}
	if c.VPSPort != "" {
		values["vps_port"] = c.VPSPort
	}
	if c.HomeUser != "" {
		values["home_user"] = c.HomeUser
	}
	if c.TunnelPort != "" {
		values["tunnel_port"] = c.TunnelPort
	}
	if c.SOCKSPort != "" {
		values["socks_port"] = c.SOCKSPort
	}
	if c.SOCKSBind != "" {
		values["socks_bind"] = c.SOCKSBind
	}
	if c.LaptopPubKey != "" {
		values["laptop_pubkey"] = c.LaptopPubKey
	}
	if c.SSHDaemon != "" {
		values["ssh_daemon"] = c.SSHDaemon
	}
	if c.DropbearPort != "" {
		values["dropbear_port"] = c.DropbearPort
	}
	if c.SkipTest {
		values["skip_test"] = "true"
	}

	// Read existing file content (if any)
	var lines []string
	written := make(map[string]bool)
	if data, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			stripped := stripComment(line)
			trimmed := strings.TrimSpace(stripped)
			idx := strings.IndexByte(trimmed, ':')
			if idx > 0 {
				key := strings.TrimSpace(trimmed[:idx])
				if val, ok := values[key]; ok {
					lines = append(lines, key+": "+val)
					written[key] = true
					continue
				}
			}
			lines = append(lines, line)
		}
	}

	// Append any keys not yet in the file
	for key, val := range values {
		if !written[key] {
			lines = append(lines, key+": "+val)
		}
	}

	content := strings.Join(lines, "\n")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return fmt.Errorf("cannot write config %s: %w", path, err)
	}

	c.sourcePath = path
	return nil
}
