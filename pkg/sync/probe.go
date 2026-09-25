package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"agy-sync/pkg/config"
	"agy-sync/pkg/ssh"
)

type RemoteFileInfo struct {
	MTime   int64
	Size    int64
	SHA256  string
	Mode    string
	BodySHA string
}

var (
	localHashMu    sync.RWMutex
	localHashCache = make(map[string]localHashEntry)
)

type localHashEntry struct {
	mtime int64
	size  int64
	hash  string
}

func GetFileHash(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	mtime := fi.ModTime().Unix()
	size := fi.Size()

	localHashMu.RLock()
	if c, ok := localHashCache[path]; ok && c.mtime == mtime && c.size == size {
		localHashMu.RUnlock()
		return c.hash
	}
	localHashMu.RUnlock()

	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	h := sha256.New()
	buf := make([]byte, 65536)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	res := hex.EncodeToString(h.Sum(nil))

	localHashMu.Lock()
	localHashCache[path] = localHashEntry{mtime: mtime, size: size, hash: res}
	localHashMu.Unlock()

	return res
}

func GetScriptBodyAndHash(content []byte) ([]byte, string) {
	body := content
	if len(content) >= 2 && content[0] == '#' && content[1] == '!' {
		idx := -1
		for i, b := range content {
			if b == '\n' {
				idx = i
				break
			}
		}
		if idx >= 0 && idx+1 < len(content) {
			body = content[idx+1:]
		}
	}
	h := sha256.Sum256(body)
	return body, hex.EncodeToString(h[:])
}

func GetPeerClockSkew(host string, port int) float64 {
	t0 := time.Now()
	cmdStr := fmt.Sprintf("%s date +%%s.%%N 2>/dev/null || date +%%s", config.PathPrefix)
	stdout, _, _, err := ssh.RunSSHCmd(host, port, cmdStr, false)
	t1 := time.Now()
	if err != nil {
		return 0.0
	}

	rtt := t1.Sub(t0).Seconds()
	var remoteTS float64
	found := false

	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		line = strings.TrimSpace(line)
		if val, err := strconv.ParseFloat(line, 64); err == nil {
			remoteTS = val
			found = true
			break
		}
	}
	if !found {
		return 0.0
	}

	localMidpoint := float64(t0.UnixNano())/1e9 + (rtt / 2.0)
	skew := remoteTS - localMidpoint
	return math.Round(skew*1000) / 1000.0
}

func BatchProbeRemoteFiles(host string, port int, remotePaths []string) map[string]*RemoteFileInfo {
	if len(remotePaths) == 0 {
		return make(map[string]*RemoteFileInfo)
	}

	pyScript := `
import sys, os, hashlib
for p in sys.argv[1:]:
    if os.path.isfile(p):
        try:
            st = os.stat(p)
            mode = oct(st.st_mode & 0o777)[2:]
            c = open(p, 'rb').read()
            h = hashlib.sha256(c).hexdigest()
            body_h = ""
            if p.endswith("/agy-sync") or p.endswith("/agy-pool"):
                b = c.split(b"\n", 1)[1] if c.startswith(b"#!") and b"\n" in c else c
                body_h = hashlib.sha256(b).hexdigest()
            print(f'{p}\t{int(st.st_mtime)}\t{st.st_size}\t{h}\t{mode}\t{body_h}')
        except Exception:
            print(f'{p}\tNONE')
    else:
        print(f'{p}\tNONE')
`
	var quotedPaths []string
	for _, p := range remotePaths {
		quotedPaths = append(quotedPaths, fmt.Sprintf("'%s'", strings.ReplaceAll(p, "'", "'\\''")))
	}

	cmdStr := fmt.Sprintf("%s (python3 - %s || python - %s)", config.PathPrefix, strings.Join(quotedPaths, " "), strings.Join(quotedPaths, " "))
	sshArgs := ssh.GetSSHCmd(host, port, cmdStr, true)
	proc := exec.Command("ssh", sshArgs...)
	proc.Stdin = strings.NewReader(pyScript)

	outBytes, err := proc.Output()
	cache := make(map[string]*RemoteFileInfo)
	if err != nil || len(outBytes) == 0 {
		return cache
	}

	for _, line := range strings.Split(strings.TrimSpace(string(outBytes)), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) >= 4 {
			p := parts[0]
			mtime, _ := strconv.ParseInt(parts[1], 10, 64)
			size, _ := strconv.ParseInt(parts[2], 10, 64)
			sha := parts[3]
			mode := "0644"
			if len(parts) > 4 && parts[4] != "" {
				mode = parts[4]
			}
			bodyH := ""
			if len(parts) > 5 {
				bodyH = parts[5]
			}
			cache[p] = &RemoteFileInfo{
				MTime:   mtime,
				Size:    size,
				SHA256:  sha,
				Mode:    mode,
				BodySHA: bodyH,
			}
		} else if len(parts) >= 2 && parts[1] == "NONE" {
			cache[parts[0]] = nil
		}
	}
	return cache
}

func CopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	srcStat, err := in.Stat()
	if err != nil {
		return err
	}

	_ = os.MkdirAll(filepath.Dir(dst), 0755)
	tmpDst := fmt.Sprintf("%s.tmp_%d_%s", dst, time.Now().UnixNano(), config.RandomToken(3))
	out, err := os.OpenFile(tmpDst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, srcStat.Mode())
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()

	if copyErr != nil {
		_ = os.Remove(tmpDst)
		return copyErr
	}
	if syncErr != nil {
		_ = os.Remove(tmpDst)
		return syncErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpDst)
		return closeErr
	}

	_ = os.Chmod(tmpDst, srcStat.Mode())
	if err := os.Rename(tmpDst, dst); err != nil {
		_ = os.Remove(tmpDst)
		return err
	}
	return nil
}

type RemoteDirectoryInventory struct {
	ConvDBs map[string][2]int64 // [mtime, size]
	Anns    map[string]int64    // mtime
	Brain   map[string][2]int64 // [mtime, size]
	Configs map[string][2]int64 // [mtime, size]
}

func BatchProbeDirectoryInventories(host string, port int, remHome string) *RemoteDirectoryInventory {
	remConv := config.ConvDir
	remAnn := config.AnnDir
	remBrain := config.BrainDir
	remCfg := config.ConfigDir
	if remHome != "" {
		remConv = config.ToRemotePath(config.ConvDir, remHome)
		remAnn = config.ToRemotePath(config.AnnDir, remHome)
		remBrain = config.ToRemotePath(config.BrainDir, remHome)
		remCfg = config.ToRemotePath(config.ConfigDir, remHome)
	}

	batchScript := fmt.Sprintf(
		"mkdir -p '%s' '%s' '%s' '%s' 2>/dev/null && ("+
			"echo '===CONV_START==='; stat -c '%%n %%Y %%s' '%s'/*.db 2>/dev/null; "+
			"echo '===ANN_START==='; stat -c '%%n %%Y' '%s'/*.pbtxt 2>/dev/null; "+
			"echo '===BRAIN_START==='; (stat -c '%%n\t%%Y\t%%s' '%s'/*/.system_generated/logs/transcript.jsonl 2>/dev/null; stat -c '%%n\t%%Y\t0' '%s'/*/ 2>/dev/null); "+
			"echo '===CFG_START==='; find '%s' -type f -exec stat -c '%%n\t%%Y\t%%s' {} + 2>/dev/null; "+
			"echo '===INV_END===')",
		remConv, remAnn, remBrain, remCfg,
		remConv,
		remAnn,
		remBrain, remBrain,
		remCfg,
	)

	stdout, _, _, err := ssh.RunSSHCmd(host, port, batchScript, false)
	if err != nil || !strings.Contains(stdout, "===CONV_START===") {
		return nil
	}

	inv := &RemoteDirectoryInventory{
		ConvDBs: make(map[string][2]int64),
		Anns:    make(map[string]int64),
		Brain:   make(map[string][2]int64),
		Configs: make(map[string][2]int64),
	}

	currentSection := ""
	for _, rawLine := range strings.Split(stdout, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		switch line {
		case "===CONV_START===":
			currentSection = "conv"
			continue
		case "===ANN_START===":
			currentSection = "ann"
			continue
		case "===BRAIN_START===":
			currentSection = "brain"
			continue
		case "===CFG_START===":
			currentSection = "cfg"
			continue
		case "===INV_END===":
			currentSection = ""
			continue
		}

		switch currentSection {
		case "conv":
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				fname := filepath.Base(parts[0])
				if !strings.HasSuffix(fname, ".db") || fname == ".db" || strings.Contains(fname, "*") || strings.Contains(fname, "/") || strings.Contains(fname, "\\") {
					continue
				}
				mtime, _ := strconv.ParseInt(parts[1], 10, 64)
				sz, _ := strconv.ParseInt(parts[2], 10, 64)
				inv.ConvDBs[fname] = [2]int64{mtime, sz}
			}
		case "ann":
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				fname := filepath.Base(parts[0])
				if !strings.HasSuffix(fname, ".pbtxt") || fname == ".pbtxt" || strings.Contains(fname, "*") || strings.Contains(fname, "/") || strings.Contains(fname, "\\") {
					continue
				}
				mtime, _ := strconv.ParseInt(parts[1], 10, 64)
				inv.Anns[fname] = mtime
			}
		case "brain":
			parts := strings.Split(line, "\t")
			if len(parts) >= 3 {
				fullPath := parts[0]
				mtime, _ := strconv.ParseInt(parts[1], 10, 64)
				sz, _ := strconv.ParseInt(parts[2], 10, 64)

				var cleanUUID string
				if strings.Contains(fullPath, "/.system_generated") {
					sub := fullPath[:strings.Index(fullPath, "/.system_generated")]
					cleanUUID = filepath.Base(sub)
				} else {
					cleanUUID = filepath.Base(strings.TrimRight(fullPath, "/"))
				}

				cleanUUID = filepath.Clean(cleanUUID)
				if cleanUUID == "" || cleanUUID == "." || cleanUUID == ".." || strings.Contains(cleanUUID, "/") || strings.Contains(cleanUUID, "\\") {
					continue
				}

				if existing, ok := inv.Brain[cleanUUID]; ok && existing[1] > 0 && sz == 0 {
					continue
				}
				inv.Brain[cleanUUID] = [2]int64{mtime, sz}
			}
		case "cfg":
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
				inv.Configs[cleanRel] = [2]int64{mtime, sz}
			}
		}
	}

	return inv
}
