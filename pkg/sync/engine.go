package sync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agy-sync/pkg/config"
	"agy-sync/pkg/ssh"
)

func SyncEngine(host string, port int, remHome string, clockSkew float64, probeCache map[string]*RemoteFileInfo) string {
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
	isELF := len(localContent) >= 4 && bytes.Equal(localContent[:4], []byte{0x7f, 'E', 'L', 'F'})

	var targetContent []byte
	var targetHash string
	binDir := filepath.Dir(config.AgySyncScript)

	if isELF {
		if remIsTermux {
			androidBin := filepath.Join(binDir, "agy-sync-android-arm64")
			if data, err := os.ReadFile(androidBin); err == nil {
				targetContent = data
			} else {
				targetContent = localContent
			}
		} else {
			// Remote is Linux VPS (Ubuntu aarch64 on kix1/sjc1)
			linuxArmBin := filepath.Join(binDir, "agy-sync-linux-arm64")
			if data, err := os.ReadFile(linuxArmBin); err == nil {
				targetContent = data
			} else {
				targetContent = localContent
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
		return "identical"
	}

	localIsTermux := false
	if fi, err := os.Stat("/data/data/com.termux"); err == nil && fi.IsDir() {
		localIsTermux = true
	}

	// Push local to remote
	if !remoteExists || locMTime >= remMTime || (isELF && localIsTermux != remIsTermux) {
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
			return "pushed"
		}
		return "failed"
	} else {
		// Remote is newer: check architecture compatibility
		if isELF && localIsTermux != remIsTermux {
			return "none"
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
			return "pulled"
		}
		return "failed"
	}
}
