BINARY     := guest-tunnel
MODULE     := github.com/yourusername/guest-tunnel
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_DIR  := dist

# ── Release URLs (set when cutting a release) ────────────────────────────────
RELEASE_BASE   ?= https://github.com/yourusername/guest-tunnel/releases/download/$(VERSION)
# Default URL for static SSH binary with FIDO2 support (auto-downloaded if system SSH lacks FIDO2)
SSH_BINARY_URL ?= $(RELEASE_BASE)

# ── Build flags ──────────────────────────────────────────────────────────────
# Server config (VPS host/user, homeserver host/user) is NOT baked in.
# It is read from config.yml at runtime. See: guest-tunnel --init
LDFLAGS := -s -w \
  -X main.Version=$(VERSION) \
  -X main.SSHBinaryURL=$(SSH_BINARY_URL)

CGO := CGO_ENABLED=0

.PHONY: all build cross clean sha256sums install help

all: build

## build: Build for the current platform
build:
	@mkdir -p $(BUILD_DIR)
	$(CGO) go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) ./cmd/guest-tunnel
	@echo "Built: $(BUILD_DIR)/$(BINARY)"

## cross: Cross-compile for macOS (amd64 + arm64) and Linux (amd64 + arm64)
cross:
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin  GOARCH=amd64 $(CGO) go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY)-darwin-amd64  ./cmd/guest-tunnel
	GOOS=darwin  GOARCH=arm64 $(CGO) go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY)-darwin-arm64  ./cmd/guest-tunnel
	GOOS=linux   GOARCH=amd64 $(CGO) go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY)-linux-amd64   ./cmd/guest-tunnel
	GOOS=linux   GOARCH=arm64 $(CGO) go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY)-linux-arm64   ./cmd/guest-tunnel
	@echo "Cross-compiled binaries in $(BUILD_DIR)/"

## sha256sums: Generate sha256sums.txt from dist/ binaries (run after cross)
sha256sums:
	@cd $(BUILD_DIR) && sha256sum $(BINARY)-* > sha256sums.txt
	@echo "Written: $(BUILD_DIR)/sha256sums.txt"
	@cat $(BUILD_DIR)/sha256sums.txt

## install: Install to /usr/local/bin (requires write permission)
install: build
	install -m 755 $(BUILD_DIR)/$(BINARY) /usr/local/bin/$(BINARY)

## clean: Remove build artifacts
clean:
	rm -rf $(BUILD_DIR)

## help: Show this help
help:
	@grep -E '^## ' Makefile | sed 's/## /  /'
