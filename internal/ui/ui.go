package ui

import (
	"fmt"
	"os"
)

var (
	RED    = "\033[0;31m"
	GREEN  = "\033[0;32m"
	YELLOW = "\033[1;33m"
	CYAN   = "\033[0;36m"
	BOLD   = "\033[1m"
	RESET  = "\033[0m"
)

func Banner(version string) {
	fmt.Printf("%s%s  guest-tunnel  %s%s\n", BOLD, CYAN, version, RESET)
	fmt.Println()
}

func Step(n int, msg string) {
	fmt.Printf("%s[%d]%s %s\n", CYAN, n, RESET, msg)
}

func OK(msg string, args ...interface{}) {
	if len(args) > 0 {
		fmt.Printf("%s  ✓ %s%s\n", GREEN, fmt.Sprintf(msg, args...), RESET)
	} else {
		fmt.Printf("%s  ✓ %s%s\n", GREEN, msg, RESET)
	}
}

func Warn(msg string, args ...interface{}) {
	if len(args) > 0 {
		fmt.Printf("%s  ⚠ %s%s\n", YELLOW, fmt.Sprintf(msg, args...), RESET)
	} else {
		fmt.Printf("%s  ⚠ %s%s\n", YELLOW, msg, RESET)
	}
}

func Hint(msg string, args ...interface{}) {
	if len(args) > 0 {
		fmt.Printf("%s     %s%s\n", CYAN, fmt.Sprintf(msg, args...), RESET)
	} else {
		fmt.Printf("%s     %s%s\n", CYAN, msg, RESET)
	}
}

func Fatal(msg string, args ...interface{}) {
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "%s  ✗ %s%s\n", RED, fmt.Sprintf(msg, args...), RESET)
	} else {
		fmt.Fprintf(os.Stderr, "%s  ✗ %s%s\n", RED, msg, RESET)
	}
	os.Exit(1)
}

func Header(msg string) {
	fmt.Printf("\n%s%s══════════════════════════════════════════%s\n", BOLD, CYAN, RESET)
	fmt.Printf("%s%s  %s%s\n", BOLD, CYAN, msg, RESET)
	fmt.Printf("%s%s══════════════════════════════════════════%s\n\n", BOLD, CYAN, RESET)
}

func Print(msg string, args ...interface{}) {
	if len(args) > 0 {
		fmt.Printf(msg+"\n", args...)
	} else {
		fmt.Printf(msg + "\n")
	}
}

func PrintConnectionInfo(bind, port string) {
	fmt.Printf("\n%s  SOCKS5 proxy: %s:%s%s\n", GREEN, bind, port, RESET)
	fmt.Printf("%s  Example: curl --socks5-hostname %s:%s http://example.home%s\n\n", CYAN, bind, port, RESET)
}
