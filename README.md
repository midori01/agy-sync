# agy-sync: Antigravity Cross-Device Full Bidirectional Synchronizer

[![Version](https://img.shields.io/badge/version-v9.4-blue.svg)](https://github.com/midori01/agy-sync)
[![Go: 1.22+](https://img.shields.io/badge/Go-1.22+-00ADD8.svg?logo=go)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Platform](https://img.shields.io/badge/Platform-Termux%20%7C%20Linux%20%7C%20macOS-green.svg)](#)
[![Dependencies](https://img.shields.io/badge/Dependencies-0%20(Pure%20Stdlib)-brightgreen.svg)](#)
[![100% Autonomous AI](https://img.shields.io/badge/built%20by-100%25%20Autonomous%20AI-purple.svg)](https://github.com/midori01/agy-sync)

**agy-sync** is a high-performance, decentralized peer-to-peer (P2P) mesh synchronization engine designed for **Google Antigravity CLI (`agy`)**. It seamlessly synchronizes conversation transcripts, session handoff context (`session_handoff.md`), SQLite conversation indices (`conversation_summaries.db`), multi-account quota pools ([`agy-pool`](https://github.com/midori01/agy-pool)), and user-defined directories across Android (Termux), Linux workstations, servers, and macOS laptops.

> [!NOTE]
> ### 🤖 100% Autonomous AI Engineering
> Every single line of code, architectural design, pipeline optimization, unit test suite, and troubleshooting routine in `agy-sync` was conceived, implemented, and verified autonomously by AI (Antigravity).

---

## 1. Overview

Developing with Antigravity across multiple machines (e.g., an Android smartphone running Termux, a Linux production server, and a macOS workstation) typically results in fragmented conversation history, out-of-sync session memory handoffs, and scattered quota pools.

**`agy-sync`** solves this by forming a decentralized, zero-dependency synchronization mesh. Utilizing SSH multiplexing, Tailscale/WireGuard auto-discovery, and streaming tar pipelines, it delivers sub-second bidirectional synchronization without relying on third-party cloud services.

---

## 2. Key Architecture & Features

- **⚡ Single-RTT Multi-Asset Inventory Pipeline**: Aggregates remote file existence, modification timestamps, sizes, and content checksums into a single round-trip pipelined SSH operation, ensuring sub-second execution even across high-latency cellular connections.
- **🚀 0-RTT SummariesDB Parity Short-Circuit**: Employs SQLite WAL-aware fast-path parity checks to instantly skip redundant SQLite database transfers when indices match.
- **🔗 Zero-Touch 1-Click Pairing & Auto-Enrollment**:
  - `agy-sync pair [port]`: Spins up an ephemeral 1-click HTTP pairing server over LAN or Tailnet.
  - `agy-sync join <pairing-url>`: Automatically handles Ed25519 SSH keypair negotiation, public key exchange, and peer registration without manual configuration.
  - `agy-sync enroll <user@host>`: Deploys and bootstraps remote headless servers via SSH with zero manual setup.
- **🌐 Intelligent Multi-Path Mesh Routing**: Automatically selects the fastest available route among LAN/Wi-Fi, WireGuard, and Tailscale CGNAT IPs (`100.x.y.z`), with built-in background keepalive routines to maintain NAT hole punching.
- **📁 Custom Directory Synchronization under `$HOME`**: Synchronizes arbitrary user project directories and files outside AGY internals (`agy-sync add ~/projects`, `agy-sync custom list`).
- **🛡️ Native `agy-pool` Integration**: Dedicated commands to synchronize multi-account quota pools and self-distribute engine binaries across all nodes (`agy-sync push-pool`, `agy-sync push-engine`).
- **📊 Real-time Bento Box Dashboard**: Terminal UI featuring collapsible Bento card dashboards, live transfer progress bars, and execution metrics.
- **🩺 Environment Diagnostics (`agy-sync doctor`)**: Automated checks for Go compilers, SSH daemons, network connectivity, and environment baselines.

---

## 3. CLI Command Matrix

| Command | Description |
| :--- | :--- |
| `agy-sync` | Synchronize with the default peer (Bento card dashboard view) |
| `agy-sync all [-f]` | Concurrently synchronize with all registered cluster nodes (Matrix table view; `-f` forces overwrite) |
| `agy-sync <alias> [-V]` | Synchronize with a specific peer alias (`-V` enables verbose streaming logs) |
| `agy-sync pair [port]` | Start a 1-click HTTP pairing server to invite a new node |
| `agy-sync join <url>` | Connect to an existing cluster using a pairing URL |
| `agy-sync enroll <target>` | Deploy and configure `agy-sync` onto a remote SSH host with zero touch |
| `agy-sync custom add <path>` | Add a file or directory under `$HOME` to the synchronization list (alias: `agy-sync add`) |
| `agy-sync custom list` | List all tracked custom sync items and their current states |
| `agy-sync push-pool` | Force-push the local `agy-pool` account configuration to cluster nodes |
| `agy-sync push-engine` | Force-distribute and install the `agy-pool` engine binary across all nodes |
| `agy-sync devices` | Inspect registered cluster devices, latency, and hardware architectures |
| `agy-sync items` | Display tracked AGY assets, sessions, and indices |
| `agy-sync doctor` | Perform full health checks on Go compiler, SSH service, and network topology |

---

## 4. Installation & Setup

### Zero-Touch Automated Installer
```bash
git clone https://github.com/midori01/agy-sync.git
cd agy-sync
./install.sh
```

### Build from Source
```bash
# Build binary for current platform
make

# Install to ~/.gemini/antigravity-cli/bin and link to system PATH
make install

# Cross-compile for all supported architectures
make all-platforms
```

---

## 5. Supported Platforms & Architectures

| Platform (OS) | CPU Architecture | Build Target | Validation Status |
| :--- | :--- | :--- | :--- |
| **Android (Termux)** | aarch64 / arm64 | `linux-arm64` | Verified in active production |
| **Linux (Ubuntu / Debian / Arch / RHEL)** | x86_64 / amd64 | `linux-amd64` | Verified (Static binary) |
| **Linux (ARM / Raspberry Pi)** | aarch64 / arm64 | `linux-arm64` | Verified (Static binary) |
| **macOS (Apple Silicon)** | arm64 (M1 / M2 / M3 / M4) | `darwin-arm64` | Verified (Static binary) |
| **macOS (Intel)** | x86_64 / amd64 | `darwin-amd64` | Verified (Static binary) |

---

## 6. Automated Testing

Run the complete Go test suite:
```bash
go test -v ./...
```

---

## 7. License

This project is licensed under the [MIT License](LICENSE).
