# Esben's MIDI Mainframe (EMM)
#
#   make run        run the daemon on this machine with simulated devices (UI on :8099)
#   make test       unit tests with the race detector
#   make dist       static Linux binaries for the Pi (arm64 + armv7) in dist/
#   make deploy HOST=pi@emm.local   build, copy and install on a Raspberry Pi OS Lite install
#   make image-check / make image   Buildroot SD-card image (see docs/SDCARD-IMAGE.md)

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO      ?= go
HOST    ?=

# On recent macOS, Go 1.22's internal linker produces test binaries for
# net-using packages that dyld refuses to start ("missing LC_UUID"); linking
# externally avoids it. Harmless elsewhere, so only applied on macOS.
ifeq ($(shell uname -s),Darwin)
TEST_LDFLAGS := -ldflags=-linkmode=external
endif

LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build run test vet fmt check dist dist-arm64 dist-armv7 deploy list-devices clean lint-scripts image-configs image-check image

all: build

build:
	$(GO) build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/emmd ./cmd/emmd
ifeq ($(shell uname -s),Darwin)
	@# Go 1.22's linker-made ad-hoc signature is rejected by recent macOS (the binary
	@# is killed on launch with no message); signing it again with codesign fixes that.
	codesign --force -s - bin/emmd
endif

run: build
	@mkdir -p emm-state
	./bin/emmd --backend sim --netcfg fake --listen 127.0.0.1:8099 \
		--ui-dir internal/web/ui --state emm-state/state.json --boot-dir "" --log-level debug

list-devices: build
	./bin/emmd --list-devices

vet:
	$(GO) vet ./...

fmt:
	@test -z "$$(gofmt -l cmd internal)" || (gofmt -l cmd internal; echo "gofmt needed"; exit 1)

test:
	$(GO) test -race -count=1 $(TEST_LDFLAGS) ./...

lint-scripts:
	bash -n scripts/*.sh image/*.sh image/board/emm/*.sh
	image/mkconfigs.sh --check

# Everything CI would check, including that the Linux targets still build.
check: fmt vet test lint-scripts
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build -o /dev/null ./cmd/emmd
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 $(GO) build -o /dev/null ./cmd/emmd
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) vet ./...

dist: dist-arm64 dist-armv7

dist-arm64:
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/emmd-linux-arm64 ./cmd/emmd

dist-armv7:
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/emmd-linux-armv7 ./cmd/emmd

# --- SD-card appliance image (Buildroot; needs Docker, 15-30 GB, hours) -------
image-configs:
	image/mkconfigs.sh

# Validates both Buildroot defconfigs against the real Kconfig; takes minutes.
image-check:
	image/build.sh --check-only

image: dist-arm64
	image/build.sh

deploy:
	@test -n "$(HOST)" || (echo "usage: make deploy HOST=pi@emm.local"; exit 1)
	./scripts/deploy.sh $(HOST)

clean:
	rm -rf bin dist emm-state
