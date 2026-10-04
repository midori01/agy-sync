BINDIR ?= $(HOME)/.gemini/antigravity-cli/bin
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "dev")$(shell git diff --quiet 2>/dev/null || echo "-dirty")
LDFLAGS := -s -w -X agy-sync/pkg/config.GitCommit=$(GIT_COMMIT)

.PHONY: all clean install linux-amd64 linux-arm64 darwin-amd64 darwin-arm64 all-platforms

all:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/agy-sync ./cmd/agy-sync

linux-arm64:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/agy-sync-linux-arm64 ./cmd/agy-sync
	@echo "$(GIT_COMMIT)" > bin/agy-sync-linux-arm64.commit

linux-amd64:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/agy-sync-linux-amd64 ./cmd/agy-sync
	@echo "$(GIT_COMMIT)" > bin/agy-sync-linux-amd64.commit

darwin-arm64:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/agy-sync-darwin-arm64 ./cmd/agy-sync
	@echo "$(GIT_COMMIT)" > bin/agy-sync-darwin-arm64.commit

darwin-amd64:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/agy-sync-darwin-amd64 ./cmd/agy-sync
	@echo "$(GIT_COMMIT)" > bin/agy-sync-darwin-amd64.commit

all-platforms: linux-arm64 linux-amd64 darwin-arm64 darwin-amd64

install: all all-platforms
	mkdir -p $(BINDIR)
	cp -f bin/agy-sync $(BINDIR)/agy-sync
	@if [ -f bin/agy-sync-linux-arm64 ]; then cp -f bin/agy-sync-linux-arm64* $(BINDIR)/; fi
	@if [ -f bin/agy-sync-linux-amd64 ]; then cp -f bin/agy-sync-linux-amd64* $(BINDIR)/; fi
	@if [ -f bin/agy-sync-darwin-arm64 ]; then cp -f bin/agy-sync-darwin-arm64* $(BINDIR)/; fi
	@if [ -f bin/agy-sync-darwin-amd64 ]; then cp -f bin/agy-sync-darwin-amd64* $(BINDIR)/; fi
	chmod 755 $(BINDIR)/agy-sync*
	@if [ -d /data/data/com.termux/files/usr/bin ]; then ln -sf $(BINDIR)/agy-sync /data/data/com.termux/files/usr/bin/agy-sync 2>/dev/null || true; fi
	@mkdir -p $(HOME)/.local/bin && ln -sf $(BINDIR)/agy-sync $(HOME)/.local/bin/agy-sync 2>/dev/null || true

clean:
	rm -rf bin/
