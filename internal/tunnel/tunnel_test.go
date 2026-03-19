package tunnel

import (
	"errors"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yourusername/guest-tunnel/internal/agent"
)

func TestEstablishWithReconnectReturnsInitialFailure(t *testing.T) {
	oldLookup := lookupSSHBin
	oldStart := startVerifiedConn
	t.Cleanup(func() {
		lookupSSHBin = oldLookup
		startVerifiedConn = oldStart
	})

	lookupSSHBin = func() (string, error) { return "ssh", nil }
	startVerifiedConn = func(string, Config, string, string, bool) (*exec.Cmd, error) {
		return nil, errors.New("boom")
	}

	_, err := establishWithReconnect(testConfig())
	if err == nil {
		t.Fatal("expected initial reconnect failure to be returned")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected error to mention boom, got %v", err)
	}
}

func TestEstablishWithReconnectWaitsForInitialSuccessAndStopsOnClose(t *testing.T) {
	oldLookup := lookupSSHBin
	oldStart := startVerifiedConn
	t.Cleanup(func() {
		lookupSSHBin = oldLookup
		startVerifiedConn = oldStart
	})

	lookupSSHBin = func() (string, error) { return "ssh", nil }

	releaseFirstAttempt := make(chan struct{})
	firstAttemptStarted := make(chan struct{})
	var startCalls atomic.Int32

	startVerifiedConn = func(string, Config, string, string, bool) (*exec.Cmd, error) {
		call := startCalls.Add(1)
		if call == 1 {
			close(firstAttemptStarted)
			<-releaseFirstAttempt
		}

		cmd := exec.Command("sh", "-c", "sleep 30")
		if err := cmd.Start(); err != nil {
			t.Fatalf("failed to start helper process: %v", err)
		}
		return cmd, nil
	}

	type result struct {
		tunnel *Tunnel
		err    error
	}
	done := make(chan result, 1)
	go func() {
		tun, err := establishWithReconnect(testConfig())
		done <- result{tunnel: tun, err: err}
	}()

	<-firstAttemptStarted
	select {
	case res := <-done:
		t.Fatalf("establishWithReconnect returned before first attempt succeeded: %+v", res)
	case <-time.After(150 * time.Millisecond):
	}

	close(releaseFirstAttempt)

	var res result
	select {
	case res = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reconnect startup")
	}

	if res.err != nil {
		t.Fatalf("expected initial success, got error: %v", res.err)
	}
	if res.tunnel == nil {
		t.Fatal("expected tunnel on successful startup")
	}

	res.tunnel.Close()
	time.Sleep(300 * time.Millisecond)

	if got := startCalls.Load(); got != 1 {
		t.Fatalf("expected close to stop reconnect loop after first process, got %d starts", got)
	}
}

func testConfig() Config {
	return Config{
		Auth:       &agent.Auth{},
		VPSUser:    "jumpuser",
		VPSHost:    "example.com",
		VPSPort:    "22",
		HomeUser:   "tunneluser",
		TunnelPort: "2222",
		SOCKSPort:  "1080",
		SOCKSBind:  "127.0.0.1",
		Reconnect:  true,
	}
}
