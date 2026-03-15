BINARY    := guest-tunnel
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_DIR := dist

LDFLAGS := -s -w -X main.Version=$(VERSION)
CGO     := CGO_ENABLED=0

.PHONY: all build cross clean help

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

## clean: Remove build artifacts
clean:
	rm -rf $(BUILD_DIR)

## help: Show this help
help:
	@grep -E '^## ' Makefile | sed 's/## /  /'
