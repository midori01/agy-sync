.PHONY: all clean android linux-arm64 linux-amd64 install

all: android linux-arm64 linux-amd64

android:
	mkdir -p bin
	go build -ldflags="-s -w" -o bin/agy-sync-android-arm64 ./cmd/agy-sync
	cp -f bin/agy-sync-android-arm64 bin/agy-sync

linux-arm64:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o bin/agy-sync-linux-arm64 ./cmd/agy-sync

linux-amd64:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/agy-sync-linux-amd64 ./cmd/agy-sync

install: all
	mkdir -p /data/data/com.termux/files/home/.gemini/antigravity-cli/bin
	cp -f bin/agy-sync /data/data/com.termux/files/home/.gemini/antigravity-cli/bin/agy-sync
	cp -f bin/agy-sync-android-arm64 /data/data/com.termux/files/home/.gemini/antigravity-cli/bin/agy-sync-android-arm64
	cp -f bin/agy-sync-linux-arm64 /data/data/com.termux/files/home/.gemini/antigravity-cli/bin/agy-sync-linux-arm64
	cp -f bin/agy-sync-linux-amd64 /data/data/com.termux/files/home/.gemini/antigravity-cli/bin/agy-sync-linux-amd64
	chmod 755 /data/data/com.termux/files/home/.gemini/antigravity-cli/bin/agy-sync*

clean:
	rm -rf bin/
