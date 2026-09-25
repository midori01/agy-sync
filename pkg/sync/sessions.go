package sync

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"agy-sync/pkg/config"
	"agy-sync/pkg/ssh"
)

func SyncConversationSummaries(host string, port int, clockSkew float64, remHome string) string {
	if _, err := os.Stat(config.SummariesDBFile); err != nil {
		return "skipped"
	}

	remDB := config.SummariesDBFile
	if remHome != "" {
		remDB = config.ToRemotePath(config.SummariesDBFile, remHome)
	}

	pyMergeScript := fmt.Sprintf(`
import sqlite3, tempfile, sys, os, subprocess, json
from datetime import datetime, timezone

local_db = '%s'
rem_db = '%s'
clock_skew = %f

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
    f"{config.PathPrefix} python3 -c \"import sqlite3, tempfile, sys, os; "
    f"p = '{rem_db}'; "
    f"tmp = tempfile.NamedTemporaryFile(suffix='.db', delete=False); "
    f"tmp.close(); "
    f"src = sqlite3.connect(f'file:{{p}}?mode=ro', uri=True); "
    f"dst = sqlite3.connect(tmp.name); "
    f"src.backup(dst); dst.close(); src.close(); "
    f"sys.stdout.buffer.write(open(tmp.name, 'rb').read()); "
    f"os.unlink(tmp.name)\" 2>/dev/null || cat '{rem_db}' 2>/dev/null"
)

ssh_cmd = ["ssh", "-p", "%d", "-o", "ConnectTimeout=8", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR", "-o", "ControlMaster=auto", "-o", "ControlPath=%s/cm-%%C", "-o", "ControlPersist=60s", "%s", remote_snap_cmd]

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
        l_ts = parse_iso(local_rows[cid].get("last_modified_time"))
        r_ts = parse_iso(remote_rows[cid].get("last_modified_time"))
        if l_ts > 0 and r_ts > 0:
            norm_r_ts = r_ts - clock_skew
            if l_ts >= norm_r_ts:
                merged_rows[cid] = local_rows[cid]
                if l_ts > norm_r_ts:
                    remote_changed = True
            else:
                merged_rows[cid] = remote_rows[cid]
                local_changed = True
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
    push_ssh = ["ssh", "-p", "%d", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR", "-o", "ControlMaster=auto", "-o", "ControlPath=%s/cm-%%C", "-o", "ControlPersist=60s", "%s", f"mkdir -p '{rem_dir}' && tmp='{rem_db}.tmp_$$' && {{ cat > \"$tmp\" && mv -f \"$tmp\" '{rem_db}'; }} || (rm -f \"$tmp\" 2>/dev/null; false)"]
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
`, config.SummariesDBFile, remDB, clockSkew, port, config.SocketDir, host, port, config.SocketDir, host)

	cmd := exec.Command("python3", "-c", pyMergeScript)
	out, err := cmd.Output()
	if err == nil && len(out) > 0 {
		return strings.TrimSpace(string(out))
	}
	return "synced"
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

func SyncConversationDBs(host string, port int, clockSkew float64, remHome string) (int, int) {
	_ = os.MkdirAll(config.ConvDir, 0755)
	localDBs := make(map[string][2]int64)

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

	statCmd := fmt.Sprintf("mkdir -p '%s' && stat -c '%%n %%Y %%s' '%s'/*.db 2>/dev/null", remConv, remConv)
	stdout, _, _, _ := ssh.RunSSHCmd(host, port, statCmd, false)
	remoteDBs := make(map[string][2]int64)

	for _, line := range strings.Split(stdout, "\n") {
		parts := strings.Fields(line)
		if len(parts) >= 3 {
			fname := filepath.Base(parts[0])
			if !strings.HasSuffix(fname, ".db") || fname == ".db" || strings.Contains(fname, "*") || strings.Contains(fname, "/") || strings.Contains(fname, "\\") {
				continue
			}
			mtime, _ := strconv.ParseInt(parts[1], 10, 64)
			sz, _ := strconv.ParseInt(parts[2], 10, 64)
			remoteDBs[fname] = [2]int64{mtime - int64(clockSkew), sz}
		}
	}

	allKeys := make(map[string]bool)
	for k := range localDBs {
		allKeys[k] = true
	}
	for k := range remoteDBs {
		allKeys[k] = true
	}

	var toPush, toPull []string
	for k := range allKeys {
		loc, inLoc := localDBs[k]
		rem, inRem := remoteDBs[k]
		if inLoc && !inRem {
			toPush = append(toPush, k)
		} else if inRem && !inLoc {
			toPull = append(toPull, k)
		} else {
			if loc[1] > rem[1] {
				toPush = append(toPush, k)
			} else if rem[1] > loc[1] {
				toPull = append(toPull, k)
			} else if loc[0] > rem[0]+5 {
				toPush = append(toPush, k)
			} else if rem[0] > loc[0]+5 {
				toPull = append(toPull, k)
			}
		}
	}
	sort.Strings(toPush)
	sort.Strings(toPull)

	pushedCount := 0
	if len(toPush) > 0 {
		stagingDir, err := os.MkdirTemp("", "conv_push_*")
		if err == nil {
			defer os.RemoveAll(stagingDir)

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
				if err := RunPipedCommands(p1, p2); err == nil {
					pushedCount = len(validPush)
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

		p2 := exec.Command("tar", "-C", config.ConvDir, "--no-same-owner", "-xzf", "-")
		if err := RunPipedCommands(p1, p2); err == nil {
			pulledCount = len(toPull)
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
			if err := RunPipedCommands(fb1, fb2); err == nil {
				pulledCount = len(toPull)
				for _, d := range toPull {
					_ = os.Remove(filepath.Join(config.ConvDir, d+"-wal"))
					_ = os.Remove(filepath.Join(config.ConvDir, d+"-shm"))
				}
			}
		}
	}

	return pushedCount, pulledCount
}

func SyncAnnotations(host string, port int, clockSkew float64, remHome string) (int, int) {
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

	cmdStr := fmt.Sprintf("mkdir -p '%s' && stat -c '%%n %%Y' '%s'/*.pbtxt 2>/dev/null", remAnn, remAnn)
	stdout, _, _, _ := ssh.RunSSHCmd(host, port, cmdStr, false)
	remoteAnns := make(map[string]int64)

	for _, line := range strings.Split(stdout, "\n") {
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			fname := filepath.Base(parts[0])
			if !strings.HasSuffix(fname, ".pbtxt") || fname == ".pbtxt" || strings.Contains(fname, "*") || strings.Contains(fname, "/") || strings.Contains(fname, "\\") {
				continue
			}
			mtime, _ := strconv.ParseInt(parts[1], 10, 64)
			remoteAnns[fname] = mtime - int64(clockSkew)
		}
	}

	var toPush, toPull []string
	for k, locMTime := range localAnns {
		if remMTime, ok := remoteAnns[k]; !ok || locMTime > remMTime+2 {
			toPush = append(toPush, k)
		}
	}
	for k, remMTime := range remoteAnns {
		if locMTime, ok := localAnns[k]; !ok || remMTime > locMTime+2 {
			toPull = append(toPull, k)
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
		if err := RunPipedCommands(p1, p2); err == nil {
			pushed = len(toPush)
		}
	}

	pulled := 0
	if len(toPull) > 0 {
		inputList := strings.Join(toPull, "\n") + "\n"
		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("tar -C '%s' -czf - -T -", remAnn), true)
		p1 := exec.Command("ssh", sshArgs...)
		p1.Stdin = strings.NewReader(inputList)

		p2 := exec.Command("tar", "-C", config.AnnDir, "--no-same-owner", "-xzf", "-")
		if err := RunPipedCommands(p1, p2); err == nil {
			pulled = len(toPull)
		}
	}

	return pushed, pulled
}

func SyncBrainSessions(host string, port int, clockSkew float64, remHome string, onProgress ...func(string)) (int, int) {
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
	stdout, _, _, _ := ssh.RunSSHCmd(host, port, cmdStr, false)
	remoteBrains := make(map[string][2]int64)

	for _, line := range strings.Split(stdout, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) >= 3 {
			rawUUID := strings.TrimSpace(parts[0])
			cleanUUID := filepath.Base(filepath.Clean(rawUUID))
			if cleanUUID == "" || cleanUUID == "." || cleanUUID == ".." || strings.Contains(cleanUUID, "/") || strings.Contains(cleanUUID, "\\") {
				continue
			}
			mtime, _ := strconv.ParseInt(parts[1], 10, 64)
			sz, _ := strconv.ParseInt(parts[2], 10, 64)
			remoteBrains[cleanUUID] = [2]int64{mtime - int64(clockSkew), sz}
		}
	}

	if len(remoteBrains) == 0 {
		fbCmd := fmt.Sprintf("mkdir -p '%s' && find '%s' -mindepth 1 -maxdepth 1 -type d -exec stat -c '%%n\t%%Y\t0' {} + 2>/dev/null", remBrain, remBrain)
		fbOut, _, _, _ := ssh.RunSSHCmd(host, port, fbCmd, false)
		for _, line := range strings.Split(fbOut, "\n") {
			parts := strings.Split(line, "\t")
			if len(parts) >= 3 {
				rawUUID := filepath.Base(strings.TrimSpace(parts[0]))
				cleanUUID := filepath.Clean(rawUUID)
				if cleanUUID == "" || cleanUUID == "." || cleanUUID == ".." || strings.Contains(cleanUUID, "/") || strings.Contains(cleanUUID, "\\") {
					continue
				}
				mtime, _ := strconv.ParseInt(parts[1], 10, 64)
				remoteBrains[cleanUUID] = [2]int64{mtime - int64(clockSkew), 0}
			}
		}
	}

	allUUIDs := make(map[string]bool)
	for k := range localBrains {
		allUUIDs[k] = true
	}
	for k := range remoteBrains {
		allUUIDs[k] = true
	}

	var toPush, toPull []string
	for k := range allUUIDs {
		loc, inLoc := localBrains[k]
		rem, inRem := remoteBrains[k]
		if inLoc && !inRem {
			toPush = append(toPush, k)
		} else if inRem && !inLoc {
			toPull = append(toPull, k)
		} else {
			if loc[0] > rem[0]+5 {
				toPush = append(toPush, k)
			} else if rem[0] > loc[0]+5 {
				toPull = append(toPull, k)
			} else if loc[1] > rem[1] {
				toPush = append(toPush, k)
			} else if rem[1] > loc[1] {
				toPull = append(toPull, k)
			}
		}
	}
	sort.Strings(toPush)
	sort.Strings(toPull)

	pushedCount := 0
	if len(toPush) > 0 {
		reportProgress(fmt.Sprintf("Brain push (%d session%s)", len(toPush), func() string {
			if len(toPush) > 1 {
				return "s"
			}
			return ""
		}()))
		inputList := strings.Join(toPush, "\n") + "\n"
		tarArgs := []string{
			"--warning=no-file-changed",
			"--exclude=*/scratch/apktool_out*",
			"--exclude=*/scratch/jadx_*",
			"--exclude=*/scratch/mtb_*",
			"--exclude=*/scratch/*.apk",
			"--exclude=*/scratch/*.dex",
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
		if err := RunPipedCommands(p1, p2); err == nil {
			pushedCount = len(toPush)
		}
	}

	pulledCount := 0
	if len(toPull) > 0 {
		reportProgress(fmt.Sprintf("Brain pull (%d session%s)", len(toPull), func() string {
			if len(toPull) > 1 {
				return "s"
			}
			return ""
		}()))
		inputList := strings.Join(toPull, "\n") + "\n"
		remTarCmd := fmt.Sprintf("tar --warning=no-file-changed --exclude='*/scratch/apktool_out*' --exclude='*/scratch/jadx_*' --exclude='*/scratch/mtb_*' --exclude='*/scratch/*.apk' --exclude='*/scratch/*.dex' --exclude='*/scratch/node_modules*' --exclude='*/scratch/build*' --exclude='*/scratch/target*' -C '%s' -czf - -T -", remBrain)
		sshArgs := ssh.GetSSHCmd(host, port, remTarCmd, true)
		p1 := exec.Command("ssh", sshArgs...)
		p1.Stdin = strings.NewReader(inputList)

		p2 := exec.Command("tar", "-C", config.BrainDir, "--no-same-owner", "-xzf", "-")
		if err := RunPipedCommands(p1, p2); err == nil {
			pulledCount = len(toPull)
		}
	}

	return pushedCount, pulledCount
}
