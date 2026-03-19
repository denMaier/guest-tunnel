BINARY        := guest-tunnel
HELPER_BINARY := fido2-agent
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_DIR     := dist

LDFLAGS := -s -w -X main.Version=$(VERSION)
CGO     := CGO_ENABLED=0

.PHONY: all build cross clean help container-smoke container-test

all: build

## build: Build for the current platform
build:
	@mkdir -p $(BUILD_DIR)
	$(CGO) go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) ./cmd/guest-tunnel
	$(CGO) go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(HELPER_BINARY) ./cmd/fido2-agent
	@echo "Built: $(BUILD_DIR)/$(BINARY)"
	@echo "Built: $(BUILD_DIR)/$(HELPER_BINARY)"

## cross: Cross-compile for macOS (amd64 + arm64) and Linux (amd64 + arm64)
cross:
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin  GOARCH=amd64 $(CGO) go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY)-darwin-amd64  ./cmd/guest-tunnel
	GOOS=darwin  GOARCH=arm64 $(CGO) go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY)-darwin-arm64  ./cmd/guest-tunnel
	GOOS=linux   GOARCH=amd64 $(CGO) go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY)-linux-amd64   ./cmd/guest-tunnel
	GOOS=linux   GOARCH=arm64 $(CGO) go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY)-linux-arm64   ./cmd/guest-tunnel
	GOOS=darwin  GOARCH=amd64 $(CGO) go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(HELPER_BINARY)-darwin-amd64  ./cmd/fido2-agent
	GOOS=darwin  GOARCH=arm64 $(CGO) go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(HELPER_BINARY)-darwin-arm64  ./cmd/fido2-agent
	GOOS=linux   GOARCH=amd64 $(CGO) go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(HELPER_BINARY)-linux-amd64   ./cmd/fido2-agent
	GOOS=linux   GOARCH=arm64 $(CGO) go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(HELPER_BINARY)-linux-arm64   ./cmd/fido2-agent
	@echo "Cross-compiled binaries in $(BUILD_DIR)/"

## clean: Remove build artifacts
clean:
	rm -rf $(BUILD_DIR)

## help: Show this help
help:
	@grep -E '^## ' Makefile | sed 's/## /  /'

## container-smoke: Run the Apple Container happy-path smoke test
container-smoke:
	./scripts/apple-container-integration.sh smoke

## container-test: Run Apple Container happy-path and failure-path tests
container-test:
	./scripts/apple-container-integration.sh test
