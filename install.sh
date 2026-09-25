#!/bin/sh
# Antigravity Cross-Device Synchronizer (agy-sync)
# Automated Zero-Touch Installer & Environment Bootstrapper
# Compatible with Android Termux & Linux (x86_64, aarch64)

if [ -z "$BASH_VERSION" ]; then
    if [ -x "/data/data/com.termux/files/usr/bin/bash" ]; then
        exec /data/data/com.termux/files/usr/bin/bash "$0" "$@"
    elif command -v bash >/dev/null 2>&1; then
        exec bash "$0" "$@"
    fi
fi

set -e

COLOR_CYAN='\033[1;36m'
COLOR_GREEN='\033[1;32m'
COLOR_YELLOW='\033[1;33m'
COLOR_RED='\033[1;31m'
COLOR_RESET='\033[0m'

echo -e "\n${COLOR_CYAN}┌─────────────────────────────────────────────────────────────────┐${COLOR_RESET}"
echo -e "${COLOR_CYAN}│  ✨ Antigravity Cross-Device Synchronizer (agy-sync v9.0)       │${COLOR_RESET}"
echo -e "${COLOR_CYAN}│  Automated Zero-Touch Installer & Environment Setup             │${COLOR_RESET}"
echo -e "${COLOR_CYAN}└─────────────────────────────────────────────────────────────────┘${COLOR_RESET}\n"

# 1. Environment & Architecture Detection
IS_TERMUX=false
if [ -d "/data/data/com.termux" ] || [ -n "$TERMUX_VERSION" ]; then
    IS_TERMUX=true
fi

ARCH="$(uname -m)"
case "$ARCH" in
    x86_64|amd64)
        TARGET_ARCH="amd64"
        ;;
    aarch64|arm64)
        TARGET_ARCH="arm64"
        ;;
    *)
        echo -e "${COLOR_RED}[❌] Unsupported CPU architecture: ${ARCH}${COLOR_RESET}"
        exit 1
        ;;
esac

OS_TYPE="$(uname -s | tr '[:upper:]' '[:lower:]')"
if [ "$OS_TYPE" != "linux" ] && [ "$IS_TERMUX" = false ]; then
    echo -e "${COLOR_RED}[❌] Unsupported OS: ${OS_TYPE}. agy-sync currently supports Linux and Android Termux.${COLOR_RESET}"
    exit 1
fi

echo -e "[🔍] Detected Platform: $(if [ "$IS_TERMUX" = true ]; then echo "Android Termux"; else echo "Linux"; fi) (${TARGET_ARCH})"

# 2. Dependency Check & Auto-Installation
echo -e "[📦] Checking foundational system packages..."
MISSING_PKGS=()

if ! command -v curl >/dev/null 2>&1; then MISSING_PKGS+=("curl"); fi
if ! command -v git >/dev/null 2>&1; then MISSING_PKGS+=("git"); fi
if ! command -v ssh >/dev/null 2>&1; then MISSING_PKGS+=("openssh"); fi
if ! command -v python3 >/dev/null 2>&1 && ! command -v python >/dev/null 2>&1; then MISSING_PKGS+=("python3"); fi

if [ ${#MISSING_PKGS[@]} -gt 0 ]; then
    echo -e "[⬇️] Installing missing dependencies: ${MISSING_PKGS[*]}..."
    if [ "$IS_TERMUX" = true ]; then
        pkg update -y >/dev/null 2>&1 || true
        pkg install -y "${MISSING_PKGS[@]}"
    elif command -v apt-get >/dev/null 2>&1; then
        sudo apt-get update -y >/dev/null 2>&1 || true
        sudo apt-get install -y "${MISSING_PKGS[@]}"
    elif command -v pacman >/dev/null 2>&1; then
        sudo pacman -Sy --noconfirm "${MISSING_PKGS[@]}"
    elif command -v dnf >/dev/null 2>&1; then
        sudo dnf install -y "${MISSING_PKGS[@]}"
    else
        echo -e "${COLOR_YELLOW}[⚠️] Could not detect package manager. Please manually install: ${MISSING_PKGS[*]}${COLOR_RESET}"
    fi
fi

# 3. Antigravity CLI (agy) Validation & Auto-Install
echo -e "[🔍] Checking Antigravity CLI (agy)..."
if command -v agy >/dev/null 2>&1; then
    AGY_VER="$(agy --version 2>/dev/null || echo "installed")"
    echo -e "${COLOR_GREEN}[✅] Antigravity CLI ready: ${AGY_VER}${COLOR_RESET}"
else
    echo -e "${COLOR_YELLOW}[🟡] Antigravity CLI not found. Installing now...${COLOR_RESET}"
    if [ "$IS_TERMUX" = true ]; then
        echo -e "[⬇️] Installing via Termux port (wallentx/antigravity-cli-termux)..."
        curl -fsSL https://raw.githubusercontent.com/wallentx/antigravity-cli-termux/dev/install.sh | bash
    else
        echo -e "[⬇️] Installing official Antigravity CLI binary..."
        curl -fsSL https://antigravity.google/cli/install.sh | bash
    fi
fi

# 4. Canonical Binary Relocation (~/.gemini/antigravity-cli/bin/agy-sync)
CANONICAL_DIR="$HOME/.gemini/antigravity-cli/bin"
CANONICAL_BIN="$CANONICAL_DIR/agy-sync"
mkdir -p "$CANONICAL_DIR"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [ -f "$SCRIPT_DIR/bin/agy-sync" ]; then
    echo -e "[📦] Installing binary from repository build..."
    cp -f "$SCRIPT_DIR/bin/agy-sync" "$CANONICAL_BIN"
elif [ -f "$SCRIPT_DIR/bin/agy-sync-linux-${TARGET_ARCH}" ]; then
    echo -e "[📦] Installing pre-built architecture binary (linux-${TARGET_ARCH})..."
    cp -f "$SCRIPT_DIR/bin/agy-sync-linux-${TARGET_ARCH}" "$CANONICAL_BIN"
elif command -v go >/dev/null 2>&1 && [ -f "$SCRIPT_DIR/cmd/agy-sync/main.go" ]; then
    echo -e "[🔨] Compiling agy-sync from Go source..."
    (cd "$SCRIPT_DIR" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$CANONICAL_BIN" ./cmd/agy-sync)
elif [ -f "./agy-sync" ]; then
    echo -e "[📦] Installing current directory agy-sync binary..."
    cp -f "./agy-sync" "$CANONICAL_BIN"
else
    # Fallback to downloading latest release or asking for build
    echo -e "${COLOR_RED}[❌] No pre-built agy-sync binary found in $SCRIPT_DIR/bin/ and 'go' compiler is missing.${COLOR_RESET}"
    exit 1
fi

# Install companion cross-architecture binaries if present (enables instant cross-arch pairing)
if [ -f "$SCRIPT_DIR/bin/agy-sync-linux-arm64" ]; then
    cp -f "$SCRIPT_DIR/bin/agy-sync-linux-arm64" "$CANONICAL_DIR/agy-sync-linux-arm64" 2>/dev/null || true
    chmod 755 "$CANONICAL_DIR/agy-sync-linux-arm64" 2>/dev/null || true
fi
if [ -f "$SCRIPT_DIR/bin/agy-sync-linux-amd64" ]; then
    cp -f "$SCRIPT_DIR/bin/agy-sync-linux-amd64" "$CANONICAL_DIR/agy-sync-linux-amd64" 2>/dev/null || true
    chmod 755 "$CANONICAL_DIR/agy-sync-linux-amd64" 2>/dev/null || true
fi

chmod 755 "$CANONICAL_BIN"

# 5. Global System Symlinks
echo -e "[🔗] Setting up global command links..."
if [ "$IS_TERMUX" = true ]; then
    ln -sf "$CANONICAL_BIN" /data/data/com.termux/files/usr/bin/agy-sync 2>/dev/null || true
    echo -e "    • Shortcut linked to /data/data/com.termux/files/usr/bin/agy-sync"
    if [[ ":$PATH:" != *":$CANONICAL_DIR:"* ]]; then
        echo -e "\nexport PATH=\"$CANONICAL_DIR:\$PATH\"" >> "$HOME/.bashrc"
        echo -e "    • Exported PATH to $HOME/.bashrc"
    fi
else
    mkdir -p "$HOME/.local/bin"
    ln -sf "$CANONICAL_BIN" "$HOME/.local/bin/agy-sync" 2>/dev/null || true
    echo -e "    • Shortcut linked to ~/.local/bin/agy-sync"
    if [ -w /usr/local/bin ]; then
        ln -sf "$CANONICAL_BIN" /usr/local/bin/agy-sync 2>/dev/null || true
        echo -e "    • Shortcut linked to /usr/local/bin/agy-sync"
    elif command -v sudo >/dev/null 2>&1; then
        sudo ln -sf "$CANONICAL_BIN" /usr/local/bin/agy-sync 2>/dev/null || true
    fi

    # Ensure PATH contains ~/.local/bin
    if [[ ":$PATH:" != *":$HOME/.local/bin:"* ]]; then
        RC_FILE="$HOME/.bashrc"
        if [ -n "$ZSH_VERSION" ] || [ -f "$HOME/.zshrc" ]; then
            RC_FILE="$HOME/.zshrc"
        fi
        echo -e "\nexport PATH=\"\$HOME/.local/bin:\$PATH\"" >> "$RC_FILE"
        echo -e "    • Exported PATH to $RC_FILE"
    fi
fi

# 6. SSH Keypair Validation
mkdir -p "$HOME/.ssh"
chmod 700 "$HOME/.ssh"
if [ ! -f "$HOME/.ssh/id_ed25519" ]; then
    echo -e "[🔑] Generating cluster SSH Ed25519 identity key..."
    ssh-keygen -t ed25519 -N "" -f "$HOME/.ssh/id_ed25519" -q
fi

# 7. Start SSH Daemon if on Termux
if [ "$IS_TERMUX" = true ]; then
    if ! pgrep -x sshd >/dev/null 2>&1; then
        echo -e "[🚀] Starting Termux OpenSSH daemon (sshd on :8022)..."
        sshd >/dev/null 2>&1 || true
    fi
fi

echo -e "\n${COLOR_GREEN}[🎉] agy-sync v9.0 successfully installed!${COLOR_RESET}"
echo -e "    • Canonical Location : ${CANONICAL_BIN}"
echo -e "    • CLI Shortcut       : agy-sync"
echo -e "\nRunning environment doctor...\n"

"$CANONICAL_BIN" doctor

echo -e "\n${COLOR_CYAN}👉 Next Step:${COLOR_RESET}"
echo -e "   • To connect this machine to your other devices, run: ${COLOR_CYAN}agy-sync init${COLOR_RESET} or ${COLOR_CYAN}agy-sync join <pairing-url>${COLOR_RESET}"
echo -e "   • To pair a new device from this machine, run: ${COLOR_CYAN}agy-sync pair${COLOR_RESET}\n"
