COMMANDS ?= magnet magnet-web
PREFIX   ?= /usr/local
DIST_DIR ?= dist
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)

# Cross-compilation targets. CGO is disabled so the binaries are portable and
# can be built from any host without a C toolchain. The torrent client falls
# back to its pure-Go uTP implementation; TCP, DHT and metadata lookups work.
PLATFORMS         ?= linux/amd64 linux/arm64 windows/amd64 windows/arm64 darwin/amd64 darwin/arm64
LINUX_PLATFORMS   ?= linux/amd64 linux/arm64
WINDOWS_PLATFORMS ?= windows/amd64 windows/arm64
DARWIN_PLATFORMS  ?= darwin/amd64 darwin/arm64

.PHONY: all build build-magnet build-magnet-web build-all build-platforms \
        build-linux build-windows build-darwin release test race vet install clean

all: build

build: build-magnet build-magnet-web

build-magnet:
	@mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/magnet ./cmd/magnet

build-magnet-web:
	@mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/magnet-web ./cmd/magnet-web

build-all:
	@$(MAKE) --no-print-directory build-platforms PLATFORMS="$(PLATFORMS)"

build-linux:
	@$(MAKE) --no-print-directory build-platforms PLATFORMS="$(LINUX_PLATFORMS)"

build-windows:
	@$(MAKE) --no-print-directory build-platforms PLATFORMS="$(WINDOWS_PLATFORMS)"

build-darwin:
	@$(MAKE) --no-print-directory build-platforms PLATFORMS="$(DARWIN_PLATFORMS)"

# Internal target used by the per-OS targets above.
build-platforms:
	@mkdir -p "$(DIST_DIR)"
	@cp -f LICENSE THIRD_PARTY_NOTICES.md "$(DIST_DIR)/"
	@set -eu; \
	for platform in $(PLATFORMS); do \
		os=$${platform%%/*}; \
		arch=$${platform##*/}; \
		ext=""; \
		if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		for cmd in $(COMMANDS); do \
			out="$(DIST_DIR)/$$cmd-$(VERSION)-$$os-$$arch$$ext"; \
			echo "  GOOS=$$os GOARCH=$$arch -> $$out"; \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
				go build -trimpath -ldflags "$(LDFLAGS)" -o "$$out" ./cmd/$$cmd; \
		done; \
	done; \
	cd "$(DIST_DIR)"; \
	if command -v sha256sum >/dev/null 2>&1; then \
		sha256sum magnet-* > SHA256SUMS; \
	elif command -v shasum >/dev/null 2>&1; then \
		shasum -a 256 magnet-* > SHA256SUMS; \
	fi

release: build-all

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

install: build
	@set -e; \
	for cmd in $(COMMANDS); do \
		install -Dm0755 bin/$$cmd $(DESTDIR)$(PREFIX)/bin/$$cmd; \
	done

clean:
	rm -rf bin dist
