VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
LDFLAGS := -s -w -X github.com/jm2/switchyard/internal/version.Version=$(VERSION)
TARGETS := linux/amd64 linux/arm64 darwin/arm64 windows/amd64
# swyd-tray is built for Linux and Windows; macOS has the Swift menu-bar app.
TRAY_TARGETS := linux/amd64 linux/arm64 windows/amd64

.PHONY: build tray test test-codex vet fmt-check cross bindir deb clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o swyd ./cmd/swyd

tray:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o swyd-tray ./cmd/swyd-tray

vet:
	go vet ./...

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

test: vet
	go test -race ./...

# Includes the integration test against the real codex binary (and a local
# mock model provider; no tokens are spent).
test-codex: vet
	SWYD_CODEX_IT=1 go test -race ./...

# dist/swyd-<os>-<arch>[.exe], dist/swyd-tray-<os>-<arch>[.exe], and
# dist/swydw-windows-amd64.exe: swyd built as a Windows GUI program, which
# the logon task runs so no console window opens.
cross:
	@mkdir -p dist
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "building swyd $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o dist/swyd-$$os-$$arch$$ext ./cmd/swyd || exit 1; \
		if [ $$os = windows ]; then \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS) -H=windowsgui" -o dist/swydw-$$os-$$arch$$ext ./cmd/swyd || exit 1; \
		fi; \
	done
	@for t in $(TRAY_TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; ext=; gui=; [ $$os = windows ] && ext=.exe && gui=-H=windowsgui; \
		echo "building swyd-tray $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS) $$gui" -o dist/swyd-tray-$$os-$$arch$$ext ./cmd/swyd-tray || exit 1; \
	done

# dist/linux-<arch>/{swyd,swyd-tray}: the layout the package scripts take.
ARCH ?= amd64
bindir:
	@test -f dist/swyd-linux-$(ARCH) || { echo "run make cross first"; exit 1; }
	@mkdir -p dist/linux-$(ARCH)
	cp dist/swyd-linux-$(ARCH) dist/linux-$(ARCH)/swyd
	cp dist/swyd-tray-linux-$(ARCH) dist/linux-$(ARCH)/swyd-tray

# A .deb for ARCH (amd64 or arm64) from the cross-built binaries.
deb: bindir
	build-aux/linux/build-deb.sh "$(VERSION)" $(ARCH) dist/linux-$(ARCH) dist/switchyard-$(ARCH).deb

clean:
	rm -rf swyd swyd-tray dist
