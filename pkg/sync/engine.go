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
// If targetArch matches runtime.GOARCH, it returns the local installed universal binary.
// Otherwise, it compiles JIT into ~/agy-sync-go/bin cache.
func GetOrBuildTargetBinary(targetArch string) ([]byte, error) {
	normArch := "arm64"
	if strings.Contains(targetArch, "x86_64") || strings.Contains(targetArch, "amd64") {
		normArch = "amd64"
	}

	if normArch == runtime.GOARCH {
		if data, err := os.ReadFile(config.AgySyncScript); err == nil && len(data) > 0 {
			return data, nil
		}
	}

	cachedBin := filepath.Join(config.AgySyncGoDir, "bin", fmt.Sprintf("agy-sync-linux-%s", normArch))
	stampFile := cachedBin + ".commit"

	if stamp, err := os.ReadFile(stampFile); err == nil && strings.TrimSpace(string(stamp)) == config.GitCommit && config.GitCommit != "dev" {
		if data, err := os.ReadFile(cachedBin); err == nil && len(data) > 0 {
			return data, nil
		}
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
	// Instead, local compiles the newly pulled source code for ITS OWN architecture.
	if goSrcStat == "pulled" || goSrcStat == "pulled & ready" {
		out, err := exec.Command("git", "-C", config.AgySyncGoDir, "rev-parse", "--short", "HEAD").Output()
		if err == nil {
			newCommit := strings.TrimSpace(string(out))
			ldflags := fmt.Sprintf("-s -w -X agy-sync/pkg/config.GitCommit=%s", newCommit)
			buildCmd := exec.Command("go", "build", "-trimpath", fmt.Sprintf("-ldflags=%s", ldflags), "-o", config.AgySyncScript, "./cmd/agy-sync")
			buildCmd.Dir = config.AgySyncGoDir
			buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", fmt.Sprintf("GOARCH=%s", runtime.GOARCH))
			if err := buildCmd.Run(); err == nil {
				config.GitCommit = newCommit
				return "pulled & rebuilt locally"
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
		return "none"
	}

	// 6. Push binary to remote
	remParent := filepath.Dir(remBin)
	tmpRem := fmt.Sprintf("%s.tmp_%d_%s", remBin, time.Now().Unix(), randomToken(3))
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
