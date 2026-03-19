package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	var (
		socketPath  = flag.String("socket", "", "path to the SSH agent socket to create")
		sshAgentBin = flag.String("ssh-agent-bin", "ssh-agent", "ssh-agent binary to launch")
		sshAddBin   = flag.String("ssh-add-bin", "ssh-add", "ssh-add binary to use for loading resident keys")
	)

	flag.Parse()

	if *socketPath == "" {
		fmt.Fprintln(os.Stderr, "--socket is required")
		os.Exit(2)
	}

	if err := os.MkdirAll(filepath.Dir(*socketPath), 0700); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create socket directory: %v\n", err)
		os.Exit(1)
	}
	_ = os.Remove(*socketPath)

	agentCmd := exec.Command(*sshAgentBin, "-D", "-a", *socketPath)
	agentCmd.Stdout = io.Discard
	agentCmd.Stderr = os.Stderr

	if err := agentCmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start ssh-agent: %v\n", err)
		os.Exit(1)
	}

	agentDead := make(chan error, 1)
	go func() {
		agentDead <- agentCmd.Wait()
	}()

	cleanup := func() {
		if agentCmd.Process != nil {
			_ = agentCmd.Process.Kill()
		}
		<-agentDead
		_ = os.Remove(*socketPath)
	}

	if err := waitForSocket(*socketPath, 5*time.Second); err != nil {
		cleanup()
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	loadCmd := exec.Command(*sshAddBin, "-K")
	loadCmd.Env = append(os.Environ(), "SSH_AUTH_SOCK="+*socketPath)
	loadCmd.Stdin = os.Stdin
	loadCmd.Stdout = os.Stdout
	loadCmd.Stderr = os.Stderr

	if err := loadCmd.Run(); err != nil {
		cleanup()
		fmt.Fprintf(os.Stderr, "failed to load resident keys with ssh-add -K: %v\n", err)
		os.Exit(1)
	}

	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigC:
		cleanup()
		if signalNum, ok := sig.(syscall.Signal); ok {
			os.Exit(128 + int(signalNum))
		}
		os.Exit(1)
	case err := <-agentDead:
		_ = os.Remove(*socketPath)
		if err != nil && !errors.Is(err, os.ErrProcessDone) {
			fmt.Fprintf(os.Stderr, "ssh-agent exited: %v\n", err)
			os.Exit(1)
		}
	}
}

func waitForSocket(socketPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if info, err := os.Stat(socketPath); err == nil && info.Mode()&os.ModeSocket != 0 {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for ssh-agent socket %s", socketPath)
		}

		time.Sleep(100 * time.Millisecond)
	}
}
