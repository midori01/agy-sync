#!/bin/sh
# Antigravity Cross-Device Synchronizer (agy-sync)
# Automated Zero-Touch Installer & Environment Bootstrapper
# Compatible with Android Termux & Linux (x86_64, aarch64)

set -e

COLOR_CYAN='\033[1;36m'
COLOR_GREEN='\033[1;32m'
COLOR_YELLOW='\033[1;33m'
COLOR_RED='\033[1;31m'
COLOR_RESET='\033[0m'

printf "\n%b┌─────────────────────────────────────────────────────────────────┐%b\n" "${COLOR_CYAN}" "${COLOR_RESET}"
printf "%b│  ✨ Antigravity Cross-Device Synchronizer (agy-sync v9.0)       │%b\n" "${COLOR_CYAN}" "${COLOR_RESET}"
printf "%b│  Automated Zero-Touch Installer & Environment Setup             │%b\n" "${COLOR_CYAN}" "${COLOR_RESET}"
printf "%b└─────────────────────────────────────────────────────────────────┘%b\n\n" "${COLOR_CYAN}" "${COLOR_RESET}"

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
        printf "%b[❌] Unsupported CPU architecture: %s%b\n" "${COLOR_RED}" "${ARCH}" "${COLOR_RESET}"
        exit 1
        ;;
esac

OS_TYPE="$(uname -s | tr '[:upper:]' '[:lower:]')"
if [ "$OS_TYPE" = "darwin" ]; then
    OS_NAME="darwin"
    printf "[🔍] Detected Platform: macOS (%s)\n" "${TARGET_ARCH}"
elif [ "$OS_TYPE" = "linux" ] || [ "$IS_TERMUX" = true ]; then
    OS_NAME="linux"
    if [ "$IS_TERMUX" = true ]; then
        printf "[🔍] Detected Platform: Android Termux (%s)\n" "${TARGET_ARCH}"
    else
        printf "[🔍] Detected Platform: Linux (%s)\n" "${TARGET_ARCH}"
    fi
else
    printf "%b[❌] Unsupported OS: %s. agy-sync currently supports Linux, Android Termux, and macOS.%b\n" "${COLOR_RED}" "${OS_TYPE}" "${COLOR_RESET}"
    exit 1
fi

# 2. Dependency Check & Auto-Installation
printf "[📦] Checking foundational system packages...\n"
MISSING_PKGS=""

if ! command -v curl >/dev/null 2>&1; then MISSING_PKGS="${MISSING_PKGS} curl"; fi
if ! command -v git >/dev/null 2>&1; then MISSING_PKGS="${MISSING_PKGS} git"; fi
if ! command -v ssh >/dev/null 2>&1; then MISSING_PKGS="${MISSING_PKGS} openssh"; fi
if ! command -v python3 >/dev/null 2>&1 && ! command -v python >/dev/null 2>&1; then MISSING_PKGS="${MISSING_PKGS} python3"; fi

if [ -n "$MISSING_PKGS" ]; then
    printf "[⬇️] Installing missing dependencies:%s...\n" "${MISSING_PKGS}"
    SUDO="sudo"
    if [ "$(id -u)" -eq 0 ] || ! command -v sudo >/dev/null 2>&1; then
        SUDO=""
    fi

    if [ "$IS_TERMUX" = true ]; then
        pkg update -y >/dev/null 2>&1 || true
        # shellcheck disable=SC2086
        pkg install -y $MISSING_PKGS
    elif command -v apt-get >/dev/null 2>&1; then
        $SUDO apt-get update -y >/dev/null 2>&1 || true
        # shellcheck disable=SC2086
        $SUDO apt-get install -y $MISSING_PKGS
    elif command -v pacman >/dev/null 2>&1; then
        # shellcheck disable=SC2086
        $SUDO pacman -Sy --noconfirm $MISSING_PKGS
    elif command -v dnf >/dev/null 2>&1; then
        # shellcheck disable=SC2086
        $SUDO dnf install -y $MISSING_PKGS
    elif command -v zypper >/dev/null 2>&1; then
        # shellcheck disable=SC2086
        $SUDO zypper install -y $MISSING_PKGS
    elif command -v apk >/dev/null 2>&1; then
        # shellcheck disable=SC2086
        $SUDO apk add $MISSING_PKGS
    elif command -v brew >/dev/null 2>&1; then
        # shellcheck disable=SC2086
        brew install $MISSING_PKGS || true
    else
        printf "%b[⚠️] Could not detect package manager. Please manually install:%s%b\n" "${COLOR_YELLOW}" "${MISSING_PKGS}" "${COLOR_RESET}"
    fi
fi

# 3. Antigravity CLI (agy) Validation & Auto-Install
printf "[🔍] Checking Antigravity CLI (agy)...\n"
if command -v agy >/dev/null 2>&1; then
    AGY_VER="$(agy --version 2>/dev/null || echo "installed")"
    printf "%b[✅] Antigravity CLI ready: %s%b\n" "${COLOR_GREEN}" "${AGY_VER}" "${COLOR_RESET}"
else
    printf "%b[🟡] Antigravity CLI not found. Installing now...%b\n" "${COLOR_YELLOW}" "${COLOR_RESET}"
    if [ "$IS_TERMUX" = true ]; then
        printf "[⬇️] Installing via Termux port (wallentx/antigravity-cli-termux)...\n"
        curl -fsSL https://raw.githubusercontent.com/wallentx/antigravity-cli-termux/dev/install.sh | bash
    else
        printf "[⬇️] Installing official Antigravity CLI binary...\n"
        curl -fsSL https://antigravity.google/cli/install.sh | bash
    fi
fi

# 4. Canonical Binary Relocation (~/.gemini/antigravity-cli/bin/agy-sync)
CANONICAL_DIR="$HOME/.gemini/antigravity-cli/bin"
CANONICAL_BIN="$CANONICAL_DIR/agy-sync"
mkdir -p "$CANONICAL_DIR"

if [ -n "$BASH_SOURCE" ]; then
    SCRIPT_DIR="$(cd "$(dirname "$BASH_SOURCE")" && pwd)"
else
    SCRIPT_DIR="$(cd "$(dirname "$0")" 2>/dev/null && pwd || pwd)"
fi

if [ -f "$SCRIPT_DIR/bin/agy-sync-${OS_NAME}-${TARGET_ARCH}" ]; then
    printf "[📦] Installing pre-built architecture binary (%s-%s)...\n" "${OS_NAME}" "${TARGET_ARCH}"
    cp -f "$SCRIPT_DIR/bin/agy-sync-${OS_NAME}-${TARGET_ARCH}" "$CANONICAL_BIN"
elif [ -f "$SCRIPT_DIR/bin/agy-sync" ] && [ "$OS_NAME" = "linux" ]; then
    printf "[📦] Installing binary from repository build...\n"
    cp -f "$SCRIPT_DIR/bin/agy-sync" "$CANONICAL_BIN"
elif command -v go >/dev/null 2>&1 && [ -f "$SCRIPT_DIR/cmd/agy-sync/main.go" ]; then
    printf "[🔨] Compiling agy-sync from Go source...\n"
    (cd "$SCRIPT_DIR" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$CANONICAL_BIN" ./cmd/agy-sync)
elif [ -f "$SCRIPT_DIR/bin/agy-sync" ]; then
    printf "[📦] Installing binary from repository build...\n"
    cp -f "$SCRIPT_DIR/bin/agy-sync" "$CANONICAL_BIN"
elif [ -f "./agy-sync" ]; then
    printf "[📦] Installing current directory agy-sync binary...\n"
    cp -f "./agy-sync" "$CANONICAL_BIN"
else
    printf "%b[❌] No pre-built agy-sync binary found in %s/bin/ and 'go' compiler is missing.%b\n" "${COLOR_RED}" "${SCRIPT_DIR}" "${COLOR_RESET}"
    exit 1
fi

# Install companion cross-architecture binaries if present (enables instant cross-platform pairing)
for comp in linux-arm64 linux-amd64 darwin-arm64 darwin-amd64; do
    if [ -f "$SCRIPT_DIR/bin/agy-sync-$comp" ]; then
        cp -f "$SCRIPT_DIR/bin/agy-sync-$comp" "$CANONICAL_DIR/agy-sync-$comp" 2>/dev/null || true
        chmod 755 "$CANONICAL_DIR/agy-sync-$comp" 2>/dev/null || true
    fi
done

chmod 755 "$CANONICAL_BIN"

# 5. Global System Symlinks
printf "[🔗] Setting up global command links...\n"
if [ "$IS_TERMUX" = true ]; then
    ln -sf "$CANONICAL_BIN" /data/data/com.termux/files/usr/bin/agy-sync 2>/dev/null || true
    printf "    • Shortcut linked to /data/data/com.termux/files/usr/bin/agy-sync\n"
    case ":$PATH:" in
        *":$CANONICAL_DIR:"*) ;;
        *)
            printf "\nexport PATH=\"%s:\$PATH\"\n" "$CANONICAL_DIR" >> "$HOME/.bashrc"
            printf "    • Exported PATH to %s/.bashrc\n" "$HOME"
            ;;
    esac
else
    mkdir -p "$HOME/.local/bin"
    ln -sf "$CANONICAL_BIN" "$HOME/.local/bin/agy-sync" 2>/dev/null || true
    printf "    • Shortcut linked to ~/.local/bin/agy-sync\n"
    if [ -w /usr/local/bin ]; then
        ln -sf "$CANONICAL_BIN" /usr/local/bin/agy-sync 2>/dev/null || true
        printf "    • Shortcut linked to /usr/local/bin/agy-sync\n"
    elif command -v sudo >/dev/null 2>&1; then
        sudo ln -sf "$CANONICAL_BIN" /usr/local/bin/agy-sync 2>/dev/null || true
    fi

    # Ensure PATH contains ~/.local/bin across user profiles
    for rc in "$HOME/.bashrc" "$HOME/.zshrc" "$HOME/.zprofile" "$HOME/.bash_profile"; do
        if [ -f "$rc" ] || [ "$(basename "$rc")" = ".bashrc" ] || [ "$(basename "$rc")" = ".zshrc" ]; then
            touch "$rc" 2>/dev/null || true
            case ":$PATH:" in
                *":$HOME/.local/bin:"*) ;;
                *)
                    if ! grep -Fq 'PATH="$HOME/.local/bin:$PATH"' "$rc" 2>/dev/null; then
                        printf '\nexport PATH="$HOME/.local/bin:$PATH"\n' >> "$rc"
                        printf "    • Exported PATH to %s\n" "$rc"
                    fi
                    ;;
            esac
        fi
    done
fi

# 6. SSH Keypair Validation
mkdir -p "$HOME/.ssh"
chmod 700 "$HOME/.ssh"
if [ ! -f "$HOME/.ssh/id_ed25519" ]; then
    printf "[🔑] Generating cluster SSH Ed25519 identity key...\n"
    ssh-keygen -t ed25519 -N "" -f "$HOME/.ssh/id_ed25519" -q
fi
chmod 600 "$HOME/.ssh/id_ed25519" 2>/dev/null || true
if [ -f "$HOME/.ssh/id_ed25519.pub" ]; then
    chmod 644 "$HOME/.ssh/id_ed25519.pub" 2>/dev/null || true
fi

# 7. Start SSH Daemon if on Termux
if [ "$IS_TERMUX" = true ]; then
    if ! pgrep -x sshd >/dev/null 2>&1; then
        printf "[🚀] Starting Termux OpenSSH daemon (sshd on :8022)...\n"
        sshd >/dev/null 2>&1 || true
    fi
fi

printf "\n%b[🎉] agy-sync v9.0 successfully installed!%b\n" "${COLOR_GREEN}" "${COLOR_RESET}"
printf "    • Canonical Location : %s\n" "${CANONICAL_BIN}"
printf "    • CLI Shortcut       : agy-sync\n"
printf "\nRunning environment doctor...\n\n"

"$CANONICAL_BIN" doctor

printf "\n%b👉 Next Step:%b\n" "${COLOR_CYAN}" "${COLOR_RESET}"
printf "   • To connect this machine to your other devices, run: %bagy-sync init%b or %bagy-sync join <pairing-url>%b\n" "${COLOR_CYAN}" "${COLOR_RESET}" "${COLOR_CYAN}" "${COLOR_RESET}"
printf "   • To pair a new device from this machine, run: %bagy-sync pair%b\n\n" "${COLOR_CYAN}" "${COLOR_RESET}"
