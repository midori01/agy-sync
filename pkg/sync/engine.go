package sync

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"agy-sync/pkg/config"
	"agy-sync/pkg/ssh"
)

// ValidateBinaryArchitecture checks whether the provided byte slice is a valid binary matching targetOS and targetArch.
func ValidateBinaryArchitecture(data []byte, targetOS string, targetArch string) bool {
	if len(data) < 20 {
		return false
	}
	normArch := "arm64"
	if strings.Contains(targetArch, "x86_64") || strings.Contains(targetArch, "amd64") {
		normArch = "amd64"
	}
	normOS := strings.ToLower(strings.TrimSpace(targetOS))
	if normOS == "" || normOS == "android" {
		normOS = "linux"
	}

	if normOS == "darwin" || normOS == "macos" {
		// Universal / Fat Mach-O binary (0xCA 0xFE 0xBA 0xBE)
		if bytes.Equal(data[:4], []byte{0xCA, 0xFE, 0xBA, 0xBE}) {
			return true
		}
		// Mach-O 64-bit Little Endian (MH_MAGIC_64 = 0xFEEDFACF, bytes: 0xCF, 0xFA, 0xED, 0xFE)
		if bytes.Equal(data[:4], []byte{0xCF, 0xFA, 0xED, 0xFE}) {
			cpuType := uint32(data[4]) | (uint32(data[5]) << 8) | (uint32(data[6]) << 16) | (uint32(data[7]) << 24)
			if normArch == "amd64" {
				return cpuType == 0x01000007 // CPU_TYPE_X86_64
			} else if normArch == "arm64" {
				return cpuType == 0x0100000C // CPU_TYPE_ARM64
			}
		}
		return false
	}

	// Linux ELF
	if !bytes.Equal(data[:4], []byte{0x7F, 'E', 'L', 'F'}) {
		return false
	}
	// data[4] is EI_CLASS: 2 = ELFCLASS64
	if data[4] != 2 {
		return false
	}
	// data[5] is EI_DATA: 1 = ELFDATA2LSB, 2 = ELFDATA2MSB
	var eMachine uint16
	if data[5] == 1 {
		eMachine = uint16(data[18]) | (uint16(data[19]) << 8)
	} else if data[5] == 2 {
		eMachine = (uint16(data[18]) << 8) | uint16(data[19])
	} else {
		return false
	}

	if normArch == "amd64" {
		return eMachine == 0x3E // EM_X86_64
	} else if normArch == "arm64" {
		return eMachine == 0xB7 // EM_AARCH64
	}
	return false
}

// ValidateELFArchitecture checks whether the provided byte slice is a valid Linux ELF matching targetArch.
func ValidateELFArchitecture(data []byte, targetArch string) bool {
	return ValidateBinaryArchitecture(data, "linux", targetArch)
}

// GetOrBuildTargetBinary provides the correct binary bytes for a target architecture and OS.
// Supports GetOrBuildTargetBinary(arch), GetOrBuildTargetBinary(os, arch), and GetOrBuildTargetBinary("darwin/amd64").
func GetOrBuildTargetBinary(targetOSAndArch ...string) ([]byte, error) {
	targetOS := "linux"
	targetArch := "arm64"

	if len(targetOSAndArch) == 1 {
		arg := targetOSAndArch[0]
		if strings.Contains(arg, "/") {
			parts := strings.SplitN(arg, "/", 2)
			targetOS = parts[0]
			targetArch = parts[1]
		} else {
			targetArch = arg
		}
	} else if len(targetOSAndArch) >= 2 {
		targetOS = targetOSAndArch[0]
		targetArch = targetOSAndArch[1]
	}

	normOS := strings.ToLower(strings.TrimSpace(targetOS))
	if normOS == "" || normOS == "android" {
		normOS = "linux"
	}
	if normOS == "macos" {
		normOS = "darwin"
	}

	normArch := "arm64"
	if strings.Contains(targetArch, "x86_64") || strings.Contains(targetArch, "amd64") {
		normArch = "amd64"
	}

	// 1. Same-architecture and same-OS fast path: returns the running/installed binary directly.
	if normOS == runtime.GOOS && normArch == runtime.GOARCH {
		if data, err := os.ReadFile(config.AgySyncScript); err == nil && len(data) > 0 {
			if ValidateBinaryArchitecture(data, normOS, normArch) {
				return data, nil
			}
		}
	}

	// 2. Check canonical directory companion binary (~/.gemini/antigravity-cli/bin/agy-sync-<os>-<arch>)
	companionBin := filepath.Join(filepath.Dir(config.AgySyncScript), fmt.Sprintf("agy-sync-%s-%s", normOS, normArch))
	if data, err := os.ReadFile(companionBin); err == nil && len(data) > 0 {
		if ValidateBinaryArchitecture(data, normOS, normArch) {
			return data, nil
		}
	}

	// 3. Check repository cache in ~/agy-sync-go/bin/agy-sync-<os>-<arch>
	cachedBin := filepath.Join(config.AgySyncGoDir, "bin", fmt.Sprintf("agy-sync-%s-%s", normOS, normArch))
	stampFile := cachedBin + ".commit"
	if stamp, err := os.ReadFile(stampFile); err == nil && strings.TrimSpace(string(stamp)) == config.GitCommit && config.GitCommit != "dev" {
		if data, err := os.ReadFile(cachedBin); err == nil && len(data) > 0 {
			if ValidateBinaryArchitecture(data, normOS, normArch) {
				return data, nil
			}
		}
	}

	// 4. JIT cross-compilation: requires local Go toolchain
	if _, err := exec.LookPath("go"); err == nil {
		_ = os.MkdirAll(filepath.Join(config.AgySyncGoDir, "bin"), 0755)
		ldflags := fmt.Sprintf("-s -w -X agy-sync/pkg/config.GitCommit=%s", config.GitCommit)
		buildCmd := exec.Command("go", "build", "-trimpath", fmt.Sprintf("-ldflags=%s", ldflags), "-o", cachedBin, "./cmd/agy-sync")
		buildCmd.Dir = config.AgySyncGoDir
		buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0", fmt.Sprintf("GOOS=%s", normOS), fmt.Sprintf("GOARCH=%s", normArch))
		if out, err := buildCmd.CombinedOutput(); err == nil {
			_ = os.WriteFile(stampFile, []byte(config.GitCommit), 0644)
			if data, err := os.ReadFile(cachedBin); err == nil && len(data) > 0 {
				if ValidateBinaryArchitecture(data, normOS, normArch) {
					return data, nil
				}
			}
		} else {
			return nil, fmt.Errorf("JIT cross-compilation for %s/%s failed: %v (%s)", normOS, normArch, err, strings.TrimSpace(string(out)))
		}
	}

	return nil, fmt.Errorf("cross-architecture binary for '%s/%s' is unavailable (host is %s/%s, pre-built binary missing, and Go compiler not installed)", normOS, normArch, runtime.GOOS, runtime.GOARCH)
}

// PullRemoteBinaryForLocal retrieves the matching native binary from the remote node
// when the local machine lacks a Go compilation environment.
func PullRemoteBinaryForLocal(host string, port int, remHome string, remOS string, remArch string, remCommit string) ([]byte, error) {
	remBin := config.AgySyncScript
	if remHome != "" {
		remBin = config.ToRemotePath(config.AgySyncScript, remHome)
	}

	normLocalOS := runtime.GOOS
	normLocalArch := runtime.GOARCH

	normRemOS := strings.ToLower(strings.TrimSpace(remOS))
	if normRemOS == "" || normRemOS == "android" {
		normRemOS = "linux"
	}
	normRemArch := "arm64"
	if strings.Contains(remArch, "x86_64") || strings.Contains(remArch, "amd64") {
		normRemArch = "amd64"
	}

	// 1. Same-architecture and same-OS: remote's installed binary is already built for local!
	if normLocalOS == normRemOS && normLocalArch == normRemArch {
		stdout, _, _, err := ssh.RunSSHCmd(host, port, fmt.Sprintf("cat '%s' 2>/dev/null", remBin), false)
		if err == nil && len(stdout) >= 20 {
			data := []byte(stdout)
			if ValidateBinaryArchitecture(data, normLocalOS, normLocalArch) {
				return data, nil
			}
		}
	}

	// 2. Request remote node (which has Go or cache) to supply local's binary
	remGoDir := fmt.Sprintf("%s/agy-sync-go", remHome)
	pullCmd := fmt.Sprintf(`
cached="%s/bin/agy-sync-%s-%s"
if [ -f "$cached" ] && [ -f "$cached.commit" ] && [ "$(cat "$cached.commit" 2>/dev/null)" = "%s" ]; then
    cat "$cached"
elif command -v go >/dev/null 2>&1 && [ -d "%s" ]; then
    mkdir -p "%s/bin" && \
    (cd "%s" && CGO_ENABLED=0 GOOS=%s GOARCH=%s go build -trimpath -ldflags="-s -w -X agy-sync/pkg/config.GitCommit=%s" -o "bin/agy-sync-%s-%s" ./cmd/agy-sync 2>/dev/null) && \
    echo "%s" > "%s/bin/agy-sync-%s-%s.commit" && \
    cat "%s/bin/agy-sync-%s-%s"
fi`, remGoDir, normLocalOS, normLocalArch, remCommit, remGoDir, remGoDir, remGoDir, normLocalOS, normLocalArch, remCommit, normLocalOS, normLocalArch, remCommit, remGoDir, normLocalOS, normLocalArch, remGoDir, normLocalOS, normLocalArch)

	stdout, _, _, err := ssh.RunSSHCmd(host, port, pullCmd, false)
	if err == nil && len(stdout) >= 20 {
		data := []byte(stdout)
		if ValidateBinaryArchitecture(data, normLocalOS, normLocalArch) {
			return data, nil
		}
	}

	return nil, fmt.Errorf("remote %s cannot supply %s/%s binary", host, normLocalOS, normLocalArch)
}

func SyncEngine(host string, port int, remHome string, remArch string, remCommit string, goSrcStat string, clockSkew float64, probeCache map[string]*RemoteFileInfo, isForce bool, remOS string, tracker ...*ByteTracker) string {
	fi, err := os.Stat(config.AgySyncScript)
	if err != nil || fi.IsDir() {
		return "none"
	}

	remBin := config.AgySyncScript
	if remHome != "" {
		remBin = config.ToRemotePath(config.AgySyncScript, remHome)
	}

	normRemOS := strings.ToLower(strings.TrimSpace(remOS))
	if normRemOS == "" || normRemOS == "android" {
		normRemOS = "linux"
	}
	if normRemOS == "macos" {
		normRemOS = "darwin"
	}

	// 1. 1-RTT Commit Match: If binary commits match and not forced, 100% identical across any architecture
	if !isForce && remCommit != "" && remCommit != "NONE" && remCommit != "dev" && config.GitCommit != "dev" && remCommit == config.GitCommit {
		return "identical"
	}

	// 2. If source code was PULLED from remote (remote was newer!):
	// Local MUST NOT push its old binary to remote!
	// Update local binary for local's architecture:
	if goSrcStat == "pulled" || goSrcStat == "pulled & ready" {
		// A. Try local compilation if local node has Go compiler
		if _, err := exec.LookPath("go"); err == nil {
			out, err := exec.Command("git", "-C", config.AgySyncGoDir, "rev-parse", "--short", "HEAD").Output()
			if err == nil {
				newCommit := strings.TrimSpace(string(out))
				ldflags := fmt.Sprintf("-s -w -X agy-sync/pkg/config.GitCommit=%s", newCommit)
				tmpLocal := fmt.Sprintf("%s.tmp_%d_%s", config.AgySyncScript, time.Now().UnixNano(), randomToken(3))
				buildCmd := exec.Command("go", "build", "-trimpath", fmt.Sprintf("-ldflags=%s", ldflags), "-o", tmpLocal, "./cmd/agy-sync")
				buildCmd.Dir = config.AgySyncGoDir
				buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0", fmt.Sprintf("GOOS=%s", runtime.GOOS), fmt.Sprintf("GOARCH=%s", runtime.GOARCH))
				if err := buildCmd.Run(); err == nil {
					_ = os.Rename(tmpLocal, config.AgySyncScript)
					_ = os.Chmod(config.AgySyncScript, 0755)
					config.GitCommit = newCommit
					if _, err := os.Stat("/data/data/com.termux/files/usr/bin"); err == nil {
						_ = os.Remove("/data/data/com.termux/files/usr/bin/agy-sync")
						_ = os.Symlink(config.AgySyncScript, "/data/data/com.termux/files/usr/bin/agy-sync")
					}
					_ = os.MkdirAll(filepath.Join(config.HomeDir, ".local", "bin"), 0755)
					_ = os.Remove(filepath.Join(config.HomeDir, ".local", "bin", "agy-sync"))
					_ = os.Symlink(config.AgySyncScript, filepath.Join(config.HomeDir, ".local", "bin", "agy-sync"))
					return "pulled & rebuilt locally"
				}
				_ = os.Remove(tmpLocal)
			}
		}

		// B. Fallback for devices WITHOUT Go compiler: pull ready binary from remote!
		if binData, err := PullRemoteBinaryForLocal(host, port, remHome, normRemOS, remArch, remCommit); err == nil && len(binData) > 0 {
			_ = os.MkdirAll(filepath.Dir(config.AgySyncScript), 0755)
			tmpLocal := fmt.Sprintf("%s.tmp_%d_%s", config.AgySyncScript, time.Now().UnixNano(), randomToken(3))
			if err := os.WriteFile(tmpLocal, binData, 0755); err == nil {
				_ = os.Rename(tmpLocal, config.AgySyncScript)
				_ = os.Chmod(config.AgySyncScript, 0755)
				config.GitCommit = remCommit
				if len(tracker) > 0 && tracker[0] != nil {
					tracker[0].AddPull(int64(len(binData)))
				}
				if _, err := os.Stat("/data/data/com.termux/files/usr/bin"); err == nil {
					_ = os.Remove("/data/data/com.termux/files/usr/bin/agy-sync")
					_ = os.Symlink(config.AgySyncScript, "/data/data/com.termux/files/usr/bin/agy-sync")
				}
				_ = os.MkdirAll(filepath.Join(config.HomeDir, ".local", "bin"), 0755)
				_ = os.Remove(filepath.Join(config.HomeDir, ".local", "bin", "agy-sync"))
				_ = os.Symlink(config.AgySyncScript, filepath.Join(config.HomeDir, ".local", "bin", "agy-sync"))
				return "pulled binary from peer"
			}
			_ = os.Remove(tmpLocal)
		}

		return "pulled"
	}

	// 3. Same-Architecture and Same-OS Direct Byte Check (Fallback for dev or unversioned binaries)
	isRemSameArch := (strings.Contains(remArch, "x86_64") || strings.Contains(remArch, "amd64")) == (runtime.GOARCH == "amd64") && (normRemOS == runtime.GOOS)

	if isRemSameArch {
		localContent, err := os.ReadFile(config.AgySyncScript)
		if err == nil {
			h := sha256.Sum256(localContent)
			locHash := hex.EncodeToString(h[:])
			var remHash string
			if probeCache != nil && probeCache[remBin] != nil {
				remHash = probeCache[remBin].SHA256
			}
			if remHash == "" {
				pyProbe := fmt.Sprintf(`python3 -c "import os, hashlib; p = '%s'; print(hashlib.sha256(open(p, 'rb').read()).hexdigest() if os.path.isfile(p) else 'NONE')" 2>/dev/null || echo NONE`, remBin)
				stdout, _, _, _ := ssh.RunSSHCmd(host, port, pyProbe, false)
				remHash = strings.TrimSpace(stdout)
			}
			if remHash != "" && remHash != "NONE" && locHash == remHash {
				return "identical"
			}
		}
	}

	// 4. If source was identical or pushed (local was newer), resolve target binary content
	targetContent, err := GetOrBuildTargetBinary(normRemOS, remArch)
	if err != nil || len(targetContent) == 0 {
		// Local has no Go compiler to cross-compile: ask remote to build for itself if remote has Go!
		remGoDir := fmt.Sprintf("%s/agy-sync-go", remHome)
		remBuildCmd := fmt.Sprintf(`%s if command -v go >/dev/null 2>&1 && [ -d '%s' ]; then (cd '%s' && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X agy-sync/pkg/config.GitCommit=%s" -o '%s' ./cmd/agy-sync 2>/dev/null && echo BUILT || echo FAILED); else echo NO_GO; fi`, config.PathPrefix, remGoDir, remGoDir, config.GitCommit, remBin)
		stdout, _, _, _ := ssh.RunSSHCmd(host, port, remBuildCmd, false)
		if strings.Contains(stdout, "BUILT") {
			return "remote compiled locally"
		}
		return "skipped (cross-arch build requires Go compiler)"
	}

	// 5. Push binary to remote with in-memory gzip stream (70% network transfer reduction)
	remParent := filepath.Dir(remBin)
	tmpRem := fmt.Sprintf("%s.tmp_%d_%s", remBin, time.Now().UnixNano(), randomToken(3))
	remTargetMTime := fi.ModTime().Unix() + int64(clockSkew)

	var streamContent []byte
	unpackCmd := fmt.Sprintf("cat > '%s'", tmpRem)
	var gzBuf bytes.Buffer
	gzWriter, gzErr := gzip.NewWriterLevel(&gzBuf, gzip.BestCompression)
	if gzErr == nil {
		_, _ = gzWriter.Write(targetContent)
		_ = gzWriter.Close()
		streamContent = gzBuf.Bytes()
		unpackCmd = fmt.Sprintf("(gzip -dc 2>/dev/null || cat) > '%s'", tmpRem)
	} else {
		streamContent = targetContent
	}

	pushCmd := fmt.Sprintf(`mkdir -p '%s' && (`+
		`%s && chmod 755 '%s' && `+
		`(touch -d @%d '%s' 2>/dev/null || true) && `+
		`mv -f '%s' '%s' && `+
		`(touch -d @%d '%s' 2>/dev/null || true) && `+
		`{ mkdir -p '%s/.local/bin' 2>/dev/null && ln -sf '%s' '%s/.local/bin/agy-sync' 2>/dev/null || true; } && `+
		`{ if [ -w /usr/local/bin ] || [ "$(id -u)" -eq 0 ]; then ln -sf '%s' /usr/local/bin/agy-sync 2>/dev/null || true; `+
		`elif command -v sudo >/dev/null 2>&1; then sudo -n ln -sf '%s' /usr/local/bin/agy-sync 2>/dev/null || true; `+
		`elif [ -d /data/data/com.termux/files/usr/bin ]; then ln -sf '%s' /data/data/com.termux/files/usr/bin/agy-sync 2>/dev/null || true; fi; }`+
		`) || (rm -f '%s' 2>/dev/null; false)`,
		remParent,
		unpackCmd, tmpRem,
		remTargetMTime, tmpRem,
		tmpRem, remBin,
		remTargetMTime, remBin,
		remHome, remBin, remHome,
		remBin, remBin, remBin,
		tmpRem,
	)

	sshArgs := ssh.GetSSHCmd(host, port, pushCmd, true)
	proc := exec.Command("ssh", sshArgs...)
	proc.Stdin = bytes.NewReader(streamContent)
	if err := proc.Run(); err == nil {
		if len(tracker) > 0 && tracker[0] != nil {
			tracker[0].AddPush(int64(len(streamContent)))
		}
		return "pushed"
	}
	return "failed"
}
