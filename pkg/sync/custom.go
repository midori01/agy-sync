package sync

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"agy-sync/pkg/config"
	"agy-sync/pkg/ssh"
	"agy-sync/pkg/terminal"
)

// SyncCustomSyncRegistry synchronizes the custom_sync.json registry between local and remote peer.
// Returns the active (non-deleted) custom items to synchronize and a status message.
func SyncCustomSyncRegistry(host string, port int, clockSkew float64, remHome string, tracker ...*ByteTracker) ([]*config.CustomItem, string) {
	localReg := config.LoadCustomSyncRegistry()
	remRegPath := config.CustomSyncFile
	if remHome != "" {
		remRegPath = config.ToRemotePath(config.CustomSyncFile, remHome)
	}

	stdout, _, _, _ := ssh.RunSSHCmd(host, port, fmt.Sprintf("cat '%s' 2>/dev/null", remRegPath), false)
	remoteReg := make(config.CustomSyncRegistry)
	if stdout != "" {
		_ = json.Unmarshal([]byte(stdout), &remoteReg)
	}

	merged, localChanged, remoteChanged := config.MergeCustomSyncRegistries(localReg, remoteReg, clockSkew)

	if localChanged || (!fileExists(config.CustomSyncFile) && len(merged) > 0) {
		_ = config.SaveCustomSyncRegistry(merged)
		if len(tracker) > 0 && tracker[0] != nil && len(stdout) > 0 {
			tracker[0].AddPull(int64(len(stdout)))
		}
	}

	if remoteChanged {
		tmpReg := fmt.Sprintf("%s.tmp_%d_%s", remRegPath, time.Now().UnixNano(), randomToken(3))
		remDir := filepath.Dir(remRegPath)
		cmd := fmt.Sprintf("mkdir -p '%s' && (cat > '%s' && mv -f '%s' '%s' || (rm -f '%s' 2>/dev/null; false))", remDir, tmpReg, tmpReg, remRegPath, tmpReg)
		sshArgs := ssh.GetSSHCmd(host, port, cmd, true)
		proc := exec.Command("ssh", sshArgs...)
		data, _ := json.MarshalIndent(merged, "", "  ")
		proc.Stdin = strings.NewReader(string(data) + "\n")
		if err := proc.Run(); err == nil {
			if len(tracker) > 0 && tracker[0] != nil {
				tracker[0].AddPush(int64(len(data)))
			}
		}
	}

	var active []*config.CustomItem
	for _, item := range merged {
		if !item.Deleted {
			active = append(active, item)
		}
	}
	sort.Slice(active, func(i, j int) bool {
		return active[i].Path < active[j].Path
	})

	status := fmt.Sprintf("synchronized (%d items)", len(active))
	if localChanged && remoteChanged {
		status = fmt.Sprintf("merged (%d items)", len(active))
	} else if localChanged {
		status = fmt.Sprintf("pulled (%d items)", len(active))
	} else if remoteChanged {
		status = fmt.Sprintf("pushed (%d items)", len(active))
	}
	return active, status
}

// SyncCustomDirectory performs full bidirectional, parity-checked synchronization of a directory under $HOME.
func SyncCustomDirectory(relPath string, host string, port int, clockSkew float64, remHome string, tracker *ByteTracker, onProgress ...func(string)) (int, int, error) {
	locDir := filepath.Join(config.HomeDir, relPath)
	remDir := fmt.Sprintf("%s/%s", strings.TrimRight(remHome, "/"), relPath)

	reportProgress := func(msg string) {
		if len(onProgress) > 0 && onProgress[0] != nil {
			onProgress[0](msg)
		}
	}

	// 1. Scan local files
	localFiles := make(map[string][2]int64)
	if fi, err := os.Stat(locDir); err == nil && fi.IsDir() {
		_ = filepath.Walk(locDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.IsDir() {
				name := info.Name()
				if name == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			name := info.Name()
			if name == ".DS_Store" || name == "index.lock" || strings.HasSuffix(name, ".swp") || strings.Contains(name, ".tmp") {
				return nil
			}
			if info.Mode()&os.ModeSymlink != 0 {
				target, err := os.Readlink(path)
				if err == nil {
					targetAbs := target
					if !filepath.IsAbs(targetAbs) {
						targetAbs = filepath.Join(filepath.Dir(path), target)
					}
					if !strings.HasPrefix(filepath.Clean(targetAbs), locDir) {
						return nil
					}
				}
			}
			subRel, err := filepath.Rel(locDir, path)
			if err == nil {
				cleanSub := filepath.ToSlash(filepath.Clean(subRel))
				if cleanSub != "." && !strings.HasPrefix(cleanSub, "..") {
					localFiles[cleanSub] = [2]int64{info.ModTime().Unix(), info.Size()}
				}
			}
			return nil
		})
	}

	// 2. Scan remote files
	remoteFiles := make(map[string][2]int64)
	scanCmd := fmt.Sprintf("if [ -d '%s' ]; then if stat -c '%%n' /dev/null >/dev/null 2>&1; then find '%s' -name .git -prune -o -type f -exec stat -c '%%n\t%%Y\t%%s' {} + 2>/dev/null; else find '%s' -name .git -prune -o -type f -exec stat -f '%%N\t%%m\t%%z' {} + 2>/dev/null; fi; else echo NOT_DIR; fi", remDir, remDir, remDir)
	stdout, _, _, err := ssh.RunSSHCmd(host, port, scanCmd, false)
	if err == nil && !strings.Contains(stdout, "NOT_DIR") {
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

				subRel := fullPath
				if strings.HasPrefix(fullPath, remDir) {
					subRel = strings.TrimLeft(strings.TrimPrefix(fullPath, remDir), "/")
				}
				cleanSub := filepath.ToSlash(filepath.Clean(subRel))
				if cleanSub != "." && !strings.HasPrefix(cleanSub, "..") && !filepath.IsAbs(cleanSub) {
					if strings.HasPrefix(cleanSub, ".git/") || cleanSub == ".git" || strings.Contains(cleanSub, "/.git/") {
						continue
					}
					baseName := filepath.Base(cleanSub)
					if baseName == ".DS_Store" || baseName == "index.lock" || strings.HasSuffix(baseName, ".swp") || strings.Contains(baseName, ".tmp") {
						continue
					}
					remoteFiles[cleanSub] = [2]int64{mtime, sz}
				}
			}
		}
	}

	if len(localFiles) == 0 && len(remoteFiles) == 0 {
		return 0, 0, nil
	}

	// 3. Parity comparison
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
		reportProgress(fmt.Sprintf("Custom ~/%s ↑ [%s/%s (0%%)]", relPath, terminal.FormatBytes(0, true), terminal.FormatBytes(totalPushBytes, true)))
		inputList := strings.Join(toPush, "\n") + "\n"
		p1 := exec.Command("tar", "-C", locDir, "-czf", "-", "-T", "-")
		p1.Stdin = strings.NewReader(inputList)

		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("mkdir -p '%s' && tar -C '%s' --no-same-owner -xzf -", remDir, remDir), true)
		p2 := exec.Command("ssh", sshArgs...)

		nBytes, err := RunPipedCommandsWithBytes(p1, p2, func(cur int64) {
			pct := 0.0
			if totalPushBytes > 0 {
				pct = (float64(cur) / float64(totalPushBytes)) * 100.0
				if pct > 100.0 {
					pct = 100.0
				}
			}
			reportProgress(fmt.Sprintf("Custom ~/%s ↑ [%s/%s (%.0f%%)]", relPath, terminal.FormatBytes(cur, true), terminal.FormatBytes(totalPushBytes, true), pct))
		})
		if err == nil {
			pushedCnt = len(toPush)
			if tracker != nil {
				tracker.AddPush(nBytes)
			}
		} else {
			return pushedCnt, 0, err
		}
	}

	pulledCnt := 0
	if len(toPull) > 0 {
		reportProgress(fmt.Sprintf("Custom ~/%s ↓ [%s/%s (0%%)]", relPath, terminal.FormatBytes(0, true), terminal.FormatBytes(totalPullBytes, true)))
		inputList := strings.Join(toPull, "\n") + "\n"
		sshArgs := ssh.GetSSHCmd(host, port, fmt.Sprintf("tar -C '%s' -czf - -T -", remDir), true)
		p1 := exec.Command("ssh", sshArgs...)
		p1.Stdin = strings.NewReader(inputList)

		_ = os.MkdirAll(locDir, 0755)
		p2 := exec.Command("tar", "-C", locDir, "--no-same-owner", "-xzf", "-")

		nBytes, err := RunPipedCommandsWithBytes(p1, p2, func(cur int64) {
			pct := 0.0
			if totalPullBytes > 0 {
				pct = (float64(cur) / float64(totalPullBytes)) * 100.0
				if pct > 100.0 {
					pct = 100.0
				}
			}
			reportProgress(fmt.Sprintf("Custom ~/%s ↓ [%s/%s (%.0f%%)]", relPath, terminal.FormatBytes(cur, true), terminal.FormatBytes(totalPullBytes, true), pct))
		})
		if err == nil {
			pulledCnt = len(toPull)
			if tracker != nil {
				tracker.AddPull(nBytes)
			}
		} else {
			return pushedCnt, pulledCnt, err
		}
	}

	return pushedCnt, pulledCnt, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// CmdCustom handles custom sync CLI commands (list, add, remove, clear).
func CmdCustom(args []string) {
	if len(args) == 0 || args[0] == "list" || args[0] == "ls" {
		ShowCustomItems()
		return
	}

	action := strings.ToLower(args[0])
	switch action {
	case "add":
		if len(args) < 2 {
			fmt.Println("Usage: agy-sync custom add <path_under_home>")
			fmt.Println("       agy-sync add <path_under_home>")
			os.Exit(1)
		}
		targetPath := args[1]
		relPath, isDir, err := config.NormalizeCustomPath(targetPath)
		if err != nil {
			fmt.Printf("[❌] %v\n", err)
			os.Exit(1)
		}

		reg := config.LoadCustomSyncRegistry()
		itemType := "file"
		if isDir {
			itemType = "dir"
		}

		locPath := filepath.Join(config.HomeDir, relPath)
		statusInfo := "not created yet locally"
		if fi, err := os.Stat(locPath); err == nil {
			if fi.IsDir() {
				entries, _ := os.ReadDir(locPath)
				statusInfo = fmt.Sprintf("directory with %d item(s)", len(entries))
			} else {
				statusInfo = fmt.Sprintf("file, %s", terminal.FormatBytes(fi.Size()))
			}
		}

		now := float64(time.Now().UnixNano()) / 1e9
		reg[relPath] = &config.CustomItem{
			Path:      relPath,
			Type:      itemType,
			Deleted:   false,
			UpdatedAt: now,
		}

		if err := config.SaveCustomSyncRegistry(reg); err != nil {
			fmt.Printf("[❌] Failed to save custom sync configuration: %v\n", err)
			os.Exit(1)
		}

		typeLabel := "File"
		if isDir {
			typeLabel = "Directory"
		}
		fmt.Printf("[✅] Registered custom sync %s: ~/%s\n", typeLabel, relPath)
		fmt.Printf("     Status: %s\n", statusInfo)
		fmt.Printf("     Sync target: Across all cluster devices at ~/%s\n", relPath)

	case "rm", "remove", "del", "delete":
		if len(args) < 2 {
			fmt.Println("Usage: agy-sync custom remove <path_under_home>")
			fmt.Println("       agy-sync rm <path_under_home>")
			os.Exit(1)
		}
		targetPath := args[1]
		reg := config.LoadCustomSyncRegistry()

		// Match either normalized relative path, or direct key match
		matchedKey := ""
		if _, ok := reg[targetPath]; ok {
			matchedKey = targetPath
		} else if rel, _, err := config.NormalizeCustomPath(targetPath); err == nil {
			if _, ok := reg[rel]; ok {
				matchedKey = rel
			}
		}

		if matchedKey == "" {
			// Fuzzy match without leading ~ or /
			cleanKey := strings.TrimPrefix(strings.TrimPrefix(targetPath, "~"), "/")
			if _, ok := reg[cleanKey]; ok {
				matchedKey = cleanKey
			}
		}

		if matchedKey == "" || (reg[matchedKey] != nil && reg[matchedKey].Deleted) {
			fmt.Printf("[⚠️] Custom path '%s' not found in active sync list.\n", targetPath)
			os.Exit(1)
		}

		now := float64(time.Now().UnixNano()) / 1e9
		reg[matchedKey].Deleted = true
		reg[matchedKey].UpdatedAt = now

		if err := config.SaveCustomSyncRegistry(reg); err != nil {
			fmt.Printf("[❌] Failed to save custom sync registry: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("[✅] Removed '~/%s' from custom sync list.\n", matchedKey)

	case "clear":
		reg := config.LoadCustomSyncRegistry()
		now := float64(time.Now().UnixNano()) / 1e9
		cleared := 0
		for _, item := range reg {
			if !item.Deleted {
				item.Deleted = true
				item.UpdatedAt = now
				cleared++
			}
		}
		_ = config.SaveCustomSyncRegistry(reg)
		fmt.Printf("[✅] Cleared %d custom sync item(s).\n", cleared)

	default:
		fmt.Printf("[❌] Unknown custom sync command '%s'. Use: add, remove, list, or clear.\n", action)
		os.Exit(1)
	}
}

// ShowCustomItems displays a Bento dashboard table of currently registered custom sync items.
func ShowCustomItems() {
	reg := config.LoadCustomSyncRegistry()
	var active []*config.CustomItem
	for _, item := range reg {
		if !item.Deleted {
			active = append(active, item)
		}
	}

	if len(active) == 0 {
		fmt.Println("\n[ℹ️] No custom sync files or directories registered yet.")
		fmt.Println("    Add a file or directory under $HOME with:")
		fmt.Println("      agy-sync custom add <file_or_dir>")
		fmt.Println("      agy-sync add <file_or_dir>")
		fmt.Println("    Example:")
		fmt.Println("      agy-sync custom add ~/.bashrc")
		fmt.Println("      agy-sync custom add ~/scripts")
		fmt.Println()
		return
	}

	sort.Slice(active, func(i, j int) bool {
		return active[i].Path < active[j].Path
	})

	colWidths := []int{8, 30, 16, 12}
	termW := terminal.GetTerminalWidth()
	if termW > 0 && termW < 74 {
		if termW < 60 {
			colWidths = []int{6, 20, 13, 10}
		} else {
			colWidths = []int{7, 24, 15, 11}
		}
	}
	totW := 0
	for _, w := range colWidths {
		totW += w
	}
	totW += len(colWidths)*2 + (len(colWidths) + 1)
	innerW := totW - 4

	title := fmt.Sprintf("Custom Synchronized Items · %d Tracked", len(active))
	if terminal.StrWidth("["+title+"]") > totW-8 {
		title = terminal.TruncateWidth(title, totW-8)
	}
	barLen := totW - 5 - terminal.StrWidth("["+title+"]")
	if barLen < 2 {
		barLen = 2
	}
	top := fmt.Sprintf("┌─ [%s] %s┐", title, strings.Repeat("─", barLen))
	headers := []string{"Type", "Target (~/ relative)", "Local Status", "Updated"}

	var headerCells []string
	for i, h := range headers {
		headerCells = append(headerCells, terminal.PadTo(h, colWidths[i]))
	}
	headerRow := fmt.Sprintf("│ %s │", strings.Join(headerCells, " │ "))

	var lines []string
	lines = append(lines, top, headerRow, terminal.MakeTableDivider(colWidths))

	for _, item := range active {
		typeStr := "📄 File"
		if item.Type == "dir" {
			typeStr = "📁 Dir"
		}

		pathDisp := fmt.Sprintf("~/%s", item.Path)
		pathDisp = terminal.CompactPath(pathDisp, colWidths[1])

		locPath := filepath.Join(config.HomeDir, item.Path)
		statusStr := "✗ Missing"
		if fi, err := os.Stat(locPath); err == nil {
			if fi.IsDir() {
				if entries, err := os.ReadDir(locPath); err == nil {
					statusStr = fmt.Sprintf("✓ %d items", len(entries))
				} else {
					statusStr = "✓ In Place"
				}
			} else {
				statusStr = fmt.Sprintf("✓ %s", terminal.FormatBytes(fi.Size(), true))
			}
		}

		updStr := "—"
		if item.UpdatedAt > 0 {
			t := time.Unix(int64(item.UpdatedAt), 0)
			updStr = t.Format("01-02 15:04")
		}

		row := []string{typeStr, pathDisp, statusStr, updStr}
		var rowCells []string
		for i, v := range row {
			rowCells = append(rowCells, terminal.PadTo(v, colWidths[i]))
		}
		lines = append(lines, fmt.Sprintf("│ %s │", strings.Join(rowCells, " │ ")))
	}

	lines = append(lines, terminal.MakeTableDivider(colWidths, "├", "┴", "┤"))
	helpNote := "Add: agy-sync custom add <path> · Remove: agy-sync custom rm <path>"
	lines = append(lines, fmt.Sprintf("│ %s │", terminal.PadTo(helpNote, innerW)))
	lines = append(lines, fmt.Sprintf("└%s┘", strings.Repeat("─", totW-2)))

	fmt.Printf("\n%s\n\n", strings.Join(lines, "\n"))
}
