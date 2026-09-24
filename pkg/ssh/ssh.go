package ssh

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"agy-sync/pkg/config"
	"agy-sync/pkg/network"
)

var safeHostCharRegex = regexp.MustCompile(`[^a-zA-Z0-9_\-\.]`)

func GetSSHCmd(host string, port int, cmd string, batch bool, connectTimeout ...int) []string {
	to := 8
	if len(connectTimeout) > 0 && connectTimeout[0] > 0 {
		to = connectTimeout[0]
	}

	args := []string{
		"-p", strconv.Itoa(port),
		"-o", fmt.Sprintf("ConnectTimeout=%d", to),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ServerAliveInterval=10",
		"-o", "ServerAliveCountMax=3",
		"-o", "TCPKeepAlive=yes",
		"-o", "ControlMaster=auto",
		"-o", fmt.Sprintf("ControlPath=%s/cm-%%C", config.SocketDir),
		"-o", "ControlPersist=60s",
	}

	if batch {
		args = append(args, "-o", "BatchMode=yes")
	}

	args = append(args, host)
	if cmd != "" {
		args = append(args, cmd)
	}

	return args
}

func RunSSHCmd(host string, port int, cmd string, check bool, connectTimeout ...int) (stdout, stderr string, exitCode int, err error) {
	sshArgs := GetSSHCmd(host, port, cmd, true, connectTimeout...)
	proc := exec.Command("ssh", sshArgs...)

	var outBuf, errBuf bytes.Buffer
	proc.Stdout = &outBuf
	proc.Stderr = &errBuf

	err = proc.Run()
	stdout = outBuf.String()
	stderr = errBuf.String()

	if proc.ProcessState != nil {
		exitCode = proc.ProcessState.ExitCode()
	}

	if err != nil && check {
		err = fmt.Errorf("ssh command failed with exit code %d: %s (%s)", exitCode, stderr, err)
	}
	return
}

// RunSSHCmdWithStdin runs a remote command over SSH while streaming input from stdin.
func RunSSHCmdWithStdin(host string, port int, cmd string, stdin io.Reader, check bool, connectTimeout ...int) (stdout, stderr string, exitCode int, err error) {
	sshArgs := GetSSHCmd(host, port, cmd, true, connectTimeout...)
	proc := exec.Command("ssh", sshArgs...)
	proc.Stdin = stdin

	var outBuf, errBuf bytes.Buffer
	proc.Stdout = &outBuf
	proc.Stderr = &errBuf

	err = proc.Run()
	stdout = outBuf.String()
	stderr = errBuf.String()

	if proc.ProcessState != nil {
		exitCode = proc.ProcessState.ExitCode()
	}

	if err != nil && check {
		err = fmt.Errorf("ssh command failed with exit code %d: %s (%s)", exitCode, stderr, err)
	}
	return
}

// ShellEscape returns a single-quote wrapped string safely escaped for POSIX shells.
func ShellEscape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// CleanupStaleControlSockets checks all master sockets in SocketDir and removes dead ones.
func CleanupStaleControlSockets() {
	entries, err := os.ReadDir(config.SocketDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "cm-") {
			sockPath := filepath.Join(config.SocketDir, e.Name())
			conn, err := net.DialTimeout("unix", sockPath, 100*time.Millisecond)
			if err != nil {
				// Socket is not responding or connection refused: remove stale file
				_ = os.Remove(sockPath)
			} else {
				_ = conn.Close()
			}
		}
	}
}

func ReconcileSSHConfig(reg ...config.Registry) {
	var registry config.Registry
	if len(reg) > 0 && reg[0] != nil {
		registry = reg[0]
	} else {
		registry = config.LoadRegistry()
	}

	var entries []string
	assignedSSHHosts := make(map[string]bool)

	for ep, data := range registry {
		if data.Deleted {
			continue
		}
		user, host, port, _ := config.ParseEndpoint(ep, 8022)
		if config.IsLocalEndpoint(ep, data) {
			continue
		}

		alias := strings.ToLower(strings.TrimSpace(data.Alias))
		native := strings.ToLower(strings.TrimSpace(data.NativeName))
		effectiveUser := user
		if effectiveUser == "" {
			effectiveUser = data.User
		}

		var hostAliases []string
		for _, a := range []string{alias, native} {
			if a != "" && config.SafeAliasRegex.MatchString(a) {
				if !assignedSSHHosts[a] {
					hostAliases = append(hostAliases, a)
					assignedSSHHosts[a] = true
				} else {
					inList := false
					for _, ha := range hostAliases {
						if ha == a {
							inList = true
							break
						}
					}
					if !inList {
						idx := 2
						for assignedSSHHosts[fmt.Sprintf("%s-%d", a, idx)] {
							idx++
						}
						disA := fmt.Sprintf("%s-%d", a, idx)
						hostAliases = append(hostAliases, disA)
						assignedSSHHosts[disA] = true
					}
				}
			}
		}

		if len(hostAliases) == 0 {
			cleanHost := safeHostCharRegex.ReplaceAllString(host, "")
			if cleanHost != "" {
				hostAliases = []string{cleanHost}
			}
		}

		if len(hostAliases) == 0 {
			continue
		}

		var entryBlocks []string
		seenLan := make(map[string]bool)
		for _, lanIP := range data.LanIps {
			lanIP = strings.TrimSpace(lanIP)
			if lanIP != "" && lanIP != host && lanIP != "127.0.0.1" && lanIP != "localhost" && lanIP != "0.0.0.0" && !seenLan[lanIP] {
				seenLan[lanIP] = true
				matchLines := []string{
					fmt.Sprintf("Match host %s exec \"agy-sync probe-lan %%h %s %d 2>/dev/null || $HOME/.gemini/antigravity-cli/bin/agy-sync probe-lan %%h %s %d 2>/dev/null\"", strings.Join(hostAliases, ","), lanIP, port, lanIP, port),
					fmt.Sprintf("  HostName %s", lanIP),
					fmt.Sprintf("  Port %d", port),
				}
				if effectiveUser != "" {
					cleanUser := safeHostCharRegex.ReplaceAllString(effectiveUser, "")
					if cleanUser != "" {
						matchLines = append(matchLines, fmt.Sprintf("  User %s", cleanUser))
					}
				}
				matchLines = append(matchLines,
					"  StrictHostKeyChecking no",
					"  UserKnownHostsFile /dev/null",
					"  LogLevel ERROR",
					"  ControlMaster auto",
					fmt.Sprintf("  ControlPath %s/cm-%%C", config.SocketDir),
					"  ControlPersist 60s",
				)
				entryBlocks = append(entryBlocks, strings.Join(matchLines, "\n"))
			}
		}

		entryLines := []string{
			fmt.Sprintf("Host %s", strings.Join(hostAliases, " ")),
			fmt.Sprintf("  HostName %s", host),
			fmt.Sprintf("  Port %d", port),
		}
		if effectiveUser != "" {
			cleanUser := safeHostCharRegex.ReplaceAllString(effectiveUser, "")
			if cleanUser != "" {
				entryLines = append(entryLines, fmt.Sprintf("  User %s", cleanUser))
			}
		}
		entryLines = append(entryLines,
			"  StrictHostKeyChecking no",
			"  UserKnownHostsFile /dev/null",
			"  LogLevel ERROR",
			"  ControlMaster auto",
			fmt.Sprintf("  ControlPath %s/cm-%%C", config.SocketDir),
			"  ControlPersist 60s",
		)
		entryBlocks = append(entryBlocks, strings.Join(entryLines, "\n"))

		entries = append(entries, strings.Join(entryBlocks, "\n\n"))
	}

	sort.Strings(entries)

	managedContent := ""
	if len(entries) > 0 {
		managedContent = fmt.Sprintf("%s\n%s\n%s", config.SSHConfigMarkerStart, strings.Join(entries, "\n\n"), config.SSHConfigMarkerEnd)
	}

	_ = os.MkdirAll(filepath.Dir(config.SSHConfigFile), 0700)
	existingContent := ""
	if data, err := os.ReadFile(config.SSHConfigFile); err == nil {
		existingContent = string(data)
	}

	before := ""
	after := existingContent
	if strings.Contains(existingContent, config.SSHConfigMarkerStart) && strings.Contains(existingContent, config.SSHConfigMarkerEnd) {
		parts1 := strings.Split(existingContent, config.SSHConfigMarkerStart)
		before = strings.TrimSpace(parts1[0])
		parts2 := strings.Split(parts1[1], config.SSHConfigMarkerEnd)
		after = strings.TrimSpace(parts2[1])
	}

	// Ensure Termux environments have Host * with User root for unmanaged external targets
	if fi, err := os.Stat("/data/data/com.termux"); err == nil && fi.IsDir() {
		if after == "" {
			after = "Host *\n  User root\n  ServerAliveInterval 15\n  ServerAliveCountMax 3\n  TCPKeepAlive yes"
		} else if strings.Contains(after, "Host *") {
			// Check if the Host * block already contains a User directive
			hasUser := false
			idx := strings.Index(after, "Host *")
			sub := after[idx+len("Host *"):]
			for _, l := range strings.Split(sub, "\n") {
				tl := strings.TrimSpace(l)
				if strings.HasPrefix(tl, "Host ") {
					break
				}
				if strings.HasPrefix(tl, "User ") {
					hasUser = true
					break
				}
			}
			if !hasUser {
				lines := strings.Split(after, "\n")
				var newLines []string
				inserted := false
				for _, l := range lines {
					newLines = append(newLines, l)
					if !inserted && strings.TrimSpace(l) == "Host *" {
						newLines = append(newLines, "  User root")
						inserted = true
					}
				}
				after = strings.Join(newLines, "\n")
			}
		} else if !strings.Contains(after, "Host *") {
			after = strings.TrimRight(after, "\n") + "\n\nHost *\n  User root\n  ServerAliveInterval 15\n  ServerAliveCountMax 3\n  TCPKeepAlive yes"
		}
	}

	var parts []string
	if before != "" {
		parts = append(parts, before)
	}
	if managedContent != "" {
		parts = append(parts, managedContent)
	}
	if after != "" {
		parts = append(parts, after)
	}

	newContent := ""
	if len(parts) > 0 {
		newContent = strings.Join(parts, "\n\n") + "\n"
	}

	if newContent != existingContent {
		tmpFile := config.SSHConfigFile + ".tmp"
		if err := os.WriteFile(tmpFile, []byte(newContent), 0600); err == nil {
			_ = os.Rename(tmpFile, config.SSHConfigFile)
		}
	}
}

func ReconcileShellIntegration(reg ...config.Registry) {
	var registry config.Registry
	if len(reg) > 0 && reg[0] != nil {
		registry = reg[0]
	} else {
		registry = config.LoadRegistry()
	}

	activePeers := make(map[string]bool)
	for ep, data := range registry {
		if data.Deleted {
			continue
		}
		_, host, _, _ := config.ParseEndpoint(ep, 8022)
		if host == "127.0.0.1" || host == "localhost" || host == "0.0.0.0" || host == "::1" {
			continue
		}
		alias := strings.ToLower(strings.TrimSpace(data.Alias))
		native := strings.ToLower(strings.TrimSpace(data.NativeName))
		if alias != "" && config.SafeAliasRegex.MatchString(alias) {
			activePeers[alias] = true
		}
		if native != "" && config.SafeAliasRegex.MatchString(native) {
			activePeers[native] = true
		}
		cleanHost := strings.ToLower(safeHostCharRegex.ReplaceAllString(host, ""))
		if cleanHost != "" && cleanHost != "127.0.0.1" && cleanHost != "localhost" {
			activePeers[cleanHost] = true
		}
		for _, lanIP := range data.LanIps {
			lip := strings.ToLower(strings.TrimSpace(safeHostCharRegex.ReplaceAllString(lanIP, "")))
			if lip != "" && !strings.HasPrefix(lip, "127.") && lip != "0.0.0.0" {
				activePeers[lip] = true
			}
		}
	}

	cfg := config.LoadConfig()
	if devSec, ok := cfg["devices"]; ok {
		for name := range devSec {
			n := strings.ToLower(strings.TrimSpace(name))
			if n != "" && config.SafeAliasRegex.MatchString(n) {
				activePeers[n] = true
			}
		}
	}

	var peerList []string
	for p := range activePeers {
		peerList = append(peerList, p)
	}
	sort.Strings(peerList)
	peersLine := strings.Join(peerList, " ")

	_ = os.MkdirAll(filepath.Dir(config.PeersListFile), 0755)
	tmpPeers := config.PeersListFile + ".tmp"
	if err := os.WriteFile(tmpPeers, []byte(peersLine+"\n"), 0644); err == nil {
		_ = os.Rename(tmpPeers, config.PeersListFile)
	}

	shellBlock := fmt.Sprintf(`%s
ssh() {
    local _target=""
    local _skip=0
    for _arg in "$@"; do
        if [ "$_skip" -eq 1 ]; then
            _skip=0
            continue
        fi
        case "$_arg" in
            --) ;;
            -b|-c|-D|-E|-e|-F|-I|-i|-J|-L|-l|-m|-O|-o|-p|-Q|-R|-S|-W|-w) _skip=1 ;;
            -*) ;;
            *) _target="$_arg"; break ;;
        esac
    done

    if [ -n "$_target" ]; then
        local _clean="${_target##*@}"
        _clean="${_clean%%:*}"
        if [ -n "$BASH_VERSION" ]; then
            _clean="${_clean,,}"
        else
            _clean=$(printf "%%s" "$_clean" | tr "[:upper:]" "[:lower:]")
        fi
        if [ -n "$_clean" ]; then
            local _peers_file="$HOME/.gemini/antigravity-cli/peers.list"
            local _is_match=0
            if [ -f "$_peers_file" ]; then
                local _peers=""
                read -r _peers < "$_peers_file" 2>/dev/null
                [[ " $_peers " == *" $_clean "* ]] && _is_match=1
            fi
            if [ "$_is_match" -eq 0 ] && [ ! -f "$_peers_file" ] && command -v agy-sync >/dev/null 2>&1; then
                agy-sync is-peer "$_clean" >/dev/null 2>&1 && _is_match=1
            fi
            if [ "$_is_match" -eq 1 ]; then
                if command -v agy-sync >/dev/null 2>&1; then
                    agy-sync ssh "$@"
                elif [ -x "$HOME/.gemini/antigravity-cli/bin/agy-sync" ]; then
                    "$HOME/.gemini/antigravity-cli/bin/agy-sync" ssh "$@"
                else
                    command ssh "$@"
                fi
                return $?
            fi
        fi
    fi

    command ssh "$@"
}
%s`, config.ShellPeerMarkerStart, config.ShellPeerMarkerEnd)

	rcTargets := []string{filepath.Join(config.HomeDir, ".bashrc")}
	zshrc := filepath.Join(config.HomeDir, ".zshrc")
	if fi, err := os.Stat(zshrc); err == nil && !fi.IsDir() {
		rcTargets = append(rcTargets, zshrc)
	}

	for _, rcFile := range rcTargets {
		existing := ""
		if data, err := os.ReadFile(rcFile); err == nil {
			existing = string(data)
		}

		newContent := ""
		if strings.Contains(existing, config.ShellPeerMarkerStart) && strings.Contains(existing, config.ShellPeerMarkerEnd) {
			p1 := strings.Split(existing, config.ShellPeerMarkerStart)
			before := strings.TrimRight(p1[0], "\n")
			p2 := strings.Split(p1[1], config.ShellPeerMarkerEnd)
			after := strings.TrimLeft(p2[1], "\n")
			var parts []string
			if before != "" {
				parts = append(parts, before)
			}
			parts = append(parts, shellBlock)
			if after != "" {
				parts = append(parts, after)
			}
			newContent = strings.Join(parts, "\n\n") + "\n"
		} else {
			if strings.TrimSpace(existing) != "" {
				newContent = strings.TrimRight(existing, "\n") + "\n\n" + shellBlock + "\n"
			} else {
				newContent = shellBlock + "\n"
			}
		}

		if newContent != existing {
			tmpRc := rcFile + ".tmp"
			mode := os.FileMode(0644)
			if fi, err := os.Stat(rcFile); err == nil {
				mode = fi.Mode()
			}
			if err := os.WriteFile(tmpRc, []byte(newContent), mode); err == nil {
				_ = os.Rename(tmpRc, rcFile)
			}
		}
	}
}

func CmdIsPeer(target string) int {
	clean := strings.ToLower(strings.TrimSpace(target))
	if strings.Contains(clean, "@") {
		clean = clean[strings.LastIndex(clean, "@")+1:]
	}
	if strings.Contains(clean, ":") {
		clean = clean[:strings.Index(clean, ":")]
	}
	if clean == "" {
		return 1
	}

	// 1. Fast path: check peers.list
	if data, err := os.ReadFile(config.PeersListFile); err == nil {
		fields := strings.Fields(string(data))
		for _, f := range fields {
			if strings.EqualFold(f, clean) {
				return 0
			}
		}
	}

	// 2. Slow path: check registry
	registry := config.LoadRegistry()
	for ep, data := range registry {
		if data.Deleted {
			continue
		}
		if strings.EqualFold(data.Alias, clean) || strings.EqualFold(data.NativeName, clean) {
			return 0
		}
		_, h, _, _ := config.ParseEndpoint(ep, 8022)
		if strings.EqualFold(h, clean) {
			return 0
		}
		for _, lip := range data.LanIps {
			if strings.EqualFold(lip, clean) {
				return 0
			}
		}
	}

	cfg := config.LoadConfig()
	if devSec, ok := cfg["devices"]; ok {
		for name, ep := range devSec {
			if strings.EqualFold(name, clean) {
				return 0
			}
			_, h, _, _ := config.ParseEndpoint(ep, 8022)
			if strings.EqualFold(h, clean) {
				return 0
			}
		}
	}

	return 1
}

func FindEndpoint(target string) (endpoint string, matchedAlias string) {
	registry := config.LoadRegistry()
	t := strings.ToLower(strings.TrimSpace(target))

	for ep, d := range registry {
		if d.Deleted {
			continue
		}
		if strings.EqualFold(d.Alias, t) {
			return ep, d.Alias
		}
		if strings.EqualFold(d.NativeName, t) {
			return ep, d.NativeName
		}
	}

	for ep, d := range registry {
		if d.Deleted {
			continue
		}
		_, h, p, _ := config.ParseEndpoint(ep, 8022)
		if strings.EqualFold(h, t) || strings.EqualFold(ep, t) || strings.EqualFold(fmt.Sprintf("%s:%d", h, p), t) {
			return ep, d.Alias
		}
		for _, lip := range d.LanIps {
			if strings.EqualFold(lip, t) {
				return ep, d.Alias
			}
		}
	}

	cfg := config.LoadConfig()
	if devSec, ok := cfg["devices"]; ok {
		for name, ep := range devSec {
			if strings.EqualFold(name, t) {
				return ep, name
			}
			_, h, p, _ := config.ParseEndpoint(ep, 8022)
			if strings.EqualFold(h, t) || strings.EqualFold(ep, t) || strings.EqualFold(fmt.Sprintf("%s:%d", h, p), t) {
				return ep, name
			}
		}
	}

	return "", ""
}

func HandleSSHCommand(rawArgs []string) {
	optTakesArg := map[string]bool{
		"-b": true, "-c": true, "-D": true, "-E": true, "-e": true, "-F": true,
		"-I": true, "-i": true, "-J": true, "-L": true, "-l": true, "-m": true,
		"-O": true, "-o": true, "-p": true, "-Q": true, "-R": true, "-S": true,
		"-W": true, "-w": true,
	}

	target := ""
	targetIdx := -1
	skipNext := false

	for idx, arg := range rawArgs {
		if skipNext {
			skipNext = false
			continue
		}
		if arg == "--" {
			if idx+1 < len(rawArgs) {
				target = rawArgs[idx+1]
				targetIdx = idx + 1
			}
			break
		}
		if optTakesArg[arg] {
			skipNext = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		target = arg
		targetIdx = idx
		break
	}

	var preArgs, postArgs []string
	if target != "" && targetIdx >= 0 {
		preArgs = rawArgs[:targetIdx]
		if targetIdx > 0 && rawArgs[targetIdx-1] == "--" {
			preArgs = rawArgs[:targetIdx-1]
		}
		postArgs = rawArgs[targetIdx+1:]
	} else {
		cfg := config.LoadConfig()
		defAlias := ""
		if syncSec, ok := cfg["sync"]; ok {
			defAlias = syncSec["default"]
		}
		target = defAlias
		preArgs = rawArgs
		postArgs = nil
	}

	if target == "" {
		fmt.Println("[❌] Usage: agy-sync ssh [alias|endpoint] [extra ssh args...]")
		os.Exit(1)
	}

	ep, matchedAlias := FindEndpoint(target)
	registry := config.LoadRegistry()
	var effectiveUser, host string
	port := 8022
	nodeName := target

	if ep != "" {
		if config.IsLocalEndpoint(ep, registry[ep]) {
			fmt.Printf("[⚠️] Cannot SSH to local device '%s'.\n", matchedAlias)
			os.Exit(1)
		}
		u, h, p, _ := config.ParseEndpoint(ep, 8022)
		user := u
		if user == "" && registry[ep] != nil {
			user = registry[ep].User
		}
		effectiveUser = user
		host = h
		port = p
		nodeName = matchedAlias
		if nodeName == "" {
			nodeName = target
		}
	} else {
		if strings.Contains(target, ":") {
			u, h, p, _ := config.ParseEndpoint(target, 8022)
			if config.IsLocalTarget(target, p, config.GetLocalEndpoint(), config.GetLocalDeviceName()) {
				fmt.Printf("[⚠️] Cannot SSH to local device '%s'.\n", target)
				os.Exit(1)
			}
			effectiveUser = u
			host = h
			port = p
			nodeName = h
		} else {
			fmt.Printf("[❌] Device '%s' not found in cluster registry.\n", target)
			os.Exit(1)
		}
	}

	effH, effP, rTier, rLat := network.ResolveFastestEndpoint(host, port, nodeName)
	effectiveTarget := effH
	if !strings.Contains(effH, "@") && effectiveUser != "" {
		effectiveTarget = fmt.Sprintf("%s@%s", effectiveUser, effH)
	}
	effectivePort := effP

	dispTitle := fmt.Sprintf("%s (%s -> %s:%d, %.1fms)", nodeName, rTier, effectiveTarget, effectivePort, rLat)
	if !strings.Contains(rTier, "LAN") {
		dispTitle = fmt.Sprintf("%s (%s:%d)", nodeName, effectiveTarget, effectivePort)
	}
	symbol := "🚀"
	if strings.Contains(rTier, "LAN") {
		symbol = "⚡"
	}
	fmt.Printf("[%s] Connecting to peer node %s...\n", symbol, dispTitle)

	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		sshBin = "ssh"
	}

	sshArgs := []string{"ssh"}
	sshArgs = append(sshArgs, preArgs...)
	sshArgs = append(sshArgs,
		"-p", strconv.Itoa(effectivePort),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ControlMaster=auto",
		"-o", fmt.Sprintf("ControlPath=%s/cm-%%C", config.SocketDir),
		"-o", "ControlPersist=60s",
		effectiveTarget,
	)
	sshArgs = append(sshArgs, postArgs...)

	_ = syscall.Exec(sshBin, sshArgs, os.Environ())
	// If exec fails, fallback to exec.Command
	cmd := exec.Command(sshBin, sshArgs[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}
