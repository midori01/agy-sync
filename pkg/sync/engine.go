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
	"strconv"
	"strings"
	"time"

	"agy-sync/pkg/config"
	"agy-sync/pkg/ssh"
)

func SyncEngine(host string, port int, remHome string, remArch string, clockSkew float64, probeCache map[string]*RemoteFileInfo) string {
	fi, err := os.Stat(config.AgySyncScript)
	if err != nil || fi.IsDir() {
		return "none"
	}

	remBin := config.AgySyncScript
	if remHome != "" {
		remBin = config.ToRemotePath(config.AgySyncScript, remHome)
	}

	localContent, err := os.ReadFile(config.AgySyncScript)
	if err != nil {
		return "none"
	}

	locBody, locHash := GetScriptBodyAndHash(localContent)
	locMTime := fi.ModTime().Unix()

	remIsTermux := strings.Contains(remHome, "/data/data/com.termux")
	localIsTermux := false
	if fi, err := os.Stat("/data/data/com.termux"); err == nil && fi.IsDir() {
		localIsTermux = true
	}
	isELF := len(localContent) >= 4 && bytes.Equal(localContent[:4], []byte{0x7f, 'E', 'L', 'F'})

	var targetContent []byte
	var targetHash string
	binDir := filepath.Dir(config.AgySyncScript)

	if isELF {
		if remIsTermux {
			androidBin := filepath.Join(binDir, "agy-sync-android-arm64")
			if data, err := os.ReadFile(androidBin); err == nil {
				targetContent = data
			} else if localIsTermux {
				targetContent = localContent
			} else {
				return "none" // Safety: Linux node without android binary must never push linux binary as android
			}
		} else {
			// Remote is Linux VPS: match architecture dynamically (amd64 / x86_64 vs arm64 / aarch64)
			archLower := strings.ToLower(remArch)
			if strings.Contains(archLower, "x86_64") || strings.Contains(archLower, "amd64") {
				linuxAmdBin := filepath.Join(binDir, "agy-sync-linux-amd64")
				if data, err := os.ReadFile(linuxAmdBin); err == nil {
					targetContent = data
				} else if !localIsTermux && runtime.GOARCH == "amd64" {
					targetContent = localContent
				} else {
					return "none" // Safety: missing AMD64 binary
				}
			} else {
				linuxArmBin := filepath.Join(binDir, "agy-sync-linux-arm64")
				if data, err := os.ReadFile(linuxArmBin); err == nil {
					targetContent = data
				} else if !localIsTermux && runtime.GOARCH == "arm64" {
					targetContent = localContent
				} else {
					return "none" // Safety: missing ARM64 binary
				}
			}
		}
		h := sha256.Sum256(targetContent)
		targetHash = hex.EncodeToString(h[:])
	} else {
		shebang := []byte("#!/usr/bin/env python3\n")
		if remIsTermux {
			shebang = []byte("#!/data/data/com.termux/files/usr/bin/env python3\n")
		}
		targetContent = append(shebang, locBody...)
		targetHash = locHash
	}

	remoteExists := false
	var remMTime int64
	var remHash string

	if probeCache != nil && probeCache[remBin] != nil {
		cached := probeCache[remBin]
		remoteExists = true
		remMTime = cached.MTime - int64(clockSkew)
		remHash = cached.BodySHA
		if remHash == "" {
			remHash = cached.SHA256
		}
	}

	if remHash == "" && (probeCache == nil || probeCache[remBin] != nil || remoteExists) {
		pyProbe := fmt.Sprintf(`python3 -c "
import os, hashlib
p = '%s'
if os.path.isfile(p):
    st = os.stat(p)
    c = open(p, 'rb').read()
    b = c.split(bytes([10]), 1)[1] if c.startswith(b'#!') and bytes([10]) in c else c
    print(f'{int(st.st_mtime)} {st.st_size} {hashlib.sha256(b).hexdigest()}')
else:
    print('NONE')
" 2>/dev/null || echo NONE`, remBin)

		stdout, _, _, _ := ssh.RunSSHCmd(host, port, pyProbe, false)
		stdout = strings.TrimSpace(stdout)
		if stdout != "NONE" && stdout != "" {
			parts := strings.Fields(stdout)
			if len(parts) >= 3 {
				remoteExists = true
				if m, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
					remMTime = m - int64(clockSkew)
				}
				remHash = parts[2]
			}
		}
	}

	if remoteExists && targetHash != "" && remHash != "" && targetHash == remHash {
		EnsureCompanionBinaries(host, port, binDir, filepath.Dir(remBin))
		return "identical"
	}

	// Push local to remote
	if !remoteExists || locMTime >= remMTime {
		remParent := filepath.Dir(remBin)
		tmpRem := fmt.Sprintf("%s.tmp_%d_%s", remBin, time.Now().Unix(), randomToken(3))
		remTargetMTime := locMTime + int64(clockSkew)

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
			EnsureCompanionBinaries(host, port, binDir, remParent)
			return "pushed"
		}
		return "failed"
	} else {
		// Remote is newer: check architecture compatibility
		if isELF && localIsTermux != remIsTermux {
			targetName := "agy-sync-linux-arm64"
			if localIsTermux {
				targetName = "agy-sync-android-arm64"
			} else if runtime.GOARCH == "amd64" {
				targetName = "agy-sync-linux-amd64"
			}
			remTargetBin := filepath.Join(filepath.Dir(remBin), targetName)

			stdout, _, _, err := ssh.RunSSHCmd(host, port, fmt.Sprintf("cat '%s' 2>/dev/null", remTargetBin), false)
			if err != nil || len(stdout) < 4 || !bytes.Equal([]byte(stdout[:4]), []byte{0x7f, 'E', 'L', 'F'}) {
				return "none"
			}
			tmpLocal := fmt.Sprintf("%s.tmp_%d_%s", config.AgySyncScript, time.Now().Unix(), randomToken(3))
			if err := os.WriteFile(tmpLocal, []byte(stdout), 0755); err == nil {
				_ = os.Rename(tmpLocal, config.AgySyncScript)
				PullCompanionBinaries(host, port, binDir, filepath.Dir(remBin))
				return "pulled"
			}
			return "failed"
		}

		stdout, _, _, err := ssh.RunSSHCmd(host, port, fmt.Sprintf("cat '%s'", remBin), false)
		if err != nil || stdout == "" {
			return "failed"
		}
		remBytes := []byte(stdout)
		remBody, _ := GetScriptBodyAndHash(remBytes)

		isRemoteELF := len(remBytes) >= 4 && bytes.Equal(remBytes[:4], []byte{0x7f, 'E', 'L', 'F'})
		var newContent []byte
		if isRemoteELF {
			newContent = remBytes
		} else {
			shebang := []byte("#!/usr/bin/env python3\n")
			if localIsTermux {
				shebang = []byte("#!/data/data/com.termux/files/usr/bin/env python3\n")
			}
			newContent = append(shebang, remBody...)
		}

		tmpLocal := fmt.Sprintf("%s.tmp_%d_%s", config.AgySyncScript, time.Now().Unix(), randomToken(3))
		if err := os.WriteFile(tmpLocal, newContent, 0755); err == nil {
			_ = os.Rename(tmpLocal, config.AgySyncScript)
			PullCompanionBinaries(host, port, binDir, filepath.Dir(remBin))
			return "pulled"
		}
		return "failed"
	}
}

func PullCompanionBinaries(host string, port int, localBinDir, remBinDir string) {
	names := []string{"agy-sync-android-arm64", "agy-sync-linux-arm64", "agy-sync-linux-amd64"}
	var checkPaths []string
	for _, name := range names {
		remPath := filepath.Join(remBinDir, name)
		checkPaths = append(checkPaths, fmt.Sprintf("'%s'", remPath))
	}

	checkCmd := fmt.Sprintf("sha256sum %s 2>/dev/null || true", strings.Join(checkPaths, " "))
	remOut, _, _, _ := ssh.RunSSHCmd(host, port, checkCmd, false)
	remHashes := make(map[string]string)
	for _, line := range strings.Split(remOut, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			remHashes[filepath.Base(fields[1])] = fields[0]
		}
	}

	for _, name := range names {
		remHash, ok := remHashes[name]
		if !ok || remHash == "" {
			continue
		}
		locPath := filepath.Join(localBinDir, name)
		if data, err := os.ReadFile(locPath); err == nil {
			h := sha256.Sum256(data)
			if hex.EncodeToString(h[:]) == remHash {
				continue // Already matching
			}
		}
		remPath := filepath.Join(remBinDir, name)
		stdout, _, _, err := ssh.RunSSHCmd(host, port, fmt.Sprintf("cat '%s' 2>/dev/null", remPath), false)
		if err == nil && len(stdout) >= 4 && bytes.Equal([]byte(stdout[:4]), []byte{0x7f, 'E', 'L', 'F'}) {
			tmpFile := fmt.Sprintf("%s.tmp_%d_%s", locPath, time.Now().Unix(), randomToken(3))
			if err := os.WriteFile(tmpFile, []byte(stdout), 0755); err == nil {
				_ = os.Rename(tmpFile, locPath)
			}
		}
	}
}

func EnsureCompanionBinaries(host string, port int, localBinDir, remBinDir string) {
	names := []string{"agy-sync-android-arm64", "agy-sync-linux-arm64", "agy-sync-linux-amd64"}
	type compInfo struct {
		name    string
		data    []byte
		locHash string
		remPath string
	}
	var toCheck []compInfo
	var checkPaths []string
	for _, name := range names {
		locPath := filepath.Join(localBinDir, name)
		data, err := os.ReadFile(locPath)
		if err != nil {
			continue
		}
		h := sha256.Sum256(data)
		remPath := filepath.Join(remBinDir, name)
		toCheck = append(toCheck, compInfo{
			name:    name,
			data:    data,
			locHash: hex.EncodeToString(h[:]),
			remPath: remPath,
		})
		checkPaths = append(checkPaths, fmt.Sprintf("'%s'", remPath))
	}
	if len(toCheck) == 0 {
		return
	}

	checkCmd := fmt.Sprintf("sha256sum %s 2>/dev/null || true", strings.Join(checkPaths, " "))
	remOut, _, _, _ := ssh.RunSSHCmd(host, port, checkCmd, false)
	remHashes := make(map[string]string)
	for _, line := range strings.Split(remOut, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			remHashes[filepath.Base(fields[1])] = fields[0]
		}
	}

	for _, item := range toCheck {
		if remHashes[item.name] == item.locHash {
			continue // Already up to date
		}

		pushCmd := fmt.Sprintf("cat > '%s' && chmod 755 '%s'", item.remPath, item.remPath)
		args := ssh.GetSSHCmd(host, port, pushCmd, true)
		p := exec.Command("ssh", args...)
		p.Stdin = bytes.NewReader(item.data)
		_ = p.Run()
	}
}
