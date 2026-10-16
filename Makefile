VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
LDFLAGS := -s -w -X github.com/jm2/switchworks/internal/version.Version=$(VERSION)
TARGETS := linux/amd64 linux/arm64 darwin/arm64 windows/amd64
# swk-tray is built for Linux and Windows; macOS has the Swift menu-bar app.
TRAY_TARGETS := linux/amd64 linux/arm64 windows/amd64

.PHONY: build tray test test-codex vet fmt-check cross bindir deb image clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o swk ./cmd/swk

tray:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o swk-tray ./cmd/swk-tray

vet:
	go vet ./...

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

test: vet
	go test -race ./...

# Includes the integration test against the real codex binary (and a local
# mock model provider; no tokens are spent).
test-codex: vet
	SWK_CODEX_IT=1 go test -race ./...

# dist/swk-<os>-<arch>[.exe], dist/swk-tray-<os>-<arch>[.exe], and
# dist/swkw-windows-amd64.exe: swk built as a Windows GUI program, which
# the logon task runs so no console window opens.
cross:
	@mkdir -p dist
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "building swk $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o dist/swk-$$os-$$arch$$ext ./cmd/swk || exit 1; \
		if [ $$os = windows ]; then \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS) -H=windowsgui" -o dist/swkw-$$os-$$arch$$ext ./cmd/swk || exit 1; \
		fi; \
	done
	@for t in $(TRAY_TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; ext=; gui=; [ $$os = windows ] && ext=.exe && gui=-H=windowsgui; \
		echo "building swk-tray $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS) $$gui" -o dist/swk-tray-$$os-$$arch$$ext ./cmd/swk-tray || exit 1; \
	done

# dist/linux-<arch>/{swk,swk-tray}: the layout the package scripts take.
ARCH ?= amd64
bindir:
	@test -f dist/swk-linux-$(ARCH) || { echo "run make cross first"; exit 1; }
	@mkdir -p dist/linux-$(ARCH)
	cp dist/swk-linux-$(ARCH) dist/linux-$(ARCH)/swk
	cp dist/swk-tray-linux-$(ARCH) dist/linux-$(ARCH)/swk-tray

# A .deb for ARCH (amd64 or arm64) from the cross-built binaries.
deb: bindir
	build-aux/linux/build-deb.sh "$(VERSION)" $(ARCH) dist/linux-$(ARCH) dist/switchworks-$(ARCH).deb

# The coordinator's container image for this machine's architecture, from
# the cross-built binary (CONTAINER=docker works too).
CONTAINER ?= podman
IMAGE ?= switchworks-coordinator:$(VERSION)
image:
	@arch=$$(go env GOARCH); test -f dist/swk-linux-$$arch || { echo "run make cross first"; exit 1; }; \
	$(CONTAINER) build -f build-aux/container/Containerfile --build-arg TARGETARCH=$$arch \
		--build-arg VERSION=$(VERSION) -t $(IMAGE) .

clean:
	rm -rf swk swk-tray dist
