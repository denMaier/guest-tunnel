package verify

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Self fetches the sha256sums file from sumsURL, finds the entry matching
// our own executable name, and compares it against SHA256(this binary).
func Self(sumsURL string) error {
	// Read our own bytes
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine own path: %w", err)
	}
	exeName := filepath.Base(exePath)

	f, err := os.Open(exePath)
	if err != nil {
		return fmt.Errorf("cannot open own binary: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("cannot hash own binary: %w", err)
	}
	actualHex := fmt.Sprintf("%x", h.Sum(nil))

	// Fetch the sums file (short timeout — this is a security check, not a
	// feature; if the network is unavailable we warn but do not block)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(sumsURL)
	if err != nil {
		return fmt.Errorf("could not fetch sha256sums from %s: %w", sumsURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sha256sums URL returned HTTP %d", resp.StatusCode)
	}

	// Parse lines of the form:
	//   <hex>  <filename>
	// (standard sha256sum output format)
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		expectedHex := parts[0]
		name := strings.TrimPrefix(parts[1], "*") // strip leading * (binary mode marker)
		name = filepath.Base(name)

		if name == exeName {
			if !strings.EqualFold(actualHex, expectedHex) {
				return fmt.Errorf(
					"hash mismatch!\n  expected: %s\n  actual:   %s\n\nDo NOT use this binary.",
					expectedHex, actualHex,
				)
			}
			return nil // verified
		}
	}

	return fmt.Errorf("no entry for %q found in sha256sums at %s", exeName, sumsURL)
}
