# clawk — build / install / sign
#
# Apple's Virtualization.framework refuses to run from an unsigned binary.
# For local dev, ad-hoc signing (`codesign -s -`) with the entitlements in
# clawk.entitlements is enough — no Apple Developer ID required.
#
# Primary targets:
#   make install     build + install + sign $(GOBIN)/clawk. Use this
#                    instead of `go run ./cmd/clawk` for your dev loop;
#                    go run recompiles to a cache path we can't sign.
#   make build       just build into ./bin/clawk (for CI / local tests).
#   make sign        sign an existing binary at BIN=path.

GOBIN ?= $(shell go env GOBIN)
ifeq ($(GOBIN),)
GOBIN := $(shell go env GOPATH)/bin
endif

BIN ?= ./bin/clawk
ENTITLEMENTS := clawk.entitlements
UNAME_S := $(shell uname -s)

.PHONY: install build sign test clean guestbin clean-guestbin

install:
	go install ./cmd/clawk
ifeq ($(UNAME_S),Darwin)
	codesign --entitlements $(ENTITLEMENTS) -s - --force $(GOBIN)/clawk
	@echo "  signed $(GOBIN)/clawk (ad-hoc)."
endif
	@echo "  clawk installed at $(GOBIN)/clawk"

build:
	mkdir -p $(dir $(BIN))
	go build -o $(BIN) ./cmd/clawk
ifeq ($(UNAME_S),Darwin)
	codesign --entitlements $(ENTITLEMENTS) -s - --force $(BIN)
endif

sign:
ifeq ($(UNAME_S),Darwin)
	codesign --entitlements $(ENTITLEMENTS) -s - --force $(BIN)
else
	@echo "sign target is a no-op on $(UNAME_S) — only macOS needs codesigning."
endif

test:
	go test ./...

# Cross-compile the in-guest binaries into internal/agentembed/prebuilt/ so the
# next `go build` embeds them and the resulting clawk needs no Go toolchain to
# boot a sandbox. Release builds run this first; source builds skip it and
# compile the guest binaries on first boot instead.
#
# GUEST_ARCH must match the architecture the artifact will RUN on: hardware
# virtualization can't cross architectures, so an arm64 clawk only boots arm64
# guests. Defaults to this machine's.
GUEST_ARCH ?= $(shell go env GOARCH)

guestbin:
	go run ./internal/guestbuild/cmd/gen-prebuilt -arch $(GUEST_ARCH)

# Drop the embedded payload, restoring a plain source build.
clean-guestbin:
	rm -f $(PREBUILT_DIR)/clawk-init $(PREBUILT_DIR)/clawk-pty-agent \
	      $(PREBUILT_DIR)/clawk-time-sync $(PREBUILT_DIR)/manifest.json

PREBUILT_DIR := internal/agentembed/prebuilt

clean: clean-guestbin
	rm -rf ./bin
