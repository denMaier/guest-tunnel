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
	"strings"
)

// Config holds the full runtime configuration for guest-tunnel.
type Config struct {
	// Gate 1 — VPS jump host
	VPSHost string `yaml:"vps_host"` // e.g. vps.example.com
	VPSUser string `yaml:"vps_user"` // e.g. jumpuser
	VPSPort string `yaml:"vps_port"` // default: 22

	// Gate 2 — Homeserver (connected via reverse tunnel on VPS)
	HomeUser   string `yaml:"home_user"`   // e.g. tunneluser
	TunnelPort string `yaml:"tunnel_port"` // reverse tunnel port on VPS (e.g., 2222)

	// SOCKS5 proxy
	SOCKSPort string `yaml:"socks_port"` // default: 1080
	SOCKSBind string `yaml:"socks_bind"` // default: 127.0.0.1
}

// Load finds and parses the config file. See package doc for search order.
// configPath may be empty, in which case the search order above is used.
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

	cfg.applyDefaults()

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config in %s: %w", path, err)
	}

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

# ── Gate 1: VPS jump host ────────────────────────────────────────────────────
vps_host: vps.example.com   # hostname or IP of your VPS
vps_user: jumpuser           # SSH user on the VPS (jump-only, no shell needed)
vps_port: 22                 # SSH port on the VPS (default: 22)

# ── Gate 2: Homeserver ───────────────────────────────────────────────────────
# Connected via reverse tunnel on the VPS (homeserver establishes -R <port>:localhost:22)
home_user: tunneluser        # SSH user on the homeserver
tunnel_port: 2222            # reverse tunnel port on VPS (default: 2222)

# ── SOCKS5 proxy ─────────────────────────────────────────────────────────────
socks_port: 1080             # local port for the SOCKS5 proxy (default: 1080)
socks_bind: 127.0.0.1        # bind address — keep 127.0.0.1 on borrowed machines
`
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

func (c *Config) validate() error {
	var errs []string
	if c.VPSHost == "" {
		errs = append(errs, "vps_host is required")
	}
	if c.VPSUser == "" {
		errs = append(errs, "vps_user is required")
	}
	if c.TunnelPort == "" {
		errs = append(errs, "tunnel_port is required")
	}
	if c.HomeUser == "" {
		errs = append(errs, "home_user is required")
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "\n  "))
	}
	return nil
}
