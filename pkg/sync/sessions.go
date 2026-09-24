package sync

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

ssh_cmd = ["ssh", "-p", "%d", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR", "-o", "ControlMaster=auto", "-o", "ControlPath=%s/cm-%%C", "-o", "ControlPersist=60s", "%s", remote_snap_cmd]

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
    conn = sqlite3.connect(db_path)
    cur = conn.cursor()
    cur.execute("PRAGMA journal_mode=WAL")
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
    push_ssh = ["ssh", "-p", "%d", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR", "-o", "ControlMaster=auto", "-o", "ControlPath=%s/cm-%%C", "-o", "ControlPersist=60s", "%s", f"mkdir -p '{rem_dir}' && cat > '{rem_db}.tmp' && mv -f '{rem_db}.tmp' '{rem_db}'"]
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
		pyCmd := fmt.Sprintf(`python3 -c "import sqlite3; s = sqlite3.connect('file:%s?mode=ro', uri=True); d = sqlite3.connect('%s'); s.backup(d); d.close(); s.close()" 2>/dev/null`, src, dst)
		if err := exec.Command("sh", "-c", pyCmd).Run(); err == nil {
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
			}
		}
	}

	pushedCount := 0
	if len(toPush) > 0 {
		stagingDir, err := os.MkdirTemp("", "conv_push_*")
		if err == nil {
			defer os.RemoveAll(stagingDir)

			for _, dbName := range toPush {
				srcFile := filepath.Join(config.ConvDir, dbName)
				dstFile := filepath.Join(stagingDir, dbName)
				// Make safe copy/snapshot
				_ = SafeDBSnapshot(srcFile, dstFile)
				if fi, err := os.Stat(srcFile); err == nil {
					_ = os.Chtimes(dstFile, fi.ModTime(), fi.ModTime())
				}
			}

			tarArgs := append([]string{"-C", stagingDir, "-czf", "-"}, toPush...)
			p1 := exec.Command("tar", tarArgs...)

			var cleanList []string
			for _, d := range toPush {
				cleanList = append(cleanList, fmt.Sprintf("'%s/%s-wal' '%s/%s-shm'", remConv, d, remConv, d))
			}
			cleanCmd := strings.Join(cleanList, " ")
			remoteCmd := fmt.Sprintf("%s mkdir -p '%s' && (tar --no-same-owner --warning=no-timestamp -C '%s' -xzf - 2>/dev/null || tar -C '%s' --no-same-owner -xzf - 2>/dev/null) && rm -f %s 2>/dev/null || true",
				config.PathPrefix, remConv, remConv, remConv, cleanCmd)

			sshArgs := ssh.GetSSHCmd(host, port, remoteCmd, true)
			p2 := exec.Command("ssh", sshArgs...)
			pipe, err := p1.StdoutPipe()
			if err == nil {
				p2.Stdin = pipe
				_ = p1.Start()
				_ = p2.Start()
				_ = p1.Wait()
				if err := p2.Wait(); err == nil {
					pushedCount = len(toPush)
				}
			}
		}
	}

	pulledCount := 0
	if len(toPull) > 0 {
		remotePullPy := fmt.Sprintf(`
import sqlite3, tempfile, shutil, os, sys
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
args_str = " ".join([f'"{x}"' for x in valid])
os.system(f'tar -C {td} -czf - {args_str} 2>/dev/null')
shutil.rmtree(td, ignore_errors=True)
`, remConv)

		b64Code := base64.StdEncoding.EncodeToString([]byte(remotePullPy))
		remoteCmd := fmt.Sprintf(`%s python3 -c "import base64; exec(base64.b64decode('%s').decode())"`, config.PathPrefix, b64Code)

		sshArgs := ssh.GetSSHCmd(host, port, remoteCmd, true)
		p1 := exec.Command("ssh", sshArgs...)
		p1.Stdin = strings.NewReader(strings.Join(toPull, "\n") + "\n")

		p2 := exec.Command("tar", "-C", config.ConvDir, "--no-same-owner", "-xzf", "-")
		pipe, err := p1.StdoutPipe()
		if err == nil {
			p2.Stdin = pipe
			_ = p1.Start()
			_ = p2.Start()
			_ = p1.Wait()
			if err := p2.Wait(); err == nil {
				pulledCount = len(toPull)
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

	pushed := 0
	if len(toPush) > 0 {
		inputList := strings.Join(toPush, "\n") + "\n"
		p1 := exec.Command("tar", "-C", config.AnnDir, "-czf", "-", "-T", "-")
		p1.Stdin = strings.NewReader(inputList)

		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("mkdir -p '%s' && tar -C '%s' --no-same-owner -xzf -", remAnn, remAnn), true)
		p2 := exec.Command("ssh", sshArgs...)
		pipe, err := p1.StdoutPipe()
		if err == nil {
			p2.Stdin = pipe
			_ = p1.Start()
			_ = p2.Start()
			_ = p1.Wait()
			if err := p2.Wait(); err == nil {
				pushed = len(toPush)
			}
		}
	}

	pulled := 0
	if len(toPull) > 0 {
		inputList := strings.Join(toPull, "\n") + "\n"
		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("tar -C '%s' -czf - -T -", remAnn), true)
		p1 := exec.Command("ssh", sshArgs...)
		p1.Stdin = strings.NewReader(inputList)

		p2 := exec.Command("tar", "-C", config.AnnDir, "--no-same-owner", "-xzf", "-")
		pipe, err := p1.StdoutPipe()
		if err == nil {
			p2.Stdin = pipe
			_ = p1.Start()
			_ = p2.Start()
			_ = p1.Wait()
			if err := p2.Wait(); err == nil {
				pulled = len(toPull)
			}
		}
	}

	return pushed, pulled
}

func SyncBrainSessions(host string, port int, clockSkew float64, remHome string) (int, int) {
	_ = os.MkdirAll(config.BrainDir, 0755)
	localBrains := make(map[string][2]int64)

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
			uuid := parts[0]
			mtime, _ := strconv.ParseInt(parts[1], 10, 64)
			sz, _ := strconv.ParseInt(parts[2], 10, 64)
			remoteBrains[uuid] = [2]int64{mtime - int64(clockSkew), sz}
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

	pushedCount := 0
	if len(toPush) > 0 {
		inputList := strings.Join(toPush, "\n") + "\n"
		p1 := exec.Command("tar", "--warning=no-file-changed", "-C", config.BrainDir, "-czf", "-", "-T", "-")
		p1.Stdin = strings.NewReader(inputList)

		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("mkdir -p '%s' && tar -C '%s' --no-same-owner -xzf -", remBrain, remBrain), true)
		p2 := exec.Command("ssh", sshArgs...)
		pipe, err := p1.StdoutPipe()
		if err == nil {
			p2.Stdin = pipe
			_ = p1.Start()
			_ = p2.Start()
			_ = p1.Wait()
			if err := p2.Wait(); err == nil {
				pushedCount = len(toPush)
			}
		}
	}

	pulledCount := 0
	if len(toPull) > 0 {
		inputList := strings.Join(toPull, "\n") + "\n"
		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("tar --warning=no-file-changed -C '%s' -czf - -T -", remBrain), true)
		p1 := exec.Command("ssh", sshArgs...)
		p1.Stdin = strings.NewReader(inputList)

		p2 := exec.Command("tar", "-C", config.BrainDir, "--no-same-owner", "-xzf", "-")
		pipe, err := p1.StdoutPipe()
		if err == nil {
			p2.Stdin = pipe
			_ = p1.Start()
			_ = p2.Start()
			_ = p1.Wait()
			if err := p2.Wait(); err == nil {
				pulledCount = len(toPull)
			}
		}
	}

	return pushedCount, pulledCount
}
