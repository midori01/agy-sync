BINDIR ?= $(HOME)/.gemini/antigravity-cli/bin

IS_TERMUX := $(shell test -d /data/data/com.termux && echo 1 || echo 0)
UNAME_M := $(shell uname -m)

ifeq ($(IS_TERMUX),1)
HOST_BIN := bin/agy-sync-android-arm64
else ifeq ($(UNAME_M),x86_64)
HOST_BIN := bin/agy-sync-linux-amd64
else
HOST_BIN := bin/agy-sync-linux-arm64
endif

.PHONY: all clean android linux-arm64 linux-amd64 install

all: android linux-arm64 linux-amd64
	cp -f $(HOST_BIN) bin/agy-sync

android:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -ldflags="-s -w" -o bin/agy-sync-android-arm64 ./cmd/agy-sync

linux-arm64:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o bin/agy-sync-linux-arm64 ./cmd/agy-sync

linux-amd64:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/agy-sync-linux-amd64 ./cmd/agy-sync

install: all
	mkdir -p $(BINDIR)
	cp -f $(HOST_BIN) $(BINDIR)/agy-sync
	cp -f bin/agy-sync-android-arm64 $(BINDIR)/agy-sync-android-arm64
	cp -f bin/agy-sync-linux-arm64 $(BINDIR)/agy-sync-linux-arm64
	cp -f bin/agy-sync-linux-amd64 $(BINDIR)/agy-sync-linux-amd64
	chmod 755 $(BINDIR)/agy-sync*

clean:
	rm -rf bin/
