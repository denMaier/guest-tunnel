package client

import (
	"strings"
	"testing"

	"github.com/yourusername/guest-tunnel/internal/config"
)

func TestBuildSSHConfigBlockUsesProxyCommandForHomeServer(t *testing.T) {
	cfg := &config.Config{
		VPSHost:    "vps.example.com",
		VPSUser:    "jumpuser",
		VPSPort:    "22",
		HomeUser:   "tunneluser",
		TunnelPort: "2222",
	}

	block := buildSSHConfigBlock(cfg)

	if !strings.Contains(block, "ProxyCommand ssh -W %h:%p vps") {
		t.Fatalf("expected homeserver block to use ProxyCommand, got:\n%s", block)
	}
	if strings.Contains(block, "ProxyJump") {
		t.Fatalf("expected homeserver block not to use ProxyJump, got:\n%s", block)
	}
}
