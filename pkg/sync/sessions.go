package sync

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	stdsync "sync"
	"time"

	"agy-sync/pkg/config"
	"agy-sync/pkg/ssh"
	"agy-sync/pkg/terminal"
)

// CompareFileParity safely compares local and remote file metadata (mtime, size)
// while protecting against false-positive syncs caused by inter-device wall-clock skew.
// Returns:
//  0: identical (no sync needed)
//  1: local is newer (needs push)
// -1: remote is newer (needs pull)
func CompareFileParity(locMTime, locSize, remRawMTime, remSize int64, clockSkew float64) int {
	// 1. Identity Short-Circuit:
	// If file sizes are strictly identical and raw mtimes are identical, it is 100% the exact same file!
	// (Preserved via tar/chtimes during previous sync). Never manipulate raw timestamps.
	if locSize == remSize && locMTime == remRawMTime {
		return 0
	}

	// 2. Strict size inequality takes precedence for SQLite databases & append logs
	if locSize > remSize {
		return 1
	} else if remSize > locSize {
		return -1
	}

	// 3. File sizes are identical (locSize == remSize), but raw mtimes differ:
	// Use clock-skew normalized comparison with an adaptive tolerance window.
	// The tolerance must absorb network jitter and clock measurement inaccuracy:
	// tolerance = max(5, abs(clockSkew) + 3)
	tolerance := int64(math.Abs(clockSkew)) + 3
	if tolerance < 5 {
		tolerance = 5
	}

	normRemMTime := remRawMTime - int64(clockSkew)
	if locMTime > normRemMTime+tolerance {
		return 1
	} else if normRemMTime > locMTime+tolerance {
		return -1
	}

	// Difference falls within clock drift window and sizes are identical -> identical!
	return 0
}

func SyncConversationSummaries(host string, port int, clockSkew float64, remHome string, probeCache map[string]*RemoteFileInfo, tracker ...*ByteTracker) string {
	fi, err := os.Stat(config.SummariesDBFile)
	if err != nil {
		return "skipped"
	}

	remDB := config.SummariesDBFile
	if remHome != "" {
		remDB = config.ToRemotePath(config.SummariesDBFile, remHome)
	}

	locWalFI, locWalErr := os.Stat(config.SummariesDBFile + "-wal")
	locHasWal := locWalErr == nil && locWalFI.Size() > 0
	remHasWal := probeCache != nil && probeCache[remDB+"-wal"] != nil && probeCache[remDB+"-wal"].Size > 0

	// Fast-path: 0-RTT short circuit if probeCache confirmed identical size/mtime or SHA256 (and NO pending WALs)
	if !locHasWal && !remHasWal && probeCache != nil && probeCache[remDB] != nil {
		cached := probeCache[remDB]
		locSize := fi.Size()
		locMTime := fi.ModTime().Unix()
		// 1. Identity short-circuit via CompareFileParity
		if CompareFileParity(locMTime, locSize, cached.MTime, cached.Size, clockSkew) == 0 {
			return "identical"
		}
		// 2. SHA256 exact match short-circuit
		locHash := GetFileHash(config.SummariesDBFile)
		if locHash != "" && cached.SHA256 != "" && locHash == cached.SHA256 {
			return "identical"
		}
	}

	pyMergeScript := fmt.Sprintf(`
import sqlite3, tempfile, sys, os, subprocess, json
from datetime import datetime, timezone

local_db = '%s'
rem_db = '%s'
clock_skew = %f
path_prefix = '%s'

def get_snapshot(db_path):
    if not os.path.isfile(db_path):
        return None
    tmp = tempfile.NamedTemporaryFile(suffix='.db', delete=False)
    tmp.close()
    try:
        src = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
        dst = sqlite3.connect(tmp.name)
        src.backup(dst)
        dst.close()
        src.close()
        return tmp.name
    except Exception:
        if os.path.exists(tmp.name):
            os.unlink(tmp.name)
        return None

def parse_iso(ts_str):
    if not ts_str:
        return 0.0
    ts = str(ts_str).strip()
    try:
        dt = datetime.fromisoformat(ts.replace("Z", "+00:00"))
        return dt.timestamp()
    except Exception:
        pass
    for fmt in ("%%Y-%%m-%%dT%%H:%%M:%%S.%%fZ", "%%Y-%%m-%%dT%%H:%%M:%%SZ", "%%Y-%%m-%%d %%H:%%M:%%S"):
        try:
            dt = datetime.strptime(ts, fmt)
            return dt.replace(tzinfo=timezone.utc).timestamp()
        except Exception:
            pass
    return 0.0

loc_snap = get_snapshot(local_db)
if not loc_snap:
    sys.exit(0)

# Fetch remote snapshot
tmp_remote = tempfile.NamedTemporaryFile(suffix='.db', delete=False)
tmp_remote.close()

remote_snap_cmd = (
    f"{path_prefix} python3 -c \"import sqlite3, tempfile, sys, os; "
    f"p = '{rem_db}'; "
    f"tmp = tempfile.NamedTemporaryFile(suffix='.db', delete=False); "
    f"tmp.close(); "
    f"src = sqlite3.connect(f'file:{{p}}?mode=ro', uri=True); "
    f"dst = sqlite3.connect(tmp.name); "
    f"src.backup(dst); dst.close(); src.close(); "
    f"sys.stdout.buffer.write(open(tmp.name, 'rb').read()); "
    f"os.unlink(tmp.name)\" 2>/dev/null || cat '{rem_db}' 2>/dev/null"
)

ssh_cmd = ["ssh", "-p", "%d", "-o", "ConnectTimeout=8", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR", "-o", "ControlMaster=auto", "-o", "ControlPath=%s/cm-%%C", "-o", "ControlPersist=60s", "-o", "Ciphers=aes128-gcm@openssh.com,aes256-gcm@openssh.com,chacha20-poly1305@openssh.com", "%s", remote_snap_cmd]

with open(tmp_remote.name, "wb") as f_out:
    subprocess.run(ssh_cmd, stdout=f_out)

rem_snap = tmp_remote.name if os.path.getsize(tmp_remote.name) > 0 else None

local_rows = {}
remote_rows = {}

try:
    lconn = sqlite3.connect(loc_snap)
    lconn.row_factory = sqlite3.Row
    local_rows = {row["conversation_id"]: dict(row) for row in lconn.execute("SELECT * FROM conversation_summaries").fetchall()}
    lconn.close()
except Exception:
    pass

if rem_snap:
    try:
        rconn = sqlite3.connect(rem_snap)
        rconn.row_factory = sqlite3.Row
        remote_rows = {row["conversation_id"]: dict(row) for row in rconn.execute("SELECT * FROM conversation_summaries").fetchall()}
        rconn.close()
    except Exception:
        pass

all_ids = set(local_rows.keys()) | set(remote_rows.keys())
merged_rows = {}
local_changed = False
remote_changed = False

for cid in all_ids:
    in_local = cid in local_rows
    in_remote = cid in remote_rows
    if in_local and not in_remote:
        merged_rows[cid] = local_rows[cid]
        remote_changed = True
    elif in_remote and not in_local:
        merged_rows[cid] = remote_rows[cid]
        local_changed = True
    else:
        l_raw_str = str(local_rows[cid].get("last_modified_time") or "")
        r_raw_str = str(remote_rows[cid].get("last_modified_time") or "")
        if l_raw_str != "" and l_raw_str == r_raw_str:
            merged_rows[cid] = local_rows[cid]
        else:
            l_ts = parse_iso(l_raw_str)
            r_ts = parse_iso(r_raw_str)
            if l_ts > 0 and r_ts > 0:
                if abs(l_ts - r_ts) < 0.001:
                    merged_rows[cid] = local_rows[cid]
                else:
                    norm_r_ts = r_ts - clock_skew
                    tol = max(5.0, abs(clock_skew) + 3.0)
                    if l_ts > norm_r_ts + tol:
                        merged_rows[cid] = local_rows[cid]
                        remote_changed = True
                    elif norm_r_ts > l_ts + tol:
                        merged_rows[cid] = remote_rows[cid]
                        local_changed = True
                    else:
                        merged_rows[cid] = local_rows[cid]
            else:
                merged_rows[cid] = local_rows[cid]

def write_db(db_path, rows):
    conn = sqlite3.connect(db_path, timeout=30.0)
    cur = conn.cursor()
    cur.execute("PRAGMA journal_mode=WAL")
    cur.execute("PRAGMA busy_timeout=5000")
    for r in rows.values():
        keys = list(r.keys())
        placeholders = ', '.join(['?'] * len(keys))
        cols = ', '.join(keys)
        cur.execute(f"INSERT OR REPLACE INTO conversation_summaries ({cols}) VALUES ({placeholders})", list(r.values()))
    conn.commit()
    conn.close()

if local_changed:
    write_db(local_db, merged_rows)

if remote_changed or not rem_snap:
    tmp_out = tempfile.NamedTemporaryFile(suffix='.db', delete=False)
    tmp_out.close()
    write_db(tmp_out.name, merged_rows)
    rem_dir = os.path.dirname(rem_db)
    push_ssh = ["ssh", "-p", "%d", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR", "-o", "ControlMaster=auto", "-o", "ControlPath=%s/cm-%%C", "-o", "ControlPersist=60s", "-o", "Ciphers=aes128-gcm@openssh.com,aes256-gcm@openssh.com,chacha20-poly1305@openssh.com", "%s", f"mkdir -p '{rem_dir}' && tmp='{rem_db}.tmp_$$' && {{ cat > \"$tmp\" && rm -f '{rem_db}-wal' '{rem_db}-shm' 2>/dev/null && mv -f \"$tmp\" '{rem_db}'; }} || (rm -f \"$tmp\" 2>/dev/null; false)"]
    with open(tmp_out.name, "rb") as f_in:
        subprocess.run(push_ssh, stdin=f_in)
    if os.path.exists(tmp_out.name):
        os.unlink(tmp_out.name)

if os.path.exists(loc_snap): os.unlink(loc_snap)
if os.path.exists(tmp_remote.name): os.unlink(tmp_remote.name)

if local_changed and remote_changed:
    print(f"merged ({len(merged_rows)} sessions)")
elif local_changed:
    print(f"pulled ({len(merged_rows)} sessions)")
elif remote_changed:
    print(f"pushed ({len(merged_rows)} sessions)")
else:
    print(f"identical ({len(merged_rows)} sessions)")
`, config.SummariesDBFile, remDB, clockSkew, config.PathPrefix, port, config.SocketDir, host, port, config.SocketDir, host)

	cmd := exec.Command("python3", "-c", pyMergeScript)
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	err = cmd.Run()
	resStr := "synced"
	if err == nil && stdoutBuf.Len() > 0 {
		resStr = strings.TrimSpace(stdoutBuf.String())
	}
	if len(tracker) > 0 && tracker[0] != nil {
		if fi, err := os.Stat(config.SummariesDBFile); err == nil && fi.Size() > 0 {
			if strings.Contains(resStr, "pushed") {
				tracker[0].AddPush(fi.Size())
			} else if strings.Contains(resStr, "pulled") {
				tracker[0].AddPull(fi.Size())
			} else if strings.Contains(resStr, "merged") {
				tracker[0].AddPush(fi.Size())
				tracker[0].AddPull(fi.Size())
			}
		}
	}
	if resStr != "identical" {
		InvalidateSummaryMtimes()
	}
	return resStr
}

var (
	cachedSummaryMtimes map[string]float64
	summaryMtimesLock   stdsync.Mutex
)

// InvalidateSummaryMtimes clears the cached conversation summary timestamps.
func InvalidateSummaryMtimes() {
	summaryMtimesLock.Lock()
	cachedSummaryMtimes = nil
	summaryMtimesLock.Unlock()
}

// LoadSummaryMtimes reads conversation_summaries.db and returns a map of
// conversation_id -> last_modified_time as unix epoch timestamps.
func LoadSummaryMtimes() map[string]float64 {
	summaryMtimesLock.Lock()
	defer summaryMtimesLock.Unlock()
	if cachedSummaryMtimes != nil {
		return cachedSummaryMtimes
	}

	m := make(map[string]float64)
	if _, err := os.Stat(config.SummariesDBFile); err != nil {
		return m
	}

	pyScript := fmt.Sprintf(`import sqlite3, os, sys
from datetime import datetime
p = '%s'
if os.path.isfile(p):
    try:
        src = sqlite3.connect(f'file:{p}?mode=ro', uri=True)
        cur = src.cursor()
        cur.execute('SELECT conversation_id, last_modified_time FROM conversation_summaries')
        for cid, ts in cur.fetchall():
            if ts:
                try:
                    t = datetime.fromisoformat(str(ts).strip().replace('Z', '+00:00')).timestamp()
                    if t > 0:
                        sys.stdout.write(f'{cid}\t{t:.3f}\n')
                except Exception:
                    pass
        src.close()
    except Exception:
        pass
`, config.SummariesDBFile)

	cmd := exec.Command("python3", "-c", pyScript)
	out, err := cmd.Output()
	if err == nil && len(out) > 0 {
		for _, line := range strings.Split(string(out), "\n") {
			parts := strings.Split(line, "\t")
			if len(parts) == 2 {
				cid := strings.TrimSpace(parts[0])
				if t, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64); err == nil {
					m[cid] = t
				}
			}
		}
	}
	cachedSummaryMtimes = m
	return m
}

// AlignLocalConversationMtimes ensures that local .db files in conversations/ have filesystem mtimes
// strictly matching their authoritative last_modified_time from conversation_summaries.db.
// This repairs tied mtimes (e.g. from bulk tar extractions) and ensures both Antigravity CLI's
// local eviction manager and agy-sync evaluate the exact same deterministic chronological order.
func AlignLocalConversationMtimes(summaryMtimes map[string]float64) {
	if len(summaryMtimes) == 0 {
		return
	}
	entries, err := os.ReadDir(config.ConvDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		uuid := strings.TrimSuffix(e.Name(), ".db")
		if ts, ok := summaryMtimes[uuid]; ok && ts > 0 {
			fPath := filepath.Join(config.ConvDir, e.Name())
			if fi, err := e.Info(); err == nil {
				curSec := fi.ModTime().Unix()
				targetSec := int64(ts)
				if curSec != targetSec {
					targetTime := time.Unix(targetSec, 0)
					_ = os.Chtimes(fPath, targetTime, targetTime)
				}
			}
		}
	}
}

func SafeDBSnapshot(src, dst string) error {
	walFile := src + "-wal"
	if fi, err := os.Stat(walFile); err == nil && fi.Size() > 0 {
		script := `import sqlite3, sys
try:
    s = sqlite3.connect(f'file:{sys.argv[1]}?mode=ro', uri=True)
    d = sqlite3.connect(sys.argv[2])
    s.backup(d)
    d.close()
    s.close()
except Exception:
    sys.exit(1)
`
		if err := exec.Command("python3", "-c", script, src, dst).Run(); err == nil {
			if fiDst, err := os.Stat(dst); err == nil && fiDst.Size() > 0 {
				return nil
			}
		}
	}
	return CopyFile(src, dst)
}

// filterActiveLRUSessions selects only the most recently active sessions up to maxLimit (default 500),
// directly aligning with Antigravity CLI's internal local cache retention cap.
// Chronological precedence is strictly determined by:
// 1. Authoritative conversation_summaries.db last_modified_time (ISO timestamps)
// 2. Fallback to filesystem mtime when summary record is unavailable (e.g. newly created session)
// 3. When timestamps are tied (e.g. historical archive sessions sharing the same extract mtime),
//    sorting by session UUID key (descending) provides a 100% deterministic, global, perspective-free
//    selection across all cluster nodes, ensuring perfect mathematical symmetry (Sync(A,B) == Sync(B,A)).
func filterActiveLRUSessions(allKeys map[string]bool, localMap, remoteMap map[string][2]int64, maxLimit int, summaryMtimes ...map[string]float64) map[string]bool {
	if maxLimit <= 0 || len(allKeys) <= maxLimit {
		return allKeys
	}

	var sumTimes map[string]float64
	if len(summaryMtimes) > 0 && summaryMtimes[0] != nil {
		sumTimes = summaryMtimes[0]
	}

	type sessionRank struct {
		key  string
		time float64
	}
	var ranks []sessionRank
	for k := range allKeys {
		uuid := strings.TrimSuffix(k, ".db")
		var effectiveTime float64
		if sumTimes != nil {
			if st, ok := sumTimes[uuid]; ok {
				effectiveTime = st
			}
		}
		if effectiveTime == 0 {
			var mt int64
			if loc, ok := localMap[k]; ok && loc[0] > mt {
				mt = loc[0]
			}
			if rem, ok := remoteMap[k]; ok && rem[0] > mt {
				mt = rem[0]
			}
			effectiveTime = float64(mt)
		}
		ranks = append(ranks, sessionRank{key: k, time: effectiveTime})
	}

	sort.Slice(ranks, func(i, j int) bool {
		if ranks[i].time != ranks[j].time {
			return ranks[i].time > ranks[j].time // newest first
		}
		return ranks[i].key > ranks[j].key // deterministic global order across all devices
	})

	active := make(map[string]bool, maxLimit)
	for i := 0; i < maxLimit && i < len(ranks); i++ {
		active[ranks[i].key] = true
	}
	return active
}

func SyncConversationDBs(host string, port int, clockSkew float64, remHome string, tracker *ByteTracker, preprobedDBs map[string][2]int64, onProgress ...func(string)) (int, int, map[string]bool) {
	_ = os.MkdirAll(config.ConvDir, 0755)
	sumTimes := LoadSummaryMtimes()
	AlignLocalConversationMtimes(sumTimes)
	localDBs := make(map[string][2]int64)

	reportProgress := func(msg string) {
		if len(onProgress) > 0 && onProgress[0] != nil {
			onProgress[0](msg)
		}
	}

	entries, err := os.ReadDir(config.ConvDir)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".db") {
				if fi, err := e.Info(); err == nil {
					localDBs[e.Name()] = [2]int64{fi.ModTime().Unix(), fi.Size()}
				}
			}
		}
	}

	remConv := config.ConvDir
	if remHome != "" {
		remConv = config.ToRemotePath(config.ConvDir, remHome)
	}

	remoteDBs := preprobedDBs
	if remoteDBs == nil {
		remoteDBs = make(map[string][2]int64)
		statCmd := fmt.Sprintf("mkdir -p '%s' && (if stat -c '%%n' /dev/null >/dev/null 2>&1; then stat -c '%%n %%Y %%s' '%s'/*.db; else stat -f '%%N %%m %%z' '%s'/*.db; fi) 2>/dev/null", remConv, remConv, remConv)
		stdout, _, exitCode, err := ssh.RunSSHCmd(host, port, statCmd, false)
		if err != nil && exitCode == 255 {
			return 0, 0, nil
		}

		for _, line := range strings.Split(stdout, "\n") {
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				fname := filepath.Base(parts[0])
				if !strings.HasSuffix(fname, ".db") || fname == ".db" || strings.Contains(fname, "*") || strings.Contains(fname, "/") || strings.Contains(fname, "\\") {
					continue
				}
				mtime, _ := strconv.ParseInt(parts[1], 10, 64)
				sz, _ := strconv.ParseInt(parts[2], 10, 64)
				remoteDBs[fname] = [2]int64{mtime, sz}
			}
		}
	}

	allKeys := make(map[string]bool)
	for k := range localDBs {
		allKeys[k] = true
	}
	for k := range remoteDBs {
		allKeys[k] = true
	}
	activeKeys := filterActiveLRUSessions(allKeys, localDBs, remoteDBs, config.MaxRetainedSessions, sumTimes)

	// Clean up local cold sessions exceeding MaxRetainedSessions to maintain
	// perfect synchronization parity with Antigravity CLI's 500-session retention cap.
	if len(localDBs) > config.MaxRetainedSessions && len(activeKeys) > 0 {
		for k := range localDBs {
			if !activeKeys[k] {
				_ = os.Remove(filepath.Join(config.ConvDir, k))
				_ = os.Remove(filepath.Join(config.ConvDir, k+"-wal"))
				_ = os.Remove(filepath.Join(config.ConvDir, k+"-shm"))
				delete(localDBs, k)
				uuid := strings.TrimSuffix(k, ".db")
				_ = os.RemoveAll(filepath.Join(config.BrainDir, uuid))
			}
		}
	}

	var toPush, toPull []string
	for k := range activeKeys {
		loc, inLoc := localDBs[k]
		rem, inRem := remoteDBs[k]
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
	for _, dbName := range toPush {
		if loc, ok := localDBs[dbName]; ok {
			totalPushBytes += loc[1]
		}
	}
	var totalPullBytes int64
	for _, dbName := range toPull {
		if rem, ok := remoteDBs[dbName]; ok {
			totalPullBytes += rem[1]
		}
	}

	pushedCount := 0
	if len(toPush) > 0 {
		stagingDir, err := os.MkdirTemp("", "conv_push_*")
		if err == nil {
			defer os.RemoveAll(stagingDir)

			reportProgress(fmt.Sprintf("Conv DBs snapshot (%d dbs, %s)...", len(toPush), terminal.FormatBytes(totalPushBytes, true)))

			var validPush []string
			for _, dbName := range toPush {
				srcFile := filepath.Join(config.ConvDir, dbName)
				dstFile := filepath.Join(stagingDir, dbName)
				if err := SafeDBSnapshot(srcFile, dstFile); err == nil {
					if fi, err := os.Stat(srcFile); err == nil {
						_ = os.Chtimes(dstFile, fi.ModTime(), fi.ModTime())
					}
					validPush = append(validPush, dbName)
				}
			}

			if len(validPush) > 0 {
				tarArgs := append([]string{"-C", stagingDir, "-czf", "-"}, validPush...)
				p1 := exec.Command("tar", tarArgs...)

				var cleanList []string
				for _, d := range validPush {
					cleanList = append(cleanList, fmt.Sprintf("'%s/%s-wal' '%s/%s-shm'", remConv, d, remConv, d))
				}
				cleanCmd := strings.Join(cleanList, " ")
				remoteCmd := fmt.Sprintf("%s mkdir -p '%s' && (tar --no-same-owner --warning=no-timestamp -C '%s' -xzf - 2>/dev/null || tar -C '%s' --no-same-owner -xzf - 2>/dev/null) && rm -f %s 2>/dev/null || true",
					config.PathPrefix, remConv, remConv, remConv, cleanCmd)

				sshArgs := ssh.GetSSHCmd(host, port, remoteCmd, true)
				p2 := exec.Command("ssh", sshArgs...)

				reportProgress(fmt.Sprintf("Conv DBs ↑ [%s/%s (0%%)]", terminal.FormatBytes(0, true), terminal.FormatBytes(totalPushBytes, true)))
				nBytes, err := RunPipedCommandsWithBytes(p1, p2, func(cur int64) {
					pct := 0.0
					if totalPushBytes > 0 {
						pct = (float64(cur) / float64(totalPushBytes)) * 100.0
						if pct > 100.0 {
							pct = 100.0
						}
					}
					reportProgress(fmt.Sprintf("Conv DBs ↑ [%s/%s (%.0f%%)]", terminal.FormatBytes(cur, true), terminal.FormatBytes(totalPushBytes, true), pct))
				})
				if err == nil {
					pushedCount = len(validPush)
					if tracker != nil {
						tracker.AddPush(nBytes)
					}
				}
			}
		}
	}

	pulledCount := 0
	if len(toPull) > 0 {
		remotePullPy := fmt.Sprintf(`
import sqlite3, tempfile, shutil, os, sys, subprocess
from pathlib import Path
c = Path('%s')
td = tempfile.mkdtemp()
items = sys.stdin.read().splitlines()
valid = []
for d in items:
    d = d.strip()
    if not d: continue
    lp = c / d
    sp = Path(td) / d
    try:
        s = sqlite3.connect(f'file:{lp}?mode=ro', uri=True)
        dst = sqlite3.connect(str(sp))
        s.backup(dst); dst.close(); s.close()
    except Exception:
        if lp.is_file(): shutil.copy2(lp, sp)
    if lp.is_file():
        try:
            st = lp.stat()
            os.utime(sp, (st.st_atime, st.st_mtime))
        except Exception: pass
    if sp.is_file(): valid.append(d)
if valid:
    subprocess.run(["tar", "-C", td, "-czf", "-"] + valid, stderr=subprocess.DEVNULL)
shutil.rmtree(td, ignore_errors=True)
`, remConv)

		b64Code := base64.StdEncoding.EncodeToString([]byte(remotePullPy))
		remoteCmd := fmt.Sprintf(`%s python3 -c "import base64; exec(base64.b64decode('%s').decode())"`, config.PathPrefix, b64Code)

		sshArgs := ssh.GetSSHCmd(host, port, remoteCmd, true)
		p1 := exec.Command("ssh", sshArgs...)
		p1.Stdin = strings.NewReader(strings.Join(toPull, "\n") + "\n")

		_ = os.MkdirAll(config.ConvDir, 0755)
		p2 := exec.Command("tar", "-C", config.ConvDir, "--no-same-owner", "-xzf", "-")

		reportProgress(fmt.Sprintf("Conv DBs ↓ [%s/%s (0%%)]", terminal.FormatBytes(0, true), terminal.FormatBytes(totalPullBytes, true)))
		pullProgressCb := func(cur int64) {
			pct := 0.0
			if totalPullBytes > 0 {
				pct = (float64(cur) / float64(totalPullBytes)) * 100.0
				if pct > 100.0 {
					pct = 100.0
				}
			}
			reportProgress(fmt.Sprintf("Conv DBs ↓ [%s/%s (%.0f%%)]", terminal.FormatBytes(cur, true), terminal.FormatBytes(totalPullBytes, true), pct))
		}

		nBytes, err := RunPipedCommandsWithBytes(p1, p2, pullProgressCb)
		if err == nil {
			pulledCount = len(toPull)
			if tracker != nil {
				tracker.AddPull(nBytes)
			}
			for _, d := range toPull {
				_ = os.Remove(filepath.Join(config.ConvDir, d+"-wal"))
				_ = os.Remove(filepath.Join(config.ConvDir, d+"-shm"))
			}
		} else {
			// Fallback: direct tar stream if remote lacks python3
			inputList := strings.Join(toPull, "\n") + "\n"
			fbArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("tar -C '%s' -czf - -T -", remConv), true)
			fb1 := exec.Command("ssh", fbArgs...)
			fb1.Stdin = strings.NewReader(inputList)
			fb2 := exec.Command("tar", "-C", config.ConvDir, "--no-same-owner", "-xzf", "-")
			fbBytes, fbErr := RunPipedCommandsWithBytes(fb1, fb2, pullProgressCb)
			if fbErr == nil {
				pulledCount = len(toPull)
				if tracker != nil {
					tracker.AddPull(fbBytes)
				}
				for _, d := range toPull {
					_ = os.Remove(filepath.Join(config.ConvDir, d+"-wal"))
					_ = os.Remove(filepath.Join(config.ConvDir, d+"-shm"))
				}
			}
		}
	}

	if pulledCount > 0 {
		AlignLocalConversationMtimes(sumTimes)
	}

	return pushedCount, pulledCount, activeKeys
}

func SyncAnnotations(host string, port int, clockSkew float64, remHome string, tracker *ByteTracker, preprobedAnns map[string]int64, onProgress ...func(string)) (int, int) {
	_ = os.MkdirAll(config.AnnDir, 0755)
	localAnns := make(map[string]int64)

	entries, err := os.ReadDir(config.AnnDir)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".pbtxt") {
				if fi, err := e.Info(); err == nil {
					localAnns[e.Name()] = fi.ModTime().Unix()
				}
			}
		}
	}

	remAnn := config.AnnDir
	if remHome != "" {
		remAnn = config.ToRemotePath(config.AnnDir, remHome)
	}

	remoteAnns := preprobedAnns
	if remoteAnns == nil {
		remoteAnns = make(map[string]int64)
		cmdStr := fmt.Sprintf("mkdir -p '%s' && (if stat -c '%%n' /dev/null >/dev/null 2>&1; then stat -c '%%n %%Y' '%s'/*.pbtxt; else stat -f '%%N %%m' '%s'/*.pbtxt; fi) 2>/dev/null", remAnn, remAnn, remAnn)
		stdout, _, exitCode, err := ssh.RunSSHCmd(host, port, cmdStr, false)
		if err != nil && exitCode == 255 {
			return 0, 0
		}

		for _, line := range strings.Split(stdout, "\n") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				fname := filepath.Base(parts[0])
				if !strings.HasSuffix(fname, ".pbtxt") || fname == ".pbtxt" || strings.Contains(fname, "*") || strings.Contains(fname, "/") || strings.Contains(fname, "\\") {
					continue
				}
				mtime, _ := strconv.ParseInt(parts[1], 10, 64)
				remoteAnns[fname] = mtime
			}
		}
	}

	var toPush, toPull []string
	tol := int64(math.Abs(clockSkew)) + 3
	if tol < 5 {
		tol = 5
	}
	for k, locMTime := range localAnns {
		remMTime, ok := remoteAnns[k]
		if !ok {
			toPush = append(toPush, k)
		} else if locMTime == remMTime {
			// Identity short-circuit
			continue
		} else {
			normRem := remMTime - int64(clockSkew)
			if locMTime > normRem+tol {
				toPush = append(toPush, k)
			}
		}
	}
	for k, remMTime := range remoteAnns {
		locMTime, ok := localAnns[k]
		if !ok {
			toPull = append(toPull, k)
		} else if locMTime == remMTime {
			// Identity short-circuit
			continue
		} else {
			normRem := remMTime - int64(clockSkew)
			if normRem > locMTime+tol {
				toPull = append(toPull, k)
			}
		}
	}
	sort.Strings(toPush)
	sort.Strings(toPull)

	pushed := 0
	if len(toPush) > 0 {
		inputList := strings.Join(toPush, "\n") + "\n"
		p1 := exec.Command("tar", "-C", config.AnnDir, "-czf", "-", "-T", "-")
		p1.Stdin = strings.NewReader(inputList)

		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("mkdir -p '%s' && tar -C '%s' --no-same-owner -xzf -", remAnn, remAnn), true)
		p2 := exec.Command("ssh", sshArgs...)
		nBytes, err := RunPipedCommandsWithBytes(p1, p2)
		if err == nil {
			pushed = len(toPush)
			if tracker != nil {
				tracker.AddPush(nBytes)
			}
		}
	}

	pulled := 0
	if len(toPull) > 0 {
		inputList := strings.Join(toPull, "\n") + "\n"
		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("tar -C '%s' -czf - -T -", remAnn), true)
		p1 := exec.Command("ssh", sshArgs...)
		p1.Stdin = strings.NewReader(inputList)

		_ = os.MkdirAll(config.AnnDir, 0755)
		p2 := exec.Command("tar", "-C", config.AnnDir, "--no-same-owner", "-xzf", "-")
		nBytes, err := RunPipedCommandsWithBytes(p1, p2)
		if err == nil {
			pulled = len(toPull)
			if tracker != nil {
				tracker.AddPull(nBytes)
			}
		}
	}

	return pushed, pulled
}

func getSessionDirSize(dir string) int64 {
	var total int64
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if info.IsDir() {
			name := info.Name()
			// Exclude .git directories anywhere inside session
			if name == ".git" || strings.HasPrefix(name, ".git") {
				return filepath.SkipDir
			}
			// Exclude runtime execution logs & steps in .system_generated
			if len(parts) >= 2 && parts[0] == ".system_generated" {
				if parts[1] == "tasks" || parts[1] == "steps" {
					return filepath.SkipDir
				}
			}
			// Exclude ANY subdirectories under scratch/ (repos, decompiled trees, build outputs)
			if len(parts) >= 2 && parts[0] == "scratch" {
				return filepath.SkipDir
			}
			if strings.HasPrefix(name, "apktool_out") || strings.HasPrefix(name, "jadx_") ||
				strings.HasPrefix(name, "mtb_") || strings.HasPrefix(name, "node_modules") ||
				strings.HasPrefix(name, "build") || strings.HasPrefix(name, "target") {
				return filepath.SkipDir
			}
		} else {
			// In scratch root, only allow small script/text files <= 512KB and skip binaries/images/archives
			if len(parts) >= 2 && parts[0] == "scratch" {
				if len(parts) > 2 || info.Size() > 512*1024 {
					return nil
				}
				name := strings.ToLower(info.Name())
				if strings.HasSuffix(name, ".img") || strings.HasSuffix(name, ".apk") ||
					strings.HasSuffix(name, ".dex") || strings.HasSuffix(name, ".so") ||
					strings.HasSuffix(name, ".a") || strings.HasSuffix(name, ".o") ||
					strings.HasSuffix(name, ".class") || strings.HasSuffix(name, ".bin") ||
					strings.HasSuffix(name, ".zip") || strings.HasPrefix(name, ".tar") ||
					strings.HasSuffix(name, ".tar") || strings.HasSuffix(name, ".gz") ||
					strings.HasSuffix(name, ".tgz") || strings.HasSuffix(name, ".7z") ||
					strings.HasSuffix(name, ".iso") || strings.HasSuffix(name, ".log") ||
					strings.Contains(name, ".tmp") {
					return nil
				}
			}
			name := strings.ToLower(info.Name())
			if strings.HasSuffix(name, ".apk") || strings.HasSuffix(name, ".dex") || strings.HasSuffix(name, ".img") {
				return nil
			}
			total += info.Size()
		}
		return nil
	})
	return total
}

func SyncBrainSessions(host string, port int, clockSkew float64, remHome string, tracker *ByteTracker, preprobedBrain map[string][2]int64, activeConvKeys map[string]bool, onProgress ...func(string)) (int, int) {
	_ = os.MkdirAll(config.BrainDir, 0755)
	localBrains := make(map[string][2]int64)

	reportProgress := func(msg string) {
		if len(onProgress) > 0 && onProgress[0] != nil {
			onProgress[0](msg)
		}
	}

	entries, err := os.ReadDir(config.BrainDir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				tFile := filepath.Join(config.BrainDir, e.Name(), ".system_generated", "logs", "transcript.jsonl")
				if fi, err := os.Stat(tFile); err == nil {
					localBrains[e.Name()] = [2]int64{fi.ModTime().Unix(), fi.Size()}
				} else if fi, err := e.Info(); err == nil {
					localBrains[e.Name()] = [2]int64{fi.ModTime().Unix(), 0}
				}
			}
		}
	}

	remBrain := config.BrainDir
	if remHome != "" {
		remBrain = config.ToRemotePath(config.BrainDir, remHome)
	}

	remoteBrains := preprobedBrain
	if remoteBrains == nil {
		// Fast-path: Shell stat for all transcript files and session directories (15-40ms vs 2500ms in Python)
		statCmd := fmt.Sprintf(
			"mkdir -p '%s' && if stat -c '%%n' /dev/null >/dev/null 2>&1; then (stat -c '%%n\t%%Y\t%%s' '%s'/*/.system_generated/logs/transcript.jsonl 2>/dev/null; stat -c '%%n\t%%Y\t0' '%s'/*/ 2>/dev/null); else (stat -f '%%N\t%%m\t%%z' '%s'/*/.system_generated/logs/transcript.jsonl 2>/dev/null; stat -f '%%N\t%%m\t0' '%s'/*/ 2>/dev/null); fi",
			remBrain, remBrain, remBrain, remBrain, remBrain,
		)
		stdout, _, exitCode, err := ssh.RunSSHCmd(host, port, statCmd, false)
		if err != nil && exitCode == 255 {
			return 0, 0
		}
		remoteBrains = make(map[string][2]int64)

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

				if existing, ok := remoteBrains[cleanUUID]; ok && existing[1] > 0 && sz == 0 {
					continue
				}
				remoteBrains[cleanUUID] = [2]int64{mtime, sz}
			}
		}
	}

	// Fallback to python scan if fast shell stat returned no entries
	if len(remoteBrains) == 0 {
		scanPy := fmt.Sprintf(`
import os
b = '%s'
if os.path.isdir(b):
    for d in os.listdir(b):
        p = os.path.join(b, d)
        if os.path.isdir(p):
            tf = os.path.join(p, '.system_generated', 'logs', 'transcript.jsonl')
            if os.path.isfile(tf):
                st = os.stat(tf)
                print(f'{d}\t{int(st.st_mtime)}\t{st.st_size}')
            else:
                st = os.stat(p)
                print(f'{d}\t{int(st.st_mtime)}\t0')
`, remBrain)

		b64Scan := base64.StdEncoding.EncodeToString([]byte(scanPy))
		cmdStr := fmt.Sprintf(`mkdir -p '%s' && %s python3 -c "import base64; exec(base64.b64decode('%s').decode())"`, remBrain, config.PathPrefix, b64Scan)
		stdoutPy, _, _, _ := ssh.RunSSHCmd(host, port, cmdStr, false)

		for _, line := range strings.Split(stdoutPy, "\n") {
			parts := strings.Split(line, "\t")
			if len(parts) >= 3 {
				rawUUID := strings.TrimSpace(parts[0])
				cleanUUID := filepath.Base(filepath.Clean(rawUUID))
				if cleanUUID == "" || cleanUUID == "." || cleanUUID == ".." || strings.Contains(cleanUUID, "/") || strings.Contains(cleanUUID, "\\") {
					continue
				}
				mtime, _ := strconv.ParseInt(parts[1], 10, 64)
				sz, _ := strconv.ParseInt(parts[2], 10, 64)
				remoteBrains[cleanUUID] = [2]int64{mtime, sz}
			}
		}
	}

	var allowedConvUUIDs map[string]bool
	if activeConvKeys != nil {
		allowedConvUUIDs = make(map[string]bool, len(activeConvKeys))
		for k := range activeConvKeys {
			uuid := strings.TrimSuffix(k, ".db")
			allowedConvUUIDs[uuid] = true
		}
	}

	allUUIDs := make(map[string]bool)
	for k := range localBrains {
		if allowedConvUUIDs != nil && !allowedConvUUIDs[k] {
			continue // skip cold/orphaned brain sessions
		}
		allUUIDs[k] = true
	}
	for k := range remoteBrains {
		if allowedConvUUIDs != nil && !allowedConvUUIDs[k] {
			continue // skip cold/orphaned brain sessions
		}
		allUUIDs[k] = true
	}
	activeUUIDs := filterActiveLRUSessions(allUUIDs, localBrains, remoteBrains, config.MaxRetainedSessions, LoadSummaryMtimes())

	var toPush, toPull []string
	for k := range activeUUIDs {
		loc, inLoc := localBrains[k]
		rem, inRem := remoteBrains[k]
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
	for _, id := range toPush {
		totalPushBytes += getSessionDirSize(filepath.Join(config.BrainDir, id))
	}

	var totalPullBytes int64
	for _, id := range toPull {
		if sz, ok := remoteBrains[id]; ok && sz[1] > 0 {
			totalPullBytes += sz[1]
		}
	}

	pushedCount := 0
	if len(toPush) > 0 {
		reportProgress(fmt.Sprintf("Brain ↑ [%s/%s (0%%)]", terminal.FormatBytes(0, true), terminal.FormatBytes(totalPushBytes, true)))
		inputList := strings.Join(toPush, "\n") + "\n"
		tarArgs := []string{
			"--warning=no-file-changed",
			// 1. Exclude task terminal logs & step execution caches
			"--exclude=*/.system_generated/tasks*",
			"--exclude=*/.system_generated/steps*",
			// 2. Exclude all git repositories inside sessions
			"--exclude=*/.git*",
			"--exclude=*/*/.git*",
			// 3. Exclude any subdirectories under scratch (repos, decompiled trees, build outputs)
			"--exclude=*/scratch/*/*",
			"--exclude=*/scratch/.*/*",
			// 4. Exclude scratch heavy binary, disk images, archives, native libs, logs & temp files
			"--exclude=*/scratch/*.img",
			"--exclude=*/scratch/*.apk",
			"--exclude=*/scratch/*.dex",
			"--exclude=*/scratch/*.so",
			"--exclude=*/scratch/*.a",
			"--exclude=*/scratch/*.o",
			"--exclude=*/scratch/*.class",
			"--exclude=*/scratch/*.bin",
			"--exclude=*/scratch/*.zip",
			"--exclude=*/scratch/*.tar*",
			"--exclude=*/scratch/*.gz",
			"--exclude=*/scratch/*.tgz",
			"--exclude=*/scratch/*.7z",
			"--exclude=*/scratch/*.iso",
			"--exclude=*/scratch/*.log",
			"--exclude=*/scratch/*.tmp*",
			"--exclude=*/scratch/apktool_out*",
			"--exclude=*/scratch/jadx_*",
			"--exclude=*/scratch/mtb_*",
			"--exclude=*/scratch/node_modules*",
			"--exclude=*/scratch/build*",
			"--exclude=*/scratch/target*",
			"-C", config.BrainDir,
			"-czf", "-",
			"-T", "-",
		}
		p1 := exec.Command("tar", tarArgs...)
		p1.Stdin = strings.NewReader(inputList)

		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("mkdir -p '%s' && tar -C '%s' --no-same-owner -xzf -", remBrain, remBrain), true)
		p2 := exec.Command("ssh", sshArgs...)
		nBytes, err := RunPipedCommandsWithBytes(p1, p2, func(cur int64) {
			pct := 0.0
			if totalPushBytes > 0 {
				pct = (float64(cur) / float64(totalPushBytes)) * 100.0
				if pct > 100.0 {
					pct = 100.0
				}
			}
			reportProgress(fmt.Sprintf("Brain ↑ [%s/%s (%.0f%%)]", terminal.FormatBytes(cur, true), terminal.FormatBytes(totalPushBytes, true), pct))
		})
		if err == nil {
			pushedCount = len(toPush)
			if tracker != nil {
				tracker.AddPush(nBytes)
			}
		}
	}

	pulledCount := 0
	if len(toPull) > 0 {
		reportProgress(fmt.Sprintf("Brain ↓ [%s/%s (0%%)]", terminal.FormatBytes(0, true), terminal.FormatBytes(totalPullBytes, true)))
		inputList := strings.Join(toPull, "\n") + "\n"
		remTarCmd := fmt.Sprintf(
			"tar --warning=no-file-changed "+
				"--exclude='*/.system_generated/tasks*' "+
				"--exclude='*/.system_generated/steps*' "+
				"--exclude='*/.git*' "+
				"--exclude='*/*/.git*' "+
				"--exclude='*/scratch/*/*' "+
				"--exclude='*/scratch/.*/*' "+
				"--exclude='*/scratch/*.img' "+
				"--exclude='*/scratch/*.apk' "+
				"--exclude='*/scratch/*.dex' "+
				"--exclude='*/scratch/*.so' "+
				"--exclude='*/scratch/*.a' "+
				"--exclude='*/scratch/*.o' "+
				"--exclude='*/scratch/*.class' "+
				"--exclude='*/scratch/*.bin' "+
				"--exclude='*/scratch/*.zip' "+
				"--exclude='*/scratch/*.tar*' "+
				"--exclude='*/scratch/*.gz' "+
				"--exclude='*/scratch/*.tgz' "+
				"--exclude='*/scratch/*.7z' "+
				"--exclude='*/scratch/*.iso' "+
				"--exclude='*/scratch/*.log' "+
				"--exclude='*/scratch/*.tmp*' "+
				"--exclude='*/scratch/apktool_out*' "+
				"--exclude='*/scratch/jadx_*' "+
				"--exclude='*/scratch/mtb_*' "+
				"--exclude='*/scratch/node_modules*' "+
				"--exclude='*/scratch/build*' "+
				"--exclude='*/scratch/target*' "+
				"-C '%s' -czf - -T -",
			remBrain,
		)
		sshArgs := ssh.GetSSHCmd(host, port, remTarCmd, true)
		p1 := exec.Command("ssh", sshArgs...)
		p1.Stdin = strings.NewReader(inputList)

		_ = os.MkdirAll(config.BrainDir, 0755)
		p2 := exec.Command("tar", "-C", config.BrainDir, "--no-same-owner", "-xzf", "-")
		nBytes, err := RunPipedCommandsWithBytes(p1, p2, func(cur int64) {
			pct := 0.0
			if totalPullBytes > 0 {
				pct = (float64(cur) / float64(totalPullBytes)) * 100.0
				if pct > 100.0 {
					pct = 100.0
				}
			}
			reportProgress(fmt.Sprintf("Brain ↓ [%s/%s (%.0f%%)]", terminal.FormatBytes(cur, true), terminal.FormatBytes(totalPullBytes, true), pct))
		})
		if err == nil {
			pulledCount = len(toPull)
			if tracker != nil {
				tracker.AddPull(nBytes)
			}
		}
	}

	return pushedCount, pulledCount
}
