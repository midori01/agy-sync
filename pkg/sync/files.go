package sync

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"agy-sync/pkg/cluster"
	"agy-sync/pkg/config"
	"agy-sync/pkg/ssh"
	"agy-sync/pkg/terminal"
)

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func SyncSingleFile(localPath, remotePath, host string, port int, name string, clockSkew float64, probeCache map[string]*RemoteFileInfo, tracker *ByteTracker, mode ...string) string {
	fi, err := os.Stat(localPath)
	localExists := (err == nil && !fi.IsDir())

	fileMode := ""
	if len(mode) > 0 && mode[0] != "" {
		fileMode = mode[0]
	} else {
		if strings.HasSuffix(localPath, "settings.json") || strings.HasSuffix(localPath, "antigravity-oauth-token") || strings.HasSuffix(localPath, "agy-pool-accounts.json") ||
			name == "settings.json" || name == "antigravity-oauth-token" || name == "agy-pool-accounts.json" {
			fileMode = "600"
		}
	}

	var remoteMTime, remoteSize int64
	var remoteHash string
	remoteExists := false

	if probeCache != nil && probeCache[remotePath] != nil {
		cached := probeCache[remotePath]
		remoteExists = true
		remoteMTime = cached.MTime
		remoteSize = cached.Size
		remoteHash = cached.SHA256
	} else if probeCache != nil && probeCache[remotePath] == nil {
		remoteExists = false
	} else {
		probeCmd := fmt.Sprintf(`f="%s"; if test -f "$f"; then if stat -c "%%Y %%s" "$f" >/dev/null 2>&1; then st="$(stat -c "%%Y %%s" "$f")"; else st="$(stat -f "%%m %%z" "$f" 2>/dev/null)"; fi; if command -v sha256sum >/dev/null 2>&1; then h="$(sha256sum "$f" | awk '{print $1}')"; else h="$(shasum -a 256 "$f" 2>/dev/null | awk '{print $1}')"; fi; echo "$st $h"; else echo "NONE"; fi`, remotePath)
		stdout, _, _, _ := ssh.RunSSHCmd(host, port, probeCmd, false)
		stdout = strings.TrimSpace(stdout)
		if stdout != "NONE" && stdout != "" {
			parts := strings.Fields(stdout)
			if len(parts) >= 3 {
				remoteExists = true
				remoteMTime, _ = strconv.ParseInt(parts[0], 10, 64)
				remoteSize, _ = strconv.ParseInt(parts[1], 10, 64)
				remoteHash = parts[2]
			}
		}
	}

	if !localExists && !remoteExists {
		return "none"
	}

	var localMTime int64
	var localHash string
	if localExists {
		localMTime = fi.ModTime().Unix()
		localHash = GetFileHash(localPath)
	}

	if localHash != "" && remoteHash != "" && localHash == remoteHash {
		return "identical"
	}

	normalizedRemoteMTime := remoteMTime - int64(clockSkew)
	// Guard against pushing empty 0-byte file over existing non-empty remote file
	if localExists && fi.Size() == 0 && remoteExists && remoteSize > 0 {
		// Remote has data while local is empty: force pull to prevent data loss
		normalizedRemoteMTime = localMTime + 9999
	} else if localExists && fi.Size() > 0 && remoteExists && remoteSize == 0 {
		// Local has data while remote is empty: force push to prevent data loss
		normalizedRemoteMTime = localMTime - 9999
	}

	if localExists && (!remoteExists || localMTime >= normalizedRemoteMTime) {
		remoteParent := filepath.Dir(remotePath)
		tmpRemote := fmt.Sprintf("%s.tmp_push_%d_%s", remotePath, time.Now().UnixNano(), randomToken(3))
		remTargetMTime := localMTime + int64(clockSkew)

		chmodClause := ""
		if fileMode != "" {
			chmodClause = fmt.Sprintf("(chmod %s '%s' 2>/dev/null || true) && ", fileMode, tmpRemote)
		}

		pushCmd := fmt.Sprintf(
			"mkdir -p '%s' && (cat > '%s' && %s(touch -d @%d '%s' 2>/dev/null || true) && mv -f '%s' '%s' && (touch -d @%d '%s' 2>/dev/null || true) || (rm -f '%s' 2>/dev/null; false))",
			remoteParent, tmpRemote, chmodClause, remTargetMTime, tmpRemote, tmpRemote, remotePath, remTargetMTime, remotePath, tmpRemote,
		)

		sshArgs := ssh.GetSSHCmd(host, port, pushCmd, true)
		proc := exec.Command("ssh", sshArgs...)
		data, err := os.ReadFile(localPath)
		if err != nil {
			return "failed"
		}
		proc.Stdin = bytes.NewReader(data)
		if err := proc.Run(); err == nil {
			if tracker != nil {
				tracker.AddPush(int64(len(data)))
			}
			return "pushed"
		}
		return "failed"
	} else if remoteExists {
		_ = os.MkdirAll(filepath.Dir(localPath), 0755)
		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("cat '%s'", remotePath), true)
		proc := exec.Command("ssh", sshArgs...)
		out, err := proc.Output()
		if err == nil && (len(out) > 0 || remoteSize == 0) {
			modeVal := os.FileMode(0644)
			if fileMode != "" {
				if parsedMode, err := strconv.ParseUint(fileMode, 8, 32); err == nil {
					modeVal = os.FileMode(parsedMode)
				}
			}
			if err := config.WriteFileAtomic(localPath, out, modeVal); err == nil {
				if tracker != nil {
					tracker.AddPull(int64(len(out)))
				}
				if normalizedRemoteMTime > 0 {
					mtime := time.Unix(normalizedRemoteMTime, 0)
					_ = os.Chtimes(localPath, mtime, mtime)
				}
				return "pulled"
			}
		}
		return "failed"
	}

	return "unchanged"
}

func SyncAuthorizedKeys(host string, port int, remHome string, probeCache map[string]*RemoteFileInfo, tracker ...*ByteTracker) string {
	localLines := make(map[string]bool)
	if data, err := os.ReadFile(config.AuthKeysFile); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				localLines[trimmed] = true
			}
		}
	}

	remAuth := config.AuthKeysFile
	if remHome != "" {
		remAuth = config.ToRemotePath(config.AuthKeysFile, remHome)
	}

	// Pre-check: SHA-256 short circuit
	if _, err := os.Stat(config.AuthKeysFile); err == nil && probeCache != nil && probeCache[remAuth] != nil {
		cached := probeCache[remAuth]
		if cached.SHA256 != "" {
			locHash := GetFileHash(config.AuthKeysFile)
			if locHash != "" && locHash == cached.SHA256 {
				return fmt.Sprintf("%d active key(s)", len(localLines))
			}
		}
	}

	stdout, _, _, err := ssh.RunSSHCmd(host, port, fmt.Sprintf("cat '%s' 2>/dev/null", remAuth), false)
	remoteLines := make(map[string]bool)
	if err == nil && stdout != "" {
		for _, line := range strings.Split(stdout, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				remoteLines[trimmed] = true
			}
		}
	}

	merged := make(map[string]bool)
	for k := range localLines {
		merged[k] = true
	}
	for k := range remoteLines {
		merged[k] = true
	}

	localChanged := (len(merged) != len(localLines))
	remoteChanged := (len(merged) != len(remoteLines))

	var sortedKeys []string
	for k := range merged {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)
	content := strings.Join(sortedKeys, "\n") + "\n"

	if localChanged {
		_ = config.WriteFileAtomic(config.AuthKeysFile, []byte(content), 0600)
		if len(tracker) > 0 && tracker[0] != nil {
			tracker[0].AddPull(int64(len(content)))
		}
	}

	if remoteChanged {
		remDir := filepath.Dir(remAuth)
		remTmp := fmt.Sprintf("%s.tmp_%d_%s", remAuth, time.Now().UnixNano(), randomToken(3))
		cmd := fmt.Sprintf("mkdir -p '%s' && (cat > '%s' && chmod 600 '%s' && mv -f '%s' '%s' || (rm -f '%s' 2>/dev/null; false))", remDir, remTmp, remTmp, remTmp, remAuth, remTmp)
		sshArgs := ssh.GetSSHCmd(host, port, cmd, true)
		proc := exec.Command("ssh", sshArgs...)
		proc.Stdin = strings.NewReader(content)
		if err := proc.Run(); err != nil {
			return "failed to update remote keys"
		}
		if len(tracker) > 0 && tracker[0] != nil {
			tracker[0].AddPush(int64(len(content)))
		}
	}

	return fmt.Sprintf("%d active key(s)", len(merged))
}

func SyncClusterRegistry(host string, port int, clockSkew float64, remHome string, probeCache map[string]*RemoteFileInfo, tracker ...*ByteTracker) string {
	localReg := config.LoadRegistry()
	remDev := config.DevicesJSONFile
	remConf := config.ConfFile
	if remHome != "" {
		remDev = config.ToRemotePath(config.DevicesJSONFile, remHome)
		remConf = config.ToRemotePath(config.ConfFile, remHome)
	}

	if _, err := os.Stat(config.DevicesJSONFile); err == nil && probeCache != nil && probeCache[remDev] != nil {
		cached := probeCache[remDev]
		if cached.SHA256 != "" {
			locHash := GetFileHash(config.DevicesJSONFile)
			if locHash != "" && locHash == cached.SHA256 {
				activeCount := 0
				for _, d := range localReg {
					if !d.Deleted {
						activeCount++
					}
				}
				return fmt.Sprintf("synchronized (%d active nodes)", activeCount)
			}
		}
	}

	stdout, _, _, _ := ssh.RunSSHCmd(host, port, fmt.Sprintf("cat '%s' 2>/dev/null", remDev), false)
	remoteReg := make(config.Registry)
	if stdout != "" {
		_ = json.Unmarshal([]byte(stdout), &remoteReg)
	}

	if len(remoteReg) == 0 {
		outConf, _, _, _ := ssh.RunSSHCmd(host, port, fmt.Sprintf("cat '%s' 2>/dev/null", remConf), false)
		if outConf != "" {
			now := float64(time.Now().UnixNano())/1e9 - 100
			lines := strings.Split(outConf, "\n")
			inDevSec := false
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line == "[devices]" {
					inDevSec = true
					continue
				} else if strings.HasPrefix(line, "[") {
					inDevSec = false
				}
				if inDevSec && strings.Contains(line, "=") {
					parts := strings.SplitN(line, "=", 2)
					name := strings.ToLower(strings.TrimSpace(parts[0]))
					val := strings.TrimSpace(parts[1])
					ep := val
					if !strings.Contains(ep, ":") {
						ep = ep + ":8022"
					}
					remoteReg[ep] = &config.DeviceEntry{
						Alias:      name,
						NativeName: name,
						IdType:     "Host",
						Deleted:    false,
						UpdatedAt:  now,
					}
				}
			}
		}
	}

	merged, localChg, remoteChg := cluster.MergeRegistries(localReg, remoteReg, clockSkew)

	if localChg || len(localReg) == 0 {
		_ = config.SaveRegistry(merged)
		cluster.ReconcileRegistry()
		if len(tracker) > 0 && tracker[0] != nil && len(stdout) > 0 {
			tracker[0].AddPull(int64(len(stdout)))
		}
	}

	if remoteChg || len(remoteReg) == 0 {
		tmpReg := fmt.Sprintf("%s.tmp_%d_%s", remDev, time.Now().UnixNano(), randomToken(3))
		remDir := filepath.Dir(remDev)
		cmd := fmt.Sprintf("mkdir -p '%s' && (cat > '%s' && mv -f '%s' '%s' || (rm -f '%s' 2>/dev/null; false))", remDir, tmpReg, tmpReg, remDev, tmpReg)
		sshArgs := ssh.GetSSHCmd(host, port, cmd, true)
		proc := exec.Command("ssh", sshArgs...)
		data, _ := json.MarshalIndent(merged, "", "  ")
		proc.Stdin = strings.NewReader(string(data) + "\n")
		if err := proc.Run(); err != nil {
			return "failed to update remote registry"
		}
		if len(tracker) > 0 && tracker[0] != nil {
			tracker[0].AddPush(int64(len(data)))
		}
		_, _, _, _ = ssh.RunSSHCmd(host, port, fmt.Sprintf("%s agy-sync refresh-registry 2>/dev/null || true", config.PathPrefix), false)
	}

	activeCount := 0
	for _, d := range merged {
		if !d.Deleted {
			activeCount++
		}
	}
	return fmt.Sprintf("synchronized (%d active nodes)", activeCount)
}

func SyncHistoryJsonl(host string, port int, remHome string, probeCache map[string]*RemoteFileInfo, tracker ...*ByteTracker) string {
	remHist := config.HistoryFile
	if remHome != "" {
		remHist = config.ToRemotePath(config.HistoryFile, remHome)
	}
	localExists := false
	if _, err := os.Stat(config.HistoryFile); err == nil {
		localExists = true
	}

	if localExists && probeCache != nil && probeCache[remHist] != nil {
		cached := probeCache[remHist]
		locHash := GetFileHash(config.HistoryFile)
		if locHash != "" && cached.SHA256 != "" && locHash == cached.SHA256 {
			cnt := 0
			if data, err := os.ReadFile(config.HistoryFile); err == nil {
				cnt = len(strings.Split(strings.TrimSpace(string(data)), "\n"))
			}
			return fmt.Sprintf("identical (%d entries)", cnt)
		}
	}

	var localLines []string
	if localExists {
		if data, err := os.ReadFile(config.HistoryFile); err == nil {
			for _, l := range strings.Split(string(data), "\n") {
				if strings.TrimSpace(l) != "" {
					localLines = append(localLines, l)
				}
			}
		}
	}

	stdout, _, _, _ := ssh.RunSSHCmd(host, port, fmt.Sprintf("cat '%s' 2>/dev/null", remHist), false)
	var remoteLines []string
	for _, l := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(l) != "" {
			remoteLines = append(remoteLines, l)
		}
	}

	type histEntry struct {
		Timestamp int64
		Line      string
	}
	entries := make(map[string]histEntry)

	parseLine := func(line string) {
		lineS := strings.TrimSpace(line)
		if lineS == "" {
			return
		}
		var parsed map[string]interface{}
		var ts int64
		disp := lineS
		if err := json.Unmarshal([]byte(lineS), &parsed); err == nil {
			if v, ok := parsed["timestamp"]; ok {
				if f, ok := v.(float64); ok {
					ts = int64(f)
				}
			}
			if d, ok := parsed["display"]; ok {
				disp = fmt.Sprintf("%v", d)
			}
			var convID string
			if c, ok := parsed["conversationId"]; ok {
				convID = fmt.Sprintf("%v", c)
			}
			key := fmt.Sprintf("%d:%s:%s", ts, disp, convID)
			if ts == 0 && disp == lineS {
				key = lineS
			}
			entries[key] = histEntry{Timestamp: ts, Line: lineS}
		} else {
			entries[lineS] = histEntry{Timestamp: 0, Line: lineS}
		}
	}

	for _, l := range localLines {
		parseLine(l)
	}
	for _, l := range remoteLines {
		parseLine(l)
	}

	var list []histEntry
	for _, e := range entries {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].Timestamp < list[j].Timestamp
	})

	var mergedLines []string
	for _, e := range list {
		mergedLines = append(mergedLines, e.Line)
	}
	mergedContent := strings.Join(mergedLines, "\n") + "\n"

	localContent := strings.Join(localLines, "\n") + "\n"
	remoteContent := strings.Join(remoteLines, "\n") + "\n"
	localChanged := (mergedContent != localContent)
	remoteChanged := (mergedContent != remoteContent)

	if localChanged {
		_ = config.WriteFileAtomic(config.HistoryFile, []byte(mergedContent), 0644)
		if len(tracker) > 0 && tracker[0] != nil {
			tracker[0].AddPull(int64(len(mergedContent)))
		}
	}

	if remoteChanged {
		remDir := filepath.Dir(remHist)
		remTmp := fmt.Sprintf("%s.tmp_%d_%s", remHist, time.Now().UnixNano(), randomToken(3))
		cmd := fmt.Sprintf("mkdir -p '%s' && (cat > '%s' && mv -f '%s' '%s' || (rm -f '%s' 2>/dev/null; false))", remDir, remTmp, remTmp, remHist, remTmp)
		sshArgs := ssh.GetSSHCmd(host, port, cmd, true)
		proc := exec.Command("ssh", sshArgs...)
		proc.Stdin = strings.NewReader(mergedContent)
		if err := proc.Run(); err != nil {
			// Retry with clean socket to recover from stale connection or mobile sleep
			ssh.CloseControlMaster(host, port)
			time.Sleep(1200 * time.Millisecond)
			procRetry := exec.Command("ssh", ssh.GetSSHCmd(host, port, cmd, true)...)
			procRetry.Stdin = strings.NewReader(mergedContent)
			if errRetry := procRetry.Run(); errRetry != nil {
				ssh.CloseControlMaster(host, port)
				time.Sleep(2000 * time.Millisecond)
				procRetry2 := exec.Command("ssh", ssh.GetSSHCmd(host, port, cmd, true)...)
				procRetry2.Stdin = strings.NewReader(mergedContent)
				if errRetry2 := procRetry2.Run(); errRetry2 != nil {
					return "failed to update remote history"
				}
			}
		}
		if len(tracker) > 0 && tracker[0] != nil {
			tracker[0].AddPush(int64(len(mergedContent)))
		}
	}

	if !localChanged && !remoteChanged {
		return fmt.Sprintf("identical (%d entries)", len(list))
	}
	return fmt.Sprintf("%d entries merged", len(list))
}

func SyncConfigsAndSkills(host string, port int, clockSkew float64, remHome string, tracker *ByteTracker, preprobedCfg map[string][2]int64, onProgress ...func(string)) (int, int) {
	_ = os.MkdirAll(config.ConfigDir, 0755)
	localFiles := make(map[string][2]int64)

	reportProgress := func(msg string) {
		if len(onProgress) > 0 && onProgress[0] != nil {
			onProgress[0](msg)
		}
	}

	_ = filepath.Walk(config.ConfigDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			if info.Mode()&os.ModeSymlink != 0 {
				target, err := os.Readlink(path)
				if err == nil {
					targetAbs := target
					if !filepath.IsAbs(targetAbs) {
						targetAbs = filepath.Join(filepath.Dir(path), target)
					}
					if !strings.HasPrefix(filepath.Clean(targetAbs), config.ConfigDir) {
						return nil
					}
				}
			}
			rel, _ := filepath.Rel(config.ConfigDir, path)
			localFiles[rel] = [2]int64{info.ModTime().Unix(), info.Size()}
		}
		return nil
	})

	remCfg := config.ConfigDir
	if remHome != "" {
		remCfg = config.ToRemotePath(config.ConfigDir, remHome)
	}

	remoteFiles := preprobedCfg
	if remoteFiles == nil {
		remoteFiles = make(map[string][2]int64)
		cmdStr := fmt.Sprintf("mkdir -p '%s' && if stat -c '%%n' /dev/null >/dev/null 2>&1; then find '%s' -type f -exec stat -c '%%n\t%%Y\t%%s' {} + 2>/dev/null; else find '%s' -type f -exec stat -f '%%N\t%%m\t%%z' {} + 2>/dev/null; fi", remCfg, remCfg, remCfg)
		stdout, _, _, _ := ssh.RunSSHCmd(host, port, cmdStr, false)

		for _, line := range strings.Split(stdout, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.Split(line, "\t")
			if len(parts) >= 3 {
				fullPath := parts[0]
				mtime, _ := strconv.ParseInt(parts[1], 10, 64)
				sz, _ := strconv.ParseInt(parts[2], 10, 64)

				rel := fullPath
				if strings.HasPrefix(fullPath, remCfg) {
					rel = strings.TrimLeft(strings.TrimPrefix(fullPath, remCfg), "/")
				} else if strings.Contains(fullPath, "config/") {
					rel = fullPath[strings.Index(fullPath, "config/")+7:]
				}
				cleanRel := filepath.Clean(rel)
				if cleanRel == "." || strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) {
					continue
				}
				remoteFiles[cleanRel] = [2]int64{mtime, sz}
			}
		}
	}

	var toPush, toPull []string
	allKeys := make(map[string]bool)
	for k := range localFiles {
		allKeys[k] = true
	}
	for k := range remoteFiles {
		allKeys[k] = true
	}
	for k := range allKeys {
		loc, inLoc := localFiles[k]
		rem, inRem := remoteFiles[k]
		if inLoc && !inRem {
			toPush = append(toPush, k)
		} else if inRem && !inLoc {
			toPull = append(toPull, k)
		} else {
			cmp := CompareFileParity(loc[0], loc[1], rem[0], rem[1], clockSkew)
			if cmp > 0 {
				toPush = append(toPush, k)
			} else if cmp < 0 {
				toPull = append(toPull, k)
			}
		}
	}
	sort.Strings(toPush)
	sort.Strings(toPull)

	var totalPushBytes int64
	for _, k := range toPush {
		if loc, ok := localFiles[k]; ok {
			totalPushBytes += loc[1]
		}
	}
	var totalPullBytes int64
	for _, k := range toPull {
		if rem, ok := remoteFiles[k]; ok {
			totalPullBytes += rem[1]
		}
	}

	pushedCnt := 0
	if len(toPush) > 0 {
		reportProgress(fmt.Sprintf("Skills ↑ [%s/%s (0%%)]", terminal.FormatBytes(0, true), terminal.FormatBytes(totalPushBytes, true)))
		inputList := strings.Join(toPush, "\n") + "\n"
		p1 := exec.Command("tar", "-C", config.ConfigDir, "-czf", "-", "-T", "-")
		p1.Stdin = strings.NewReader(inputList)

		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("mkdir -p '%s' && tar -C '%s' --no-same-owner -xzf -", remCfg, remCfg), true)
		p2 := exec.Command("ssh", sshArgs...)

		nBytes, err := RunPipedCommandsWithBytes(p1, p2, func(cur int64) {
			pct := 0.0
			if totalPushBytes > 0 {
				pct = (float64(cur) / float64(totalPushBytes)) * 100.0
				if pct > 100.0 {
					pct = 100.0
				}
			}
			reportProgress(fmt.Sprintf("Skills ↑ [%s/%s (%.0f%%)]", terminal.FormatBytes(cur, true), terminal.FormatBytes(totalPushBytes, true), pct))
		})
		if err == nil {
			pushedCnt = len(toPush)
			if tracker != nil {
				tracker.AddPush(nBytes)
			}
		}
	}

	pulledCnt := 0
	if len(toPull) > 0 {
		reportProgress(fmt.Sprintf("Skills ↓ [%s/%s (0%%)]", terminal.FormatBytes(0, true), terminal.FormatBytes(totalPullBytes, true)))
		inputList := strings.Join(toPull, "\n") + "\n"
		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("tar -C '%s' -czf - -T -", remCfg), true)
		p1 := exec.Command("ssh", sshArgs...)
		p1.Stdin = strings.NewReader(inputList)

		_ = os.MkdirAll(config.ConfigDir, 0755)
		p2 := exec.Command("tar", "-C", config.ConfigDir, "--no-same-owner", "-xzf", "-")
		nBytes, err := RunPipedCommandsWithBytes(p1, p2, func(cur int64) {
			pct := 0.0
			if totalPullBytes > 0 {
				pct = (float64(cur) / float64(totalPullBytes)) * 100.0
				if pct > 100.0 {
					pct = 100.0
				}
			}
			reportProgress(fmt.Sprintf("Skills ↓ [%s/%s (%.0f%%)]", terminal.FormatBytes(cur, true), terminal.FormatBytes(totalPullBytes, true), pct))
		})
		if err == nil {
			pulledCnt = len(toPull)
			if tracker != nil {
				tracker.AddPull(nBytes)
			}
		}
	}

	return pushedCnt, pulledCnt
}

func SyncPoolAccounts(host string, port int, remHome string, clockSkew float64, probeCache map[string]*RemoteFileInfo, tracker ...*ByteTracker) string {
	var trk *ByteTracker
	if len(tracker) > 0 {
		trk = tracker[0]
	}
	remPoolCfg := config.PoolAccountsFile
	if remHome != "" {
		remPoolCfg = config.ToRemotePath(config.PoolAccountsFile, remHome)
	}
	localExists := false
	if _, err := os.Stat(config.PoolAccountsFile); err == nil {
		localExists = true
	}

	remoteExists := false
	if probeCache != nil && probeCache[remPoolCfg] != nil {
		cached := probeCache[remPoolCfg]
		remoteExists = true
		locHash := ""
		if localExists {
			locHash = GetFileHash(config.PoolAccountsFile)
		}
		if locHash != "" && cached.SHA256 != "" && locHash == cached.SHA256 {
			cnt := 0
			if data, err := os.ReadFile(config.PoolAccountsFile); err == nil {
				var parsed map[string]interface{}
				if err := json.Unmarshal(data, &parsed); err == nil {
					if accs, ok := parsed["accounts"].([]interface{}); ok {
						cnt = len(accs)
					}
				}
			}
			return fmt.Sprintf("identical (%d accounts)", cnt)
		}
	} else {
		stdout, _, _, _ := ssh.RunSSHCmd(host, port, fmt.Sprintf("test -f '%s' && echo EXISTS || echo NONE", remPoolCfg), false)
		remoteExists = strings.Contains(stdout, "EXISTS")
	}

	if !localExists && !remoteExists {
		return "none"
	}

	if localExists && !remoteExists {
		st := SyncSingleFile(config.PoolAccountsFile, remPoolCfg, host, port, "agy-pool-accounts.json", clockSkew, probeCache, trk, "600")
		if st == "failed" {
			return "failed"
		}
		return "pushed"
	}

	if !localExists && remoteExists {
		st := SyncSingleFile(config.PoolAccountsFile, remPoolCfg, host, port, "agy-pool-accounts.json", clockSkew, probeCache, trk, "600")
		if st == "failed" {
			return "failed"
		}
		return "pulled"
	}

	// Superset union
	var localData map[string]interface{}
	if data, err := os.ReadFile(config.PoolAccountsFile); err == nil {
		if err := json.Unmarshal(data, &localData); err != nil {
			return "failed (local accounts.json corrupted)"
		}
	}

	stdout, _, _, _ := ssh.RunSSHCmd(host, port, fmt.Sprintf("cat '%s' 2>/dev/null", remPoolCfg), false)
	var remoteData map[string]interface{}
	if stdout != "" {
		if err := json.Unmarshal([]byte(stdout), &remoteData); err != nil {
			return "failed (remote accounts.json corrupted)"
		}
	}

	extractAccs := func(m map[string]interface{}) []map[string]interface{} {
		var res []map[string]interface{}
		if m != nil {
			if list, ok := m["accounts"].([]interface{}); ok {
				for _, item := range list {
					if accMap, ok := item.(map[string]interface{}); ok {
						res = append(res, accMap)
					}
				}
			}
		}
		return res
	}

	localAccs := extractAccs(localData)
	remoteAccs := extractAccs(remoteData)

	getAccKey := func(a map[string]interface{}) string {
		if em, ok := a["email"].(string); ok && em != "" {
			return em
		}
		if id, ok := a["id"].(string); ok && id != "" {
			return id
		}
		b, _ := json.Marshal(a)
		return string(b)
	}

	mergeAccount := func(a, b map[string]interface{}) map[string]interface{} {
		res := make(map[string]interface{})
		for k, v := range a {
			res[k] = v
		}

		normalizeTime := func(t float64) float64 {
			if t > 1e11 {
				return t / 1000.0
			}
			return t
		}

		// 1. Identify which side has newer OAuth token credentials
		getTokenTime := func(m map[string]interface{}) float64 {
			if v, ok := m["updated_at"].(float64); ok && v > 0 {
				return normalizeTime(v)
			}
			if v, ok := m["token_expiry"].(float64); ok && v > 0 {
				return normalizeTime(v)
			}
			return 0
		}
		if getTokenTime(b) > getTokenTime(a) {
			tokenKeys := []string{"access_token", "refresh_token", "id_token", "token_expiry", "updated_at"}
			for _, tk := range tokenKeys {
				if v, ok := b[tk]; ok {
					res[tk] = v
				}
			}
		}

		// 2. Identify which side has newer quota telemetry (last_quota)
		getQuotaTime := func(m map[string]interface{}) float64 {
			if q, ok := m["last_quota"].(map[string]interface{}); ok {
				if u, ok := q["updated_at"].(float64); ok && u > 0 {
					return normalizeTime(u)
				}
			}
			return 0
		}
		if getQuotaTime(b) > getQuotaTime(a) {
			if qb, ok := b["last_quota"]; ok {
				res["last_quota"] = qb
			}
		}

		// 3. Usage Telemetry: monotonic max
		getIntVal := func(m map[string]interface{}, key string) int64 {
			if v, ok := m[key]; ok {
				if f, ok := v.(float64); ok {
					if (key == "last_used_at" || key == "created_at") && f > 1e11 {
						f = f / 1000.0
					}
					return int64(f)
				}
			}
			return 0
		}
		for _, k := range []string{"gen_count", "request_count", "error_count", "last_used_at"} {
			vA := getIntVal(a, k)
			vB := getIntVal(b, k)
			if vB > vA {
				res[k] = vB
			} else if vA > 0 {
				res[k] = vA
			}
		}

		// 4. Rate-limiting & validation status: latest activity wins
		getLatestActivityTime := func(m map[string]interface{}) float64 {
			t := getTokenTime(m)
			if u := float64(getIntVal(m, "last_used_at")); u > t {
				t = u
			}
			if q := getQuotaTime(m); q > t {
				t = q
			}
			return t
		}
		if getLatestActivityTime(b) > getLatestActivityTime(a) {
			for _, sk := range []string{"status", "validation_url", "rate_limited_until"} {
				if v, ok := b[sk]; ok {
					res[sk] = v
				} else {
					delete(res, sk)
				}
			}
		}

		// 5. Created_at: earliest creation time wins
		createdA := getIntVal(a, "created_at")
		createdB := getIntVal(b, "created_at")
		if createdA > 0 && createdB > 0 {
			if createdB < createdA {
				res["created_at"] = createdB
			} else {
				res["created_at"] = createdA
			}
		} else if createdB > 0 {
			res["created_at"] = createdB
		}

		// 6. Name: retain non-empty name
		if n, ok := res["name"].(string); !ok || strings.TrimSpace(n) == "" {
			if nb, ok := b["name"].(string); ok && strings.TrimSpace(nb) != "" {
				res["name"] = nb
			}
		}

		return res
	}

	mergedMap := make(map[string]map[string]interface{})
	for _, a := range localAccs {
		k := getAccKey(a)
		mergedMap[k] = a
	}
	for _, a := range remoteAccs {
		k := getAccKey(a)
		if locA, ok := mergedMap[k]; !ok {
			mergedMap[k] = a
		} else {
			mergedMap[k] = mergeAccount(locA, a)
		}
	}

	// Deterministic array order: preserve local array order first, then append any new remote accounts
	var orderedKeys []string
	seenKeys := make(map[string]bool)

	for _, a := range localAccs {
		k := getAccKey(a)
		if !seenKeys[k] {
			seenKeys[k] = true
			orderedKeys = append(orderedKeys, k)
		}
	}
	for _, a := range remoteAccs {
		k := getAccKey(a)
		if !seenKeys[k] {
			seenKeys[k] = true
			orderedKeys = append(orderedKeys, k)
		}
	}

	var mergedAccs []map[string]interface{}
	for _, k := range orderedKeys {
		if a, ok := mergedMap[k]; ok {
			mergedAccs = append(mergedAccs, a)
		}
	}

	// Base template
	base := localData
	if len(base) == 0 {
		base = remoteData
	}
	mergedPool := make(map[string]interface{})
	for k, v := range base {
		mergedPool[k] = v
	}
	mergedPool["accounts"] = mergedAccs

	// Merge generation_events if present
	extractEvents := func(m map[string]interface{}) []map[string]interface{} {
		var res []map[string]interface{}
		if m != nil {
			if list, ok := m["generation_events"].([]interface{}); ok {
				for _, item := range list {
					if em, ok := item.(map[string]interface{}); ok {
						res = append(res, em)
					}
				}
			}
		}
		return res
	}

	locEvents := extractEvents(localData)
	remEvents := extractEvents(remoteData)
	if len(locEvents) > 0 || len(remEvents) > 0 {
		type evKey struct {
			ID string
			TS int64
		}
		seenEv := make(map[evKey]bool)
		var allEvents []map[string]interface{}
		for _, ev := range append(locEvents, remEvents...) {
			idStr, _ := ev["id"].(string)
			var tsVal int64
			if f, ok := ev["ts"].(float64); ok {
				tsVal = int64(f)
			}
			k := evKey{ID: idStr, TS: tsVal}
			if !seenEv[k] {
				seenEv[k] = true
				allEvents = append(allEvents, ev)
			}
		}
		sort.Slice(allEvents, func(i, j int) bool {
			var tsI, tsJ float64
			if f, ok := allEvents[i]["ts"].(float64); ok {
				tsI = f
			}
			if f, ok := allEvents[j]["ts"].(float64); ok {
				tsJ = f
			}
			return tsI < tsJ
		})
		if len(allEvents) > 1000 {
			allEvents = allEvents[len(allEvents)-1000:]
		}
		mergedPool["generation_events"] = allEvents
	}

	// Ensure active_account_id is valid
	validActive := false
	curActive, _ := mergedPool["active_account_id"].(string)
	for _, acc := range mergedAccs {
		if idStr, ok := acc["id"].(string); ok && idStr == curActive {
			validActive = true
			break
		}
	}
	if !validActive && len(mergedAccs) > 0 {
		if firstID, ok := mergedAccs[0]["id"].(string); ok {
			mergedPool["active_account_id"] = firstID
		}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(mergedPool)
	contentStr := buf.String()

	localChanged := true
	remoteChanged := true
	if data, err := os.ReadFile(config.PoolAccountsFile); err == nil && string(data) == contentStr {
		localChanged = false
	}
	if stdout == contentStr {
		remoteChanged = false
	}

	if localChanged {
		_ = config.WriteFileAtomic(config.PoolAccountsFile, []byte(contentStr), 0600)
		if trk != nil {
			trk.AddPull(int64(len(contentStr)))
		}
	}

	if remoteChanged {
		remDir := filepath.Dir(remPoolCfg)
		remTmp := fmt.Sprintf("%s.tmp_%d_%s", remPoolCfg, time.Now().UnixNano(), randomToken(3))
		cmd := fmt.Sprintf("mkdir -p '%s' && (cat > '%s' && chmod 600 '%s' && mv -f '%s' '%s' || (rm -f '%s' 2>/dev/null; false))", remDir, remTmp, remTmp, remTmp, remPoolCfg, remTmp)
		sshArgs := ssh.GetSSHCmd(host, port, cmd, true)
		proc := exec.Command("ssh", sshArgs...)
		proc.Stdin = strings.NewReader(contentStr)
		if err := proc.Run(); err != nil {
			return "failed to update remote accounts"
		}
		if trk != nil {
			trk.AddPush(int64(len(contentStr)))
		}
	}

	if localChanged && remoteChanged {
		return fmt.Sprintf("merged (%d accounts)", len(mergedAccs))
	} else if localChanged {
		return fmt.Sprintf("pulled (%d accounts)", len(mergedAccs))
	} else if remoteChanged {
		return fmt.Sprintf("pushed (%d accounts)", len(mergedAccs))
	}
	return fmt.Sprintf("identical (%d accounts)", len(mergedAccs))
}

func SyncPoolEngine(host string, port int, remHome string, clockSkew float64, probeCache map[string]*RemoteFileInfo, tracker ...*ByteTracker) string {
	remPoolDir := fmt.Sprintf("%s/agy-pool", remHome)
	localHasPool := false
	if fi, err := os.Stat(config.PoolDir); err == nil && fi.IsDir() {
		localHasPool = true
	}

	localBin := filepath.Join(config.PoolDir, "bin", "agy-pool")
	remBin := fmt.Sprintf("%s/bin/agy-pool", remPoolDir)

	var locHash string
	var locMTime int64
	if fi, err := os.Stat(localBin); err == nil {
		if data, err := os.ReadFile(localBin); err == nil {
			_, locHash = GetScriptBodyAndHash(data)
		}
		locMTime = fi.ModTime().Unix()
	}

	// 1-RTT Fast Path: Probe cache hit check (0 extra network roundtrips)
	if probeCache != nil && probeCache[remBin] != nil {
		cached := probeCache[remBin]
		if locHash != "" && (cached.BodySHA == locHash || cached.SHA256 == locHash) {
			return "up-to-date"
		}
	}

	stdout, _, _, _ := ssh.RunSSHCmd(host, port, fmt.Sprintf("test -d '%s' && echo YES || echo NO", remPoolDir), false)
	remoteHasPool := strings.Contains(stdout, "YES")

	if !localHasPool && !remoteHasPool {
		return "none"
	}

	if localHasPool && remoteHasPool {
		pyProbe := fmt.Sprintf(`%s python3 -c "
import os, hashlib
p = '%s'
if os.path.isfile(p):
    st = os.stat(p)
    c = open(p, 'rb').read()
    b = c.split(bytes([10]), 1)[1] if c.startswith(b'#!') and bytes([10]) in c else c
    print(f'{int(st.st_mtime)} {hashlib.sha256(b).hexdigest()}')
else:
    print('NONE')
" 2>/dev/null || echo NONE`, config.PathPrefix, remBin)

		outProbe, _, _, _ := ssh.RunSSHCmd(host, port, pyProbe, false)
		outProbe = strings.TrimSpace(outProbe)
		var remMTime int64
		remHash := ""
		if outProbe != "" && outProbe != "NONE" {
			parts := strings.Fields(outProbe)
			if len(parts) >= 2 {
				m, _ := strconv.ParseInt(parts[0], 10, 64)
				remMTime = m - int64(clockSkew)
				remHash = parts[1]
			}
		}

		if locHash != "" && remHash != "" && locHash == remHash {
			return "up-to-date"
		}

		if locMTime < remMTime {
			// Remote is newer, pull to local
			sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("tar -C '%s' --exclude=.git -czf - agy-pool", remHome), true)
			p1 := exec.Command("ssh", sshArgs...)
			p2 := exec.Command("tar", "-C", config.HomeDir, "--no-same-owner", "-xzf", "-")
			nBytes, err := RunPipedCommandsWithBytes(p1, p2)
			if err == nil {
				_ = os.Chmod(localBin, 0755)
				if len(tracker) > 0 && tracker[0] != nil {
					tracker[0].AddPull(nBytes)
				}
				return "pulled & ready"
			}
			return "failed"
		}
	}

	// Push local to remote
	if localHasPool {
		p1 := exec.Command("tar", "-C", config.HomeDir, "--exclude=.git", "--exclude=__pycache__", "--exclude=*.pyc", "--exclude=.DS_Store", "-czf", "-", "agy-pool")
		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("tar -C '%s' --no-same-owner -xzf - 2>/dev/null", remHome), true)
		p2 := exec.Command("ssh", sshArgs...)
		nBytes, err := RunPipedCommandsWithBytes(p1, p2)
		if err == nil {
			if len(tracker) > 0 && tracker[0] != nil {
				tracker[0].AddPush(nBytes)
			}
			postCmd := fmt.Sprintf(`export PATH="/data/data/com.termux/files/usr/bin:/usr/local/bin:$HOME/.local/bin:$HOME/bin:$PATH";
chmod +x '%s/bin/agy-pool' '%s/bin/agy-raw' '%s/install.sh' 2>/dev/null || true;
if [ -f '%s/install.sh' ]; then
  bash '%s/install.sh' >/dev/null 2>&1 || true;
fi;
if [ -w /usr/local/bin ] || [ "$(id -u)" -eq 0 ]; then
  ln -sf '%s/bin/agy-pool' /usr/local/bin/agy-pool 2>/dev/null || true;
elif [ -d /data/data/com.termux/files/usr/bin ]; then
  ln -sf '%s/bin/agy-pool' /data/data/com.termux/files/usr/bin/agy-pool 2>/dev/null || true;
fi;
if [ -x '%s/bin/agy-pool' ]; then
  '%s/bin/agy-pool' restart >/dev/null 2>&1 || true;
elif command -v agy-pool >/dev/null 2>&1; then
  agy-pool restart >/dev/null 2>&1 || true;
fi`, remPoolDir, remPoolDir, remPoolDir, remPoolDir, remPoolDir, remPoolDir, remPoolDir, remPoolDir, remPoolDir)
			_, _, _, _ = ssh.RunSSHCmd(host, port, postCmd, false)
			if !remoteHasPool {
				return "installed & ready"
			}
			return "pushed"
		}
		return "failed"
	}

	return "none"
}

func SyncAgySyncGo(host string, port int, remHome string, clockSkew float64, isForce bool, tracker ...*ByteTracker) string {
	remGoDir := fmt.Sprintf("%s/agy-sync-go", remHome)
	localHasGo := false
	if fi, err := os.Stat(config.AgySyncGoDir); err == nil && fi.IsDir() {
		localHasGo = true
	}

	remCheckCmd := fmt.Sprintf("if [ -d '%s' ]; then echo YES; git -C '%s' rev-parse HEAD 2>/dev/null || echo NONE; git -C '%s' log -1 --format=%%ct 2>/dev/null || echo 0; git -C '%s' status --porcelain --untracked-files=no 2>/dev/null; else echo NO; fi", remGoDir, remGoDir, remGoDir, remGoDir)
	remOut, _, _, _ := ssh.RunSSHCmd(host, port, remCheckCmd, false)
	remLines := strings.Split(strings.TrimSpace(remOut), "\n")
	remoteHasGo := (len(remLines) > 0 && strings.TrimSpace(remLines[0]) == "YES")

	if !localHasGo && !remoteHasGo {
		return "none"
	}

	// 1. Initial adoption: local doesn't have it, remote does -> pull
	if !localHasGo && remoteHasGo {
		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("tar -C '%s' --exclude=bin --exclude='*.tmp*' --exclude=.git/index.lock -czf - agy-sync-go", remHome), true)
		p1 := exec.Command("ssh", sshArgs...)
		p2 := exec.Command("tar", "-C", config.HomeDir, "--no-same-owner", "-xzf", "-")
		nBytes, err := RunPipedCommandsWithBytes(p1, p2)
		if err == nil {
			if len(tracker) > 0 && tracker[0] != nil {
				tracker[0].AddPull(nBytes)
			}
			return "pulled & ready"
		}
		return "failed"
	}

	// 2. Initial deployment: local has it, remote does not -> push
	if localHasGo && !remoteHasGo {
		p1 := exec.Command("tar", "-C", config.HomeDir, "--exclude=bin", "--exclude='*.tmp*'", "--exclude=.git/index.lock", "-czf", "-", "agy-sync-go")
		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("tar -C '%s' --no-same-owner -xzf - 2>/dev/null", remHome), true)
		p2 := exec.Command("ssh", sshArgs...)
		nBytes, err := RunPipedCommandsWithBytes(p1, p2)
		if err == nil {
			if len(tracker) > 0 && tracker[0] != nil {
				tracker[0].AddPush(nBytes)
			}
			return "installed & ready"
		}
		return "failed"
	}

	// 3. Both sides have repository: inspect git status and commit timestamps
	locHash := ""
	var locTime int64
	if out, err := exec.Command("git", "-C", config.AgySyncGoDir, "rev-parse", "HEAD").Output(); err == nil {
		locHash = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("git", "-C", config.AgySyncGoDir, "log", "-1", "--format=%ct").Output(); err == nil {
		locTime, _ = strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	}
	locDirty := false
	if out, err := exec.Command("git", "-C", config.AgySyncGoDir, "status", "--porcelain", "--untracked-files=no").Output(); err == nil {
		if len(strings.TrimSpace(string(out))) > 0 {
			locDirty = true
		}
	}

	remHash := ""
	var remTime int64
	remDirty := false
	if len(remLines) > 1 {
		remHash = strings.TrimSpace(remLines[1])
	}
	if len(remLines) > 2 {
		remTime, _ = strconv.ParseInt(strings.TrimSpace(remLines[2]), 10, 64)
	}
	if len(remLines) > 3 {
		for _, l := range remLines[3:] {
			if len(strings.TrimSpace(l)) > 0 {
				remDirty = true
				break
			}
		}
	}

	if isForce && remDirty {
		cleanCmd := fmt.Sprintf("git -C '%s' checkout . 2>/dev/null && git -C '%s' clean -fd 2>/dev/null", remGoDir, remGoDir)
		_, _, _, _ = ssh.RunSSHCmd(host, port, cleanCmd, false)
		remDirty = false
	}

	// Case 1: Commits are identical
	if locHash != "" && remHash != "" && locHash == remHash {
		if !locDirty && !remDirty {
			return "identical"
		}
		if remDirty && !locDirty {
			// Remote has active in-progress edits on top of the same commit.
			// Never wipe remote's in-progress work!
			return "identical (remote working tree preserved)"
		}
		if locDirty && !remDirty {
			// Local has uncommitted work to push to remote
		} else if locDirty && remDirty {
			if !isForce {
				return "conflict (both dirty)"
			}
		}
	}

	// Case 2 & 3: Commits differ
	// If both sides are dirty with uncommitted edits, protect against collision unless force
	if !isForce && locDirty && remDirty && locHash != remHash {
		return "conflict (both dirty)"
	}

	// Determine direction based on commit timestamps
	// If local commit is strictly newer (or local dirty while remote clean): push
	// If remote commit is strictly newer: pull
	shouldPush := false
	if isForce || (locDirty && !remDirty) || locTime > remTime || (locTime == remTime && locHash >= remHash) {
		shouldPush = true
	}

	if shouldPush {
		p1 := exec.Command("tar", "-C", config.HomeDir, "--exclude=bin", "--exclude='*.tmp*'", "--exclude=.git/index.lock", "-czf", "-", "agy-sync-go")
		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("git -C '%s' checkout . 2>/dev/null; git -C '%s' clean -fd 2>/dev/null; tar -C '%s' --no-same-owner -xzf - 2>/dev/null && git -C '%s' checkout . 2>/dev/null", remGoDir, remGoDir, remHome, remGoDir), true)
		p2 := exec.Command("ssh", sshArgs...)
		nBytes, err := RunPipedCommandsWithBytes(p1, p2)
		if err == nil {
			if len(tracker) > 0 && tracker[0] != nil {
				tracker[0].AddPush(nBytes)
			}
			return "pushed"
		}
		return "failed"
	} else {
		// Remote is newer: pull from remote
		_ = exec.Command("git", "-C", config.AgySyncGoDir, "checkout", ".").Run()
		_ = exec.Command("git", "-C", config.AgySyncGoDir, "clean", "-fd").Run()
		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("tar -C '%s' --exclude=bin --exclude='*.tmp*' --exclude=.git/index.lock -czf - agy-sync-go", remHome), true)
		p1 := exec.Command("ssh", sshArgs...)
		p2 := exec.Command("tar", "-C", config.HomeDir, "--no-same-owner", "-xzf", "-")
		nBytes, err := RunPipedCommandsWithBytes(p1, p2)
		if err == nil {
			_ = exec.Command("git", "-C", config.AgySyncGoDir, "checkout", ".").Run()
			if len(tracker) > 0 && tracker[0] != nil {
				tracker[0].AddPull(nBytes)
			}
			return "pulled"
		}
		return "failed"
	}
}
