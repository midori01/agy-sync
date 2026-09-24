package sync

import (
	"bytes"
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

// GetOrBuildTargetBinary provides the correct binary bytes for a target architecture.
// If targetArch matches runtime.GOARCH, it returns the local installed universal binary directly (0 Go compiler needed!).
// Otherwise, it checks cache in ~/agy-sync-go/bin or cross-compiles JIT if a local Go compiler is available.
func GetOrBuildTargetBinary(targetArch string) ([]byte, error) {
	normArch := "arm64"
	if strings.Contains(targetArch, "x86_64") || strings.Contains(targetArch, "amd64") {
		normArch = "amd64"
	}

	// 1. Same-architecture fast path: returns the running/installed binary directly.
	// Does not require Go compiler on the local machine!
	if normArch == runtime.GOARCH {
		if data, err := os.ReadFile(config.AgySyncScript); err == nil && len(data) > 0 {
			return data, nil
		}
	}

	// 2. Check if a pre-compiled or previously cached cross-arch binary exists
	cachedBin := filepath.Join(config.AgySyncGoDir, "bin", fmt.Sprintf("agy-sync-linux-%s", normArch))
	stampFile := cachedBin + ".commit"

	if stamp, err := os.ReadFile(stampFile); err == nil && strings.TrimSpace(string(stamp)) == config.GitCommit && config.GitCommit != "dev" {
		if data, err := os.ReadFile(cachedBin); err == nil && len(data) > 0 {
			return data, nil
		}
	}

	// 3. JIT cross-compilation: requires local Go toolchain
	if _, err := exec.LookPath("go"); err != nil {
		return nil, fmt.Errorf("local Go compiler not available to cross-compile for %s", normArch)
	}

	_ = os.MkdirAll(filepath.Join(config.AgySyncGoDir, "bin"), 0755)
	ldflags := fmt.Sprintf("-s -w -X agy-sync/pkg/config.GitCommit=%s", config.GitCommit)
	buildCmd := exec.Command("go", "build", "-trimpath", fmt.Sprintf("-ldflags=%s", ldflags), "-o", cachedBin, "./cmd/agy-sync")
	buildCmd.Dir = config.AgySyncGoDir
	buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", fmt.Sprintf("GOARCH=%s", normArch))
	if out, err := buildCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("JIT build for %s failed: %v (%s)", normArch, err, strings.TrimSpace(string(out)))
	}

	_ = os.WriteFile(stampFile, []byte(config.GitCommit), 0644)
	return os.ReadFile(cachedBin)
}

// PullRemoteBinaryForLocal retrieves the matching native binary from the remote node
// when the local machine lacks a Go compilation environment.
func PullRemoteBinaryForLocal(host string, port int, remHome string, remArch string, remCommit string) ([]byte, error) {
	remBin := config.AgySyncScript
	if remHome != "" {
		remBin = config.ToRemotePath(config.AgySyncScript, remHome)
	}

	normLocalArch := runtime.GOARCH
	normRemArch := "arm64"
	if strings.Contains(remArch, "x86_64") || strings.Contains(remArch, "amd64") {
		normRemArch = "amd64"
	}

	// 1. Same-architecture: remote's installed binary is already built for local's arch!
	if normLocalArch == normRemArch {
		stdout, _, _, err := ssh.RunSSHCmd(host, port, fmt.Sprintf("cat '%s' 2>/dev/null", remBin), false)
		if err == nil && len(stdout) >= 4 && bytes.Equal([]byte(stdout[:4]), []byte{0x7f, 'E', 'L', 'F'}) {
			return []byte(stdout), nil
		}
	}

	// 2. Cross-architecture: request remote node (which has Go or cache) to supply local's binary
	remGoDir := fmt.Sprintf("%s/agy-sync-go", remHome)
	pullCmd := fmt.Sprintf(`
cached="%s/bin/agy-sync-linux-%s"
if [ -f "$cached" ] && [ -f "$cached.commit" ] && [ "$(cat "$cached.commit" 2>/dev/null)" = "%s" ]; then
    cat "$cached"
elif command -v go >/dev/null 2>&1 && [ -d "%s" ]; then
    mkdir -p "%s/bin" && \
    (cd "%s" && CGO_ENABLED=0 GOOS=linux GOARCH=%s go build -trimpath -ldflags="-s -w -X agy-sync/pkg/config.GitCommit=%s" -o "bin/agy-sync-linux-%s" ./cmd/agy-sync 2>/dev/null) && \
    echo "%s" > "%s/bin/agy-sync-linux-%s.commit" && \
    cat "%s/bin/agy-sync-linux-%s"
fi`, remGoDir, normLocalArch, remCommit, remGoDir, remGoDir, remGoDir, normLocalArch, remCommit, normLocalArch, remCommit, remGoDir, normLocalArch, remGoDir, normLocalArch)

	stdout, _, _, err := ssh.RunSSHCmd(host, port, pullCmd, false)
	if err == nil && len(stdout) >= 4 && bytes.Equal([]byte(stdout[:4]), []byte{0x7f, 'E', 'L', 'F'}) {
		return []byte(stdout), nil
	}

	return nil, fmt.Errorf("remote %s cannot supply %s binary", host, normLocalArch)
}

func SyncEngine(host string, port int, remHome string, remArch string, remCommit string, goSrcStat string, clockSkew float64, probeCache map[string]*RemoteFileInfo) string {
	fi, err := os.Stat(config.AgySyncScript)
	if err != nil || fi.IsDir() {
		return "none"
	}

	remBin := config.AgySyncScript
	if remHome != "" {
		remBin = config.ToRemotePath(config.AgySyncScript, remHome)
	}

	// 1. Conflict and Dirty Protection: Never overwrite if source diverged or remote has uncommitted work
	if strings.Contains(goSrcStat, "dirty") || strings.Contains(goSrcStat, "conflict") {
		return "safety guard (source diverged)"
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
				buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", fmt.Sprintf("GOARCH=%s", runtime.GOARCH))
				if err := buildCmd.Run(); err == nil {
					_ = os.Rename(tmpLocal, config.AgySyncScript)
					_ = os.Chmod(config.AgySyncScript, 0755)
					config.GitCommit = newCommit
					return "pulled & rebuilt locally"
				}
				_ = os.Remove(tmpLocal)
			}
		}

		// B. Fallback for devices WITHOUT Go compiler: pull ready binary from remote!
		if binData, err := PullRemoteBinaryForLocal(host, port, remHome, remArch, remCommit); err == nil && len(binData) > 0 {
			tmpLocal := fmt.Sprintf("%s.tmp_%d_%s", config.AgySyncScript, time.Now().UnixNano(), randomToken(3))
			if err := os.WriteFile(tmpLocal, binData, 0755); err == nil {
				_ = os.Rename(tmpLocal, config.AgySyncScript)
				config.GitCommit = remCommit
				return "pulled binary from peer"
			}
		}

		return "pulled"
	}

	// 3. 1-RTT Commit Match: If binary commits match, 100% identical across any architecture
	if remCommit != "" && remCommit != "NONE" && remCommit != "dev" && config.GitCommit != "dev" && remCommit == config.GitCommit {
		return "identical"
	}

	// 4. Same-Architecture Direct Byte Check (Fallback for dev or unversioned binaries)
	isRemSameArch := (strings.Contains(remArch, "x86_64") || strings.Contains(remArch, "amd64")) == (runtime.GOARCH == "amd64")

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

	// 5. If source was identical or pushed (local was newer), resolve target binary content
	targetContent, err := GetOrBuildTargetBinary(remArch)
	if err != nil || len(targetContent) == 0 {
		// Local has no Go compiler to cross-compile: ask remote to build for itself if remote has Go!
		remGoDir := fmt.Sprintf("%s/agy-sync-go", remHome)
		remBuildCmd := fmt.Sprintf(`%s if command -v go >/dev/null 2>&1 && [ -d '%s' ]; then (cd '%s' && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X agy-sync/pkg/config.GitCommit=%s" -o '%s' ./cmd/agy-sync 2>/dev/null && echo BUILT || echo FAILED); else echo NO_GO; fi`, config.PathPrefix, remGoDir, remGoDir, config.GitCommit, remBin)
		stdout, _, _, _ := ssh.RunSSHCmd(host, port, remBuildCmd, false)
		if strings.Contains(stdout, "BUILT") {
			return "remote compiled locally"
		}
		return "skipped (cross-arch build requires Go compiler)"
	}

	// 6. Push binary to remote
	remParent := filepath.Dir(remBin)
	tmpRem := fmt.Sprintf("%s.tmp_%d_%s", remBin, time.Now().UnixNano(), randomToken(3))
	remTargetMTime := fi.ModTime().Unix() + int64(clockSkew)

	pushCmd := fmt.Sprintf(`mkdir -p '%s' && cat > '%s' && chmod 755 '%s' && `+
		`(touch -d @%d '%s' 2>/dev/null || true) && `+
		`mv -f '%s' '%s' && `+
		`(touch -d @%d '%s' 2>/dev/null || true) && `+
		`mkdir -p '%s/.local/bin' 2>/dev/null && ln -sf '%s' '%s/.local/bin/agy-sync' 2>/dev/null || true; `+
		`if [ -w /usr/local/bin ] || [ "$(id -u)" -eq 0 ]; then ln -sf '%s' /usr/local/bin/agy-sync 2>/dev/null || true; `+
		`elif command -v sudo >/dev/null 2>&1; then sudo -n ln -sf '%s' /usr/local/bin/agy-sync 2>/dev/null || true; `+
		`elif [ -d /data/data/com.termux/files/usr/bin ]; then ln -sf '%s' /data/data/com.termux/files/usr/bin/agy-sync 2>/dev/null || true; fi`,
		remParent, tmpRem, tmpRem,
		remTargetMTime, tmpRem,
		tmpRem, remBin,
		remTargetMTime, remBin,
		remHome, remBin, remHome,
		remBin, remBin, remBin,
	)

	sshArgs := ssh.GetSSHCmd(host, port, pushCmd, true)
	proc := exec.Command("ssh", sshArgs...)
	proc.Stdin = bytes.NewReader(targetContent)
	if err := proc.Run(); err == nil {
		return "pushed"
	}
	return "failed"
}
