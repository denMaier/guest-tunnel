package proxy

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Configure attempts to set the SOCKS5 proxy system-wide (macOS) or
// per-user (Linux). Returns a cleanup function that restores original state.
func Configure(socksPort string) (func(), error) {
	switch runtime.GOOS {
	case "darwin":
		return configureMacOS(socksPort)
	case "linux":
		return configureLinux(socksPort)
	default:
		return nil, fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

// -------------------------------------------------------------------------- //
// macOS — networksetup                                                        //
// -------------------------------------------------------------------------- //

func configureMacOS(socksPort string) (func(), error) {
	// Get all network services
	out, err := exec.Command("networksetup", "-listallnetworkservices").Output()
	if err != nil {
		return nil, fmt.Errorf("networksetup not available: %w", err)
	}

	var services []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		// Skip the header line and empty lines
		if line == "" || strings.HasPrefix(line, "An asterisk") {
			continue
		}
		// Only configure active-looking services
		if strings.Contains(line, "Wi-Fi") || strings.Contains(line, "Ethernet") ||
			strings.Contains(line, "USB") || strings.Contains(line, "Thunderbolt") {
			services = append(services, line)
		}
	}

	if len(services) == 0 {
		return nil, fmt.Errorf("no network services found")
	}

	// Record original state and enable proxy
	type original struct {
		service string
		enabled string
		host    string
		port    string
	}
	var originals []original

	for _, svc := range services {
		// Read current state
		state, err := exec.Command("networksetup", "-getsocksfirewallproxy", svc).Output()
		if err != nil {
			continue
		}
		o := original{service: svc}
		for _, l := range strings.Split(string(state), "\n") {
			if strings.HasPrefix(l, "Enabled:") {
				o.enabled = strings.TrimSpace(strings.TrimPrefix(l, "Enabled:"))
			}
			if strings.HasPrefix(l, "Server:") {
				o.host = strings.TrimSpace(strings.TrimPrefix(l, "Server:"))
			}
			if strings.HasPrefix(l, "Port:") {
				o.port = strings.TrimSpace(strings.TrimPrefix(l, "Port:"))
			}
		}
		originals = append(originals, o)

		// Set proxy
		exec.Command("networksetup", "-setsocksfirewallproxy", svc, "127.0.0.1", socksPort).Run()
		exec.Command("networksetup", "-setsocksfirewallproxystate", svc, "on").Run()
	}

	fmt.Printf("    \033[32m✓\033[0m macOS SOCKS proxy set on: %s\n", strings.Join(services, ", "))

	cleanup := func() {
		for _, o := range originals {
			if o.enabled == "Yes" {
				exec.Command("networksetup", "-setsocksfirewallproxy", o.service, o.host, o.port).Run()
			} else {
				exec.Command("networksetup", "-setsocksfirewallproxystate", o.service, "off").Run()
			}
		}
		fmt.Println("    \033[32m✓\033[0m macOS proxy settings restored")
	}
	return cleanup, nil
}

// -------------------------------------------------------------------------- //
// Linux — environment variable hint + gsettings if available                 //
// -------------------------------------------------------------------------- //

func configureLinux(socksPort string) (func(), error) {
	noop := func() {}

	// Try gsettings (GNOME)
	if _, err := exec.LookPath("gsettings"); err == nil {
		if err := configureGNOME(socksPort); err == nil {
			cleanup := func() {
				exec.Command("gsettings", "set", "org.gnome.system.proxy", "mode", "none").Run()
				fmt.Println("    \033[32m✓\033[0m GNOME proxy settings restored")
			}
			fmt.Println("    \033[32m✓\033[0m GNOME proxy configured")
			return cleanup, nil
		}
	}

	// Print export hint — always useful regardless of DE
	printLinuxEnvHint(socksPort)

	// Write a small shell snippet to /tmp the user can source
	snippet := fmt.Sprintf("export ALL_PROXY=socks5h://localhost:%s\nexport HTTPS_PROXY=socks5h://localhost:%s\nexport HTTP_PROXY=socks5h://localhost:%s\n",
		socksPort, socksPort, socksPort)
	snippetPath := "/tmp/guest-tunnel-proxy.sh"
	_ = os.WriteFile(snippetPath, []byte(snippet), 0600)
	fmt.Printf("    \033[90m→\033[0m Proxy env snippet written to %s\n", snippetPath)
	fmt.Printf("    \033[90m→\033[0m Run: source %s\n", snippetPath)

	return noop, nil
}

func configureGNOME(socksPort string) error {
	cmds := [][]string{
		{"gsettings", "set", "org.gnome.system.proxy", "mode", "manual"},
		{"gsettings", "set", "org.gnome.system.proxy.socks", "host", "127.0.0.1"},
		{"gsettings", "set", "org.gnome.system.proxy.socks", "port", socksPort},
	}
	for _, c := range cmds {
		if err := exec.Command(c[0], c[1:]...).Run(); err != nil {
			return err
		}
	}
	return nil
}

func printLinuxEnvHint(socksPort string) {
	fmt.Printf(`
    \033[90m→\033[0m To route shell traffic through the tunnel:
      export ALL_PROXY=socks5h://localhost:%s

`, socksPort)
}
