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
)

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func SyncSingleFile(localPath, remotePath, host string, port int, name string, clockSkew float64, probeCache map[string]*RemoteFileInfo, mode ...string) string {
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
		probeCmd := fmt.Sprintf(`f="%s"; if test -f "$f"; then echo "$(stat -c "%%Y %%s" "$f") $(sha256sum "$f" | awk '{print $1}')"; else echo "NONE"; fi`, remotePath)
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
	if localExists && (!remoteExists || localMTime >= normalizedRemoteMTime) {
		remoteParent := filepath.Dir(remotePath)
		tmpRemote := fmt.Sprintf("%s.tmp_push_%d_%s", remotePath, time.Now().Unix(), randomToken(3))
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
			return "pushed"
		}
		return "failed"
	} else if remoteExists {
		_ = os.MkdirAll(filepath.Dir(localPath), 0755)
		tmpLocal := fmt.Sprintf("%s.tmp_pull_%d_%s", localPath, time.Now().Unix(), randomToken(3))
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
			if err := os.WriteFile(tmpLocal, out, modeVal); err == nil {
				_ = os.Rename(tmpLocal, localPath)
				if normalizedRemoteMTime > 0 {
					mtime := time.Unix(normalizedRemoteMTime, 0)
					_ = os.Chtimes(localPath, mtime, mtime)
				}
				return "pulled"
			}
		}
		_ = os.Remove(tmpLocal)
		return "failed"
	}

	return "unchanged"
}

func SyncAuthorizedKeys(host string, port int, remHome string, probeCache map[string]*RemoteFileInfo) string {
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
		_ = os.MkdirAll(filepath.Dir(config.AuthKeysFile), 0700)
		tmpAuth := config.AuthKeysFile + ".tmp"
		if err := os.WriteFile(tmpAuth, []byte(content), 0600); err == nil {
			_ = os.Rename(tmpAuth, config.AuthKeysFile)
		}
	}

	if remoteChanged {
		remDir := filepath.Dir(remAuth)
		cmd := fmt.Sprintf("mkdir -p '%s' && cat > '%s.tmp' && chmod 600 '%s.tmp' && mv -f '%s.tmp' '%s'", remDir, remAuth, remAuth, remAuth, remAuth)
		sshArgs := ssh.GetSSHCmd(host, port, cmd, true)
		proc := exec.Command("ssh", sshArgs...)
		proc.Stdin = strings.NewReader(content)
		if err := proc.Run(); err != nil {
			return "failed to update remote keys"
		}
	}

	return fmt.Sprintf("%d active key(s)", len(merged))
}

func SyncClusterRegistry(host string, port int, clockSkew float64, remHome string, probeCache map[string]*RemoteFileInfo) string {
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
	}

	if remoteChg || len(remoteReg) == 0 {
		tmpReg := fmt.Sprintf("%s.tmp_%d", remDev, time.Now().Unix())
		remDir := filepath.Dir(remDev)
		cmd := fmt.Sprintf("mkdir -p '%s' && cat > '%s' && mv -f '%s' '%s'", remDir, tmpReg, tmpReg, remDev)
		sshArgs := ssh.GetSSHCmd(host, port, cmd, true)
		proc := exec.Command("ssh", sshArgs...)
		data, _ := json.MarshalIndent(merged, "", "  ")
		proc.Stdin = strings.NewReader(string(data) + "\n")
		if err := proc.Run(); err != nil {
			return "failed to update remote registry"
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

func SyncHistoryJsonl(host string, port int, remHome string, probeCache map[string]*RemoteFileInfo) string {
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
		}
		key := fmt.Sprintf("%d:%s", ts, disp)
		entries[key] = histEntry{Timestamp: ts, Line: lineS}
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
		_ = os.MkdirAll(filepath.Dir(config.HistoryFile), 0755)
		tmpLocal := fmt.Sprintf("%s.tmp_%d_%s", config.HistoryFile, time.Now().Unix(), randomToken(3))
		if err := os.WriteFile(tmpLocal, []byte(mergedContent), 0644); err == nil {
			_ = os.Rename(tmpLocal, config.HistoryFile)
		}
	}

	if remoteChanged {
		remDir := filepath.Dir(remHist)
		remTmp := fmt.Sprintf("%s.tmp_%d_%s", remHist, time.Now().Unix(), randomToken(3))
		cmd := fmt.Sprintf("mkdir -p '%s' && (cat > '%s' && mv -f '%s' '%s' || (rm -f '%s' 2>/dev/null; false))", remDir, remTmp, remTmp, remHist, remTmp)
		sshArgs := ssh.GetSSHCmd(host, port, cmd, true)
		proc := exec.Command("ssh", sshArgs...)
		proc.Stdin = strings.NewReader(mergedContent)
		if err := proc.Run(); err != nil {
			return "failed to update remote history"
		}
	}

	if !localChanged && !remoteChanged {
		return fmt.Sprintf("identical (%d entries)", len(list))
	}
	return fmt.Sprintf("%d entries merged", len(list))
}

func SyncConfigsAndSkills(host string, port int, clockSkew float64, remHome string) (int, int) {
	_ = os.MkdirAll(config.ConfigDir, 0755)
	localFiles := make(map[string][2]int64)

	_ = filepath.Walk(config.ConfigDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(config.ConfigDir, path)
			localFiles[rel] = [2]int64{info.ModTime().Unix(), info.Size()}
		}
		return nil
	})

	remCfg := config.ConfigDir
	if remHome != "" {
		remCfg = config.ToRemotePath(config.ConfigDir, remHome)
	}

	cmdStr := fmt.Sprintf("mkdir -p '%s' && find '%s' -type f -exec stat -c '%%n\t%%Y\t%%s' {} + 2>/dev/null", remCfg, remCfg)
	stdout, _, _, _ := ssh.RunSSHCmd(host, port, cmdStr, false)
	remoteFiles := make(map[string][2]int64)

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
			remMTime := mtime - int64(clockSkew)

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
			remoteFiles[cleanRel] = [2]int64{remMTime, sz}
		}
	}

	var toPush, toPull []string
	for k, loc := range localFiles {
		if rem, ok := remoteFiles[k]; !ok {
			toPush = append(toPush, k)
		} else if loc[0] > rem[0]+2 || (loc[1] != rem[1] && loc[0] > rem[0]) {
			toPush = append(toPush, k)
		}
	}
	for k, rem := range remoteFiles {
		if loc, ok := localFiles[k]; !ok {
			toPull = append(toPull, k)
		} else if rem[0] > loc[0]+2 || (rem[1] != loc[1] && rem[0] > loc[0]) {
			toPull = append(toPull, k)
		}
	}

	pushedCnt := 0
	if len(toPush) > 0 {
		inputList := strings.Join(toPush, "\n") + "\n"
		p1 := exec.Command("tar", "-C", config.ConfigDir, "-czf", "-", "-T", "-")
		p1.Stdin = strings.NewReader(inputList)

		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("mkdir -p '%s' && tar -C '%s' --no-same-owner -xzf -", remCfg, remCfg), true)
		p2 := exec.Command("ssh", sshArgs...)

		if err := RunPipedCommands(p1, p2); err == nil {
			pushedCnt = len(toPush)
		}
	}

	pulledCnt := 0
	if len(toPull) > 0 {
		inputList := strings.Join(toPull, "\n") + "\n"
		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("tar -C '%s' -czf - -T -", remCfg), true)
		p1 := exec.Command("ssh", sshArgs...)
		p1.Stdin = strings.NewReader(inputList)

		p2 := exec.Command("tar", "-C", config.ConfigDir, "--no-same-owner", "-xzf", "-")
		if err := RunPipedCommands(p1, p2); err == nil {
			pulledCnt = len(toPull)
		}
	}

	return pushedCnt, pulledCnt
}

func SyncPoolAccounts(host string, port int, remHome string, clockSkew float64, probeCache map[string]*RemoteFileInfo) string {
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
		st := SyncSingleFile(config.PoolAccountsFile, remPoolCfg, host, port, "agy-pool-accounts.json", clockSkew, probeCache, "600")
		if st == "failed" {
			return "failed"
		}
		return "pushed"
	}

	if !localExists && remoteExists {
		st := SyncSingleFile(config.PoolAccountsFile, remPoolCfg, host, port, "agy-pool-accounts.json", clockSkew, probeCache, "600")
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

		// 1. Identify which side has newer OAuth token credentials
		getTokenTime := func(m map[string]interface{}) float64 {
			if v, ok := m["updated_at"].(float64); ok && v > 0 {
				return v
			}
			if v, ok := m["token_expiry"].(float64); ok && v > 0 {
				return v
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
					return u
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

	contentBytes, _ := json.MarshalIndent(mergedPool, "", "  ")
	contentStr := string(contentBytes) + "\n"

	localChanged := true
	remoteChanged := true
	if data, err := os.ReadFile(config.PoolAccountsFile); err == nil && string(data) == contentStr {
		localChanged = false
	}
	if stdout == contentStr {
		remoteChanged = false
	}

	if localChanged {
		tmpLocal := fmt.Sprintf("%s.tmp_%d_%s", config.PoolAccountsFile, time.Now().Unix(), randomToken(3))
		if err := os.WriteFile(tmpLocal, []byte(contentStr), 0600); err == nil {
			_ = os.Rename(tmpLocal, config.PoolAccountsFile)
		}
	}

	if remoteChanged {
		remDir := filepath.Dir(remPoolCfg)
		remTmp := fmt.Sprintf("%s.tmp_%d_%s", remPoolCfg, time.Now().Unix(), randomToken(3))
		cmd := fmt.Sprintf("mkdir -p '%s' && cat > '%s' && chmod 600 '%s' && mv -f '%s' '%s'", remDir, remTmp, remTmp, remTmp, remPoolCfg)
		sshArgs := ssh.GetSSHCmd(host, port, cmd, true)
		proc := exec.Command("ssh", sshArgs...)
		proc.Stdin = strings.NewReader(contentStr)
		if err := proc.Run(); err != nil {
			return "failed to update remote accounts"
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

func SyncPoolEngine(host string, port int, remHome string, clockSkew float64, probeCache ...map[string]*RemoteFileInfo) string {
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
	if len(probeCache) > 0 && probeCache[0] != nil && probeCache[0][remBin] != nil {
		cached := probeCache[0][remBin]
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
			if err := RunPipedCommands(p1, p2); err == nil {
				_ = os.Chmod(localBin, 0755)
				return "pulled & ready"
			}
			return "failed"
		}
	}

	// Push local to remote
	if localHasPool {
		p1 := exec.Command("tar", "-C", config.HomeDir, "--exclude=.git", "-czf", "-", "agy-pool")
		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("tar -C '%s' --no-same-owner -xzf - 2>/dev/null", remHome), true)
		p2 := exec.Command("ssh", sshArgs...)
		if err := RunPipedCommands(p1, p2); err == nil {
			postCmd := fmt.Sprintf(`chmod +x '%s/bin/agy-pool' '%s/install.sh' 2>/dev/null || true;
if [ -w /usr/local/bin ] || [ "$(id -u)" -eq 0 ]; then
  ln -sf '%s/bin/agy-pool' /usr/local/bin/agy-pool 2>/dev/null || true;
elif [ -d /data/data/com.termux/files/usr/bin ]; then
  ln -sf '%s/bin/agy-pool' /data/data/com.termux/files/usr/bin/agy-pool 2>/dev/null || true;
fi`, remPoolDir, remPoolDir, remPoolDir, remPoolDir)
			_, _, _, _ = ssh.RunSSHCmd(host, port, postCmd, false)
			if !remoteHasPool {
				return "installed & ready"
			}
			return "up-to-date"
		}
		return "failed"
	}

	return "none"
}

func SyncAgySyncGo(host string, port int, remHome string, clockSkew float64) string {
	remGoDir := fmt.Sprintf("%s/agy-sync-go", remHome)
	localHasGo := false
	if fi, err := os.Stat(config.AgySyncGoDir); err == nil && fi.IsDir() {
		localHasGo = true
	}

	remCheckCmd := fmt.Sprintf("if [ -d '%s' ]; then echo YES; git -C '%s' rev-parse HEAD 2>/dev/null || echo NONE; git -C '%s' log -1 --format=%%ct 2>/dev/null || echo 0; git -C '%s' status --porcelain 2>/dev/null; else echo NO; fi", remGoDir, remGoDir, remGoDir, remGoDir)
	remOut, _, _, _ := ssh.RunSSHCmd(host, port, remCheckCmd, false)
	remLines := strings.Split(strings.TrimSpace(remOut), "\n")
	remoteHasGo := (len(remLines) > 0 && strings.TrimSpace(remLines[0]) == "YES")

	if !localHasGo && !remoteHasGo {
		return "none"
	}

	// 1. Initial adoption: local doesn't have it, remote does -> pull
	if !localHasGo && remoteHasGo {
		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("tar -C '%s' --exclude=bin --exclude='*.tmp*' -czf - agy-sync-go", remHome), true)
		p1 := exec.Command("ssh", sshArgs...)
		p2 := exec.Command("tar", "-C", config.HomeDir, "--no-same-owner", "-xzf", "-")
		if err := RunPipedCommands(p1, p2); err == nil {
			return "pulled & ready"
		}
		return "failed"
	}

	// 2. Initial deployment: local has it, remote does not -> push
	if localHasGo && !remoteHasGo {
		p1 := exec.Command("tar", "-C", config.HomeDir, "--exclude=bin", "--exclude='*.tmp*'", "-czf", "-", "agy-sync-go")
		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("tar -C '%s' --no-same-owner -xzf - 2>/dev/null", remHome), true)
		p2 := exec.Command("ssh", sshArgs...)
		if err := RunPipedCommands(p1, p2); err == nil {
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
	if out, err := exec.Command("git", "-C", config.AgySyncGoDir, "status", "--porcelain").Output(); err == nil {
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

	if locHash != "" && remHash != "" && locHash == remHash && !locDirty && !remDirty {
		return "identical"
	}

	// Conflict protection: if both sides are dirty with uncommitted edits, never overwrite either side
	if locDirty && remDirty {
		return "conflict (both dirty)"
	}
	// Safety protection: if remote is dirty and local is clean, never overwrite remote's uncommitted work
	if remDirty && !locDirty {
		return "remote modified (dirty)"
	}

	// Determine direction:
	// If local has uncommitted work (locDirty), local takes precedence to push
	// If both are clean, compare commit timestamps
	shouldPush := false
	if locDirty && !remDirty {
		shouldPush = true
	} else if locTime >= remTime {
		shouldPush = true
	}

	if shouldPush {
		p1 := exec.Command("tar", "-C", config.HomeDir, "--exclude=bin", "--exclude='*.tmp*'", "-czf", "-", "agy-sync-go")
		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("tar -C '%s' --no-same-owner -xzf - 2>/dev/null", remHome), true)
		p2 := exec.Command("ssh", sshArgs...)
		if err := RunPipedCommands(p1, p2); err == nil {
			return "pushed"
		}
		return "failed"
	} else {
		// Remote is newer: pull from remote
		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("tar -C '%s' --exclude=bin --exclude='*.tmp*' -czf - agy-sync-go", remHome), true)
		p1 := exec.Command("ssh", sshArgs...)
		p2 := exec.Command("tar", "-C", config.HomeDir, "--no-same-owner", "-xzf", "-")
		if err := RunPipedCommands(p1, p2); err == nil {
			return "pulled"
		}
		return "failed"
	}
}
