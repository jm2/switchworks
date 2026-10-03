VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
LDFLAGS := -s -w -X github.com/jm2/switchyard/internal/version.Version=$(VERSION)
TARGETS := linux/amd64 linux/arm64 darwin/arm64 windows/amd64

.PHONY: build test test-codex vet cross clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o swyd ./cmd/swyd

vet:
	go vet ./...

test: vet
	go test -race ./...

# Includes the integration test against the real codex binary (and a local
# mock model provider; no tokens are spent).
test-codex: vet
	SWYD_CODEX_IT=1 go test -race ./...

cross:
	@mkdir -p dist
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o dist/swyd-$$os-$$arch$$ext ./cmd/swyd || exit 1; \
	done

clean:
	rm -rf swyd dist
