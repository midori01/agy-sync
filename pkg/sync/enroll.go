package sync

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"runtime"

	"agy-sync/pkg/cluster"
	"agy-sync/pkg/config"
	"agy-sync/pkg/ssh"
	"agy-sync/pkg/terminal"
)

func getDeployDirSize(dir string, excludeDirs ...string) int64 {
	var total int64
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			for _, ex := range excludeDirs {
				if name == ex || strings.HasPrefix(name, ex) {
					return filepath.SkipDir
				}
			}
		} else {
			total += info.Size()
		}
		return nil
	})
	return total
}

func CmdEnroll(args []string) {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Printf(`
Usage: agy-sync enroll <target> [alias] [options]
       agy-sync deploy <target> [alias] [options]

Arguments:
  target                SSH target (e.g. ubuntu@129.225.149.111 or host:port)
  alias                 Cluster alias for the node (e.g. kix1, default: derived from hostname)

Options:
  --hostname <name>     Configure remote system hostname and /etc/hosts (e.g. orcl-kix1)
  --tailscale-key <key> Tailscale auth key to auto-join network (remembered in cluster config)
  --no-tailscale        Skip Tailscale installation & network joining
  --no-pool             Skip agy-pool multi-account deployment
  --no-agy              Skip official Google Antigravity CLI installation
`)
		return
	}

	target := args[0]
	aliasOverride := ""
	newHostname := ""
	tsKey := ""
	installAgy := true
	installPool := true
	setupTailscale := true

	i := 1
	for i < len(args) {
		a := args[i]
		if (a == "--hostname" || a == "-H") && i+1 < len(args) {
			newHostname = args[i+1]
			i += 2
		} else if (a == "--tailscale-key" || a == "--ts-key" || a == "--auth-key") && i+1 < len(args) {
			tsKey = args[i+1]
			i += 2
		} else if a == "--no-tailscale" {
			setupTailscale = false
			i++
		} else if a == "--no-pool" {
			installPool = false
			i++
		} else if a == "--no-agy" {
			installAgy = false
			i++
		} else if !strings.HasPrefix(a, "-") && aliasOverride == "" {
			aliasOverride = a
			i++
		} else {
			i++
		}
	}

	user, host, port, targetSSH := config.ParseEndpoint(target, 22)
	if host == "" {
		fmt.Printf("[❌] Invalid target specification: '%s'\n", target)
		return
	}

	fmt.Printf("[🚀] Initiating zero-touch automated deployment to '%s' (port %d)...\n", targetSSH, port)
	sshTarget := targetSSH

	// 1. Reachability & OS detection probe
	probeCmd := "echo READY && uname -s && uname -m && uname -n && whoami && echo \"HOME:$HOME\""
	stdout, _, _, err := ssh.RunSSHCmd(sshTarget, port, probeCmd, false, 8)
	if err != nil || !strings.Contains(stdout, "READY") {
		fmt.Printf("[❌] Cannot establish initial SSH connection to '%s:%d': Host unreachable or key not accepted\n", sshTarget, port)
		return
	}

	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	readyIdx := -1
	for idx, l := range lines {
		if strings.TrimSpace(l) == "READY" {
			readyIdx = idx
			break
		}
	}

	osName := "Linux"
	arch := "unknown"
	remoteHostname := "unknown"
	remoteUser := user
	if remoteUser == "" {
		remoteUser = "root"
	}
	remHome := ""

	if readyIdx >= 0 {
		if readyIdx+1 < len(lines) {
			osName = strings.TrimSpace(lines[readyIdx+1])
		}
		if readyIdx+2 < len(lines) {
			arch = strings.TrimSpace(lines[readyIdx+2])
		}
		if readyIdx+3 < len(lines) {
			remoteHostname = strings.TrimSpace(lines[readyIdx+3])
		}
		if readyIdx+4 < len(lines) {
			remoteUser = strings.TrimSpace(lines[readyIdx+4])
		}
	}

	for _, l := range lines {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "HOME:") {
			remHome = strings.TrimSpace(strings.TrimPrefix(l, "HOME:"))
		}
	}
	if remHome == "" {
		if remoteUser == "root" {
			remHome = "/root"
		} else {
			remHome = fmt.Sprintf("/home/%s", remoteUser)
		}
	}

	if remoteUser != "" && !strings.Contains(sshTarget, "@") {
		sshTarget = fmt.Sprintf("%s@%s", remoteUser, host)
	}

	fmt.Printf("[✅] Connected to remote target: %s %s (hostname: '%s', user: '%s')\n", osName, arch, remoteHostname, remoteUser)

	// 2. Prerequisites Auto-Installation
	fmt.Println("[📦] Checking and auto-installing remote prerequisites...")
	pkgSetupScript := `
if [ -d "/data/data/com.termux" ]; then
    pkg update -y >/dev/null 2>&1 || true
    pkg install -y openssh python tar curl golang make git >/dev/null 2>&1 || true
elif command -v apt-get >/dev/null 2>&1; then
    SUDO=""
    if [ "$(id -u)" -ne 0 ] && command -v sudo >/dev/null 2>&1; then
        SUDO="sudo -n"
    fi
    export DEBIAN_FRONTEND=noninteractive
    $SUDO apt-get update -y >/dev/null 2>&1 || true
    $SUDO apt-get install -y openssh-client openssh-server python3 python3-sqlite3 tar curl iptables golang-go make git >/dev/null 2>&1 || true
elif command -v pacman >/dev/null 2>&1; then
    SUDO=""
    if [ "$(id -u)" -ne 0 ] && command -v sudo >/dev/null 2>&1; then
        SUDO="sudo -n"
    fi
    $SUDO pacman -Sy --noconfirm openssh python tar curl iptables go make git >/dev/null 2>&1 || true
elif command -v dnf >/dev/null 2>&1; then
    SUDO=""
    if [ "$(id -u)" -ne 0 ] && command -v sudo >/dev/null 2>&1; then
        SUDO="sudo -n"
    fi
    $SUDO dnf install -y openssh-server openssh-clients python3 tar curl iptables golang make git >/dev/null 2>&1 || true
fi
`
	_, _, _, _ = ssh.RunSSHCmd(sshTarget, port, pkgSetupScript, false)

	// 3. Hostname Configuration
	if newHostname != "" && newHostname != remoteHostname {
		fmt.Printf("[🔖] Setting system hostname to '%s'...\n", newHostname)
		hnameScript := fmt.Sprintf(`
SUDO=""
if [ "$(id -u)" -ne 0 ] && command -v sudo >/dev/null 2>&1; then
    SUDO="sudo -n"
fi
$SUDO hostnamectl set-hostname '%s' 2>/dev/null || $SUDO hostname '%s' 2>/dev/null || true
if [ -f /etc/hosts ]; then
    $SUDO sed -i 's/127\.0\.1\.1.*/127.0.1.1 %s/' /etc/hosts 2>/dev/null || true
    if ! grep -q '%s' /etc/hosts; then
        echo "127.0.1.1 %s" | $SUDO tee -a /etc/hosts >/dev/null 2>&1 || true
    fi
fi
`, newHostname, newHostname, newHostname, newHostname, newHostname)
		_, _, _, _ = ssh.RunSSHCmd(sshTarget, port, hnameScript, false)
		remoteHostname = newHostname
	}

	// 4. Tailscale Deployment & Network Join
	if tsKey == "" {
		tsKey = cluster.GetClusterTailscaleKey()
	}
	cfg := config.LoadConfig()

	tsIP := ""
	if setupTailscale {
		if tsKey != "" {
			if _, ok := cfg["cluster"]; !ok {
				cfg["cluster"] = make(map[string]string)
			}
			cfg["cluster"]["tailscale_auth_key"] = tsKey
			config.SaveConfig(cfg)
		}

		fmt.Println("[🌐] Checking Tailscale status on remote host...")
		_, _, code, _ := ssh.RunSSHCmd(sshTarget, port, "command -v tailscale >/dev/null 2>&1", false)
		if code != 0 {
			fmt.Println("[🌐] Installing Tailscale via official installer...")
			tsInstallScript := `
SUDO=""
if [ "$(id -u)" -ne 0 ] && command -v sudo >/dev/null 2>&1; then
    SUDO="sudo -n"
fi
curl -fsSL https://tailscale.com/install.sh | $SUDO sh >/dev/null 2>&1
`
			_, _, _, _ = ssh.RunSSHCmd(sshTarget, port, tsInstallScript, false)
		}

		ipRes, _, _, _ := ssh.RunSSHCmd(sshTarget, port, "tailscale ip -4 2>/dev/null", false)
		tsIP = strings.TrimSpace(ipRes)

		if tsIP == "" {
			if tsKey != "" {
				fmt.Println("[🌐] Connecting to Tailscale mesh using auth key...")
				tsJoinScript := fmt.Sprintf(`
SUDO=""
if [ "$(id -u)" -ne 0 ] && command -v sudo >/dev/null 2>&1; then
    SUDO="sudo -n"
fi
$SUDO tailscale up --authkey='%s' --hostname='%s' --accept-routes >/dev/null 2>&1
`, tsKey, remoteHostname)
				_, _, _, _ = ssh.RunSSHCmd(sshTarget, port, tsJoinScript, false)
				ipRes, _, _, _ = ssh.RunSSHCmd(sshTarget, port, "tailscale ip -4 2>/dev/null", false)
				tsIP = strings.TrimSpace(ipRes)
			} else {
				fmt.Println("[🌐] Initiating Tailscale mesh connection...")
				tsStartScript := fmt.Sprintf(`
SUDO=""
if [ "$(id -u)" -ne 0 ] && command -v sudo >/dev/null 2>&1; then
    SUDO="sudo -n"
fi
$SUDO tailscale up --hostname='%s' --accept-routes --reset >/tmp/tailscale-up.log 2>&1 &
`, remoteHostname)
				_, _, _, _ = ssh.RunSSHCmd(sshTarget, port, tsStartScript, false)

				loginURL := ""
				for attempt := 0; attempt < 8; attempt++ {
					time.Sleep(1 * time.Second)
					stOut, _, _, _ := ssh.RunSSHCmd(sshTarget, port, "tailscale status 2>/dev/null || cat /tmp/tailscale-up.log 2>/dev/null", false)
					for _, line := range strings.Split(stOut, "\n") {
						line = strings.TrimSpace(line)
						if strings.Contains(line, "login.tailscale.com/a/") {
							idx := strings.Index(line, "https://login.tailscale.com/a/")
							if idx >= 0 {
								loginURL = line[idx:]
								break
							}
						}
					}
					if loginURL != "" {
						break
					}
					ipCheck, _, _, _ := ssh.RunSSHCmd(sshTarget, port, "tailscale ip -4 2>/dev/null", false)
					if strings.TrimSpace(ipCheck) != "" {
						tsIP = strings.TrimSpace(ipCheck)
						break
					}
				}

				if tsIP == "" && loginURL != "" {
					fmt.Printf("\n[🌐] ═══ Tailscale Authorization Required ═══\n")
					fmt.Printf("👉 Please open this URL in your browser to approve '%s':\n", remoteHostname)
					fmt.Printf("   \033[1;36m%s\033[0m\n\n", loginURL)
					fmt.Println("[⏳] Awaiting Tailscale authentication (waiting up to 120s)...")

					for wait := 0; wait < 40; wait++ {
						time.Sleep(3 * time.Second)
						ipCheck, _, _, _ := ssh.RunSSHCmd(sshTarget, port, "tailscale ip -4 2>/dev/null", false)
						if strings.TrimSpace(ipCheck) != "" {
							tsIP = strings.TrimSpace(ipCheck)
							break
						}
					}
				}
			}
		}

		if tsIP != "" {
			fmt.Printf("[✅] Tailscale mesh connected: %s\n", tsIP)
			fwScript := `
SUDO=""
if [ "$(id -u)" -ne 0 ] && command -v sudo >/dev/null 2>&1; then
    SUDO="sudo -n"
fi
$SUDO iptables -C INPUT -i tailscale0 -j ACCEPT 2>/dev/null || $SUDO iptables -I INPUT 1 -i tailscale0 -j ACCEPT 2>/dev/null || true
if command -v systemctl >/dev/null 2>&1; then
    $SUDO systemctl enable --now ssh 2>/dev/null || $SUDO systemctl enable --now sshd 2>/dev/null || true
fi
`
			_, _, _, _ = ssh.RunSSHCmd(sshTarget, port, fwScript, false)
		} else {
			fmt.Println("[⚠️] Tailscale not connected; falling back to direct connection.")
		}
	}

	// 5. Antigravity CLI (agy-cli) Deployment
	if installAgy {
		fmt.Println("[✨] Checking Antigravity CLI (agy)...")
		agyCheck, _, code, _ := ssh.RunSSHCmd(sshTarget, port, "command -v agy >/dev/null 2>&1 && agy --version", false)
		if code != 0 {
			fmt.Println("[✨] Installing official Antigravity CLI binary...")
			agyInstallScript := `
if [ ! -d "/data/data/com.termux" ]; then
    curl -fsSL https://antigravity.google/cli/install.sh | bash >/dev/null 2>&1 || true
    mkdir -p ~/.local/bin
    if [ -w "/usr/local/bin" ]; then
        ln -sf ~/.local/bin/agy /usr/local/bin/agy 2>/dev/null || true
    elif command -v sudo >/dev/null 2>&1; then
        sudo -n ln -sf ~/.local/bin/agy /usr/local/bin/agy 2>/dev/null || true
    fi
fi
`
			_, _, _, _ = ssh.RunSSHCmd(sshTarget, port, agyInstallScript, false)
			agyVer, _, _, _ := ssh.RunSSHCmd(sshTarget, port, "agy --version 2>/dev/null", false)
			if strings.TrimSpace(agyVer) != "" {
				fmt.Printf("[✅] Antigravity CLI installed successfully (v%s)\n", strings.TrimSpace(agyVer))
			}
		} else {
			fmt.Printf("[✅] Antigravity CLI already present (v%s)\n", strings.TrimSpace(agyCheck))
		}
	}

	// 6. Mutual Ed25519 SSH Key Exchange Across Entire Cluster
	fmt.Println("[🔑] Performing cluster-wide mutual Ed25519 SSH key exchange...")
	keyGenScript := `
mkdir -p ~/.ssh && chmod 700 ~/.ssh
if [ ! -f ~/.ssh/id_ed25519.pub ]; then
    ssh-keygen -t ed25519 -N "" -f ~/.ssh/id_ed25519 -q
fi
cat ~/.ssh/id_ed25519.pub
`
	remPubRes, _, _, _ := ssh.RunSSHCmd(sshTarget, port, keyGenScript, false)
	remPubkey := strings.TrimSpace(remPubRes)
	if remPubkey != "" {
		_ = os.MkdirAll(filepath.Dir(config.AuthKeysFile), 0700)
		localKeys := make(map[string]bool)
		if data, err := os.ReadFile(config.AuthKeysFile); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if k := strings.TrimSpace(line); k != "" {
					localKeys[k] = true
				}
			}
		}
		localKeys[remPubkey] = true
		var sortedKeys []string
		for k := range localKeys {
			sortedKeys = append(sortedKeys, k)
		}
		sort.Strings(sortedKeys)
		allKeysContent := strings.Join(sortedKeys, "\n") + "\n"
		_ = os.WriteFile(config.AuthKeysFile, []byte(allKeysContent), 0600)

		deployKeysCmd := "mkdir -p ~/.ssh && chmod 700 ~/.ssh && cat > ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys"
		sshArgs := ssh.GetSSHCmd(sshTarget, port, deployKeysCmd, true)
		proc := exec.Command("ssh", sshArgs...)
		proc.Stdin = strings.NewReader(allKeysContent)
		if err := proc.Run(); err != nil {
			fmt.Printf("[⚠️] Warning: failed to write authorized_keys to %s: %v\n", sshTarget, err)
		}

		cluster.BroadcastAuthorizedKeyToOnlinePeers(remPubkey)
		fmt.Println("[✅] Full-mesh SSH mutual trust established across cluster.")
	}

	// 7. Deploy agy-sync Synchronizer Engine
	targetBinaryData, err := GetOrBuildTargetBinary(osName, arch)
	if err != nil || len(targetBinaryData) == 0 {
		// Remote architecture binary could not be built/found locally.
		// Check if remote target has 'go' compiler to build from source directly!
		remCheckGo := `command -v go 2>/dev/null || echo NONE`
		stdout, _, _, _ := ssh.RunSSHCmd(sshTarget, port, remCheckGo, false)
		if strings.TrimSpace(stdout) != "NONE" && strings.TrimSpace(stdout) != "" {
			fmt.Printf("[💡] Local host lacks Go cross-compiler for %s/%s, but remote target has Go installed. Will build on target.\n", osName, arch)
		} else {
			platformName, installGoCmd := GetGoInstallAdvice()
			fmt.Printf("[⚠️] Warning: Cannot provide pre-compiled binary for remote architecture '%s/%s' (host is %s/%s, companion binary missing, and Go compiler not installed).\n", osName, arch, runtime.GOOS, runtime.GOARCH)
			fmt.Printf("    👉 Tip: Install Go on this device (%s: '%s') or on the target device.\n", platformName, installGoCmd)
		}
	} else {
		binSz := int64(len(targetBinaryData))
		spinEngine := terminal.NewSpinner(fmt.Sprintf("Deploying agy-sync engine (%s)", terminal.FormatBytes(binSz)), true)
		spinEngine.Start()

		deployEngineCmd := `
mkdir -p ~/.gemini/antigravity-cli/bin ~/.local/bin
cat > ~/.gemini/antigravity-cli/bin/agy-sync
chmod +x ~/.gemini/antigravity-cli/bin/agy-sync
ln -sf ~/.gemini/antigravity-cli/bin/agy-sync ~/.local/bin/agy-sync 2>/dev/null || true
if [ -w "/usr/local/bin" ]; then
    ln -sf ~/.gemini/antigravity-cli/bin/agy-sync /usr/local/bin/agy-sync 2>/dev/null || true
elif command -v sudo >/dev/null 2>&1; then
    sudo -n ln -sf ~/.gemini/antigravity-cli/bin/agy-sync /usr/local/bin/agy-sync 2>/dev/null || true
elif [ -d /data/data/com.termux/files/usr/bin ]; then
    ln -sf ~/.gemini/antigravity-cli/bin/agy-sync /data/data/com.termux/files/usr/bin/agy-sync 2>/dev/null || true
fi
`
		procEngine := exec.Command("ssh", ssh.GetSSHCmd(sshTarget, port, deployEngineCmd, true)...)
		procEngine.Stdin = bytes.NewReader(targetBinaryData)
		if err := procEngine.Run(); err == nil {
			spinEngine.Stop(fmt.Sprintf("\033[32m✓\033[0m Deploying agy-sync engine... done \033[90m(%s)\033[0m", terminal.FormatBytes(binSz)))
		} else {
			spinEngine.Stop(fmt.Sprintf("\033[31m✗\033[0m Deploying agy-sync engine... failed: %v", err))
		}
	}

	// 8. Deploy agy-sync-go source & agy-pool Multi-Account Engine
	if _, err := os.Stat(config.AgySyncGoDir); err == nil {
		repoSz := getDeployDirSize(config.AgySyncGoDir, "bin", ".git")
		spinRepo := terminal.NewSpinner(fmt.Sprintf("Deploying agy-sync-go source (%s)", terminal.FormatBytes(repoSz)), true)
		spinRepo.Start()

		p1 := exec.Command("tar", "-C", config.HomeDir, "--exclude=bin", "--exclude='*.tmp*'", "--exclude=.git/index.lock", "-czf", "-", "agy-sync-go")
		p2 := exec.Command("ssh", ssh.GetSSHCmd(sshTarget, port, "tar -C ~ --no-same-owner -xzf - 2>/dev/null && git -C ~/agy-sync-go checkout . 2>/dev/null", true)...)
		nBytes, err := RunPipedCommandsWithBytes(p1, p2, func(cur int64) {
			pct := 0.0
			if repoSz > 0 {
				pct = (float64(cur) / float64(repoSz)) * 100.0
				if pct > 100.0 {
					pct = 100.0
				}
			}
			spinRepo.SetStatus(fmt.Sprintf("Deploying agy-sync-go ↑ [%s/%s (%.0f%%)]", terminal.FormatBytes(cur, true), terminal.FormatBytes(repoSz, true), pct))
		})
		if err == nil {
			spinRepo.Stop(fmt.Sprintf("\033[32m✓\033[0m Deploying agy-sync-go source... done \033[90m(%s)\033[0m", terminal.FormatBytes(nBytes)))
		} else {
			spinRepo.Stop(fmt.Sprintf("\033[31m✗\033[0m Deploying agy-sync-go source... failed: %v", err))
		}
	}

	if installPool {
		if _, err := os.Stat(config.PoolDir); err == nil {
			poolSz := getDeployDirSize(config.PoolDir, ".git")
			spinPool := terminal.NewSpinner(fmt.Sprintf("Deploying agy-pool engine (%s)", terminal.FormatBytes(poolSz)), true)
			spinPool.Start()

			p1 := exec.Command("tar", "-C", config.HomeDir, "--exclude=.git", "-czf", "-", "agy-pool")
			poolSetup := `
tar -C ~ --no-same-owner -xzf -
if [ -f ~/agy-pool/install.sh ]; then
    bash ~/agy-pool/install.sh >/dev/null 2>&1 || true
fi
`
			p2 := exec.Command("ssh", ssh.GetSSHCmd(sshTarget, port, poolSetup, true)...)
			nBytes, err := RunPipedCommandsWithBytes(p1, p2, func(cur int64) {
				pct := 0.0
				if poolSz > 0 {
					pct = (float64(cur) / float64(poolSz)) * 100.0
					if pct > 100.0 {
						pct = 100.0
					}
				}
				spinPool.SetStatus(fmt.Sprintf("Deploying agy-pool ↑ [%s/%s (%.0f%%)]", terminal.FormatBytes(cur, true), terminal.FormatBytes(poolSz, true), pct))
			})
			if err == nil {
				spinPool.Stop(fmt.Sprintf("\033[32m✓\033[0m Deploying agy-pool engine... done \033[90m(%s)\033[0m", terminal.FormatBytes(nBytes)))
			} else {
				spinPool.Stop(fmt.Sprintf("\033[31m✗\033[0m Deploying agy-pool engine... failed: %v", err))
			}
		}
	}

	// 9. Register in devices.json & Reconcile SSH Config
	fmt.Println("[🪪] Registering node into cluster registry...")
	preferredIP := host
	if tsIP != "" {
		preferredIP = tsIP
	}
	preferredEP := fmt.Sprintf("%s:%d", preferredIP, port)
	if remoteUser != "" && remoteUser != os.Getenv("USER") {
		preferredEP = fmt.Sprintf("%s@%s:%d", remoteUser, preferredIP, port)
	}

	lanProbeCmd := `(ifconfig 2>/dev/null | awk '/^[a-zA-Z0-9]/ {iface=$1} /inet (addr:)?/ {if (iface ~ /^(wlan|eth|en|ra|br|swlan|usb|rndis|wg)/) print $2}' | sed 's/addr://'; ip -o -4 addr show 2>/dev/null | awk '$2 ~ /^(wlan|eth|en|ra|br|swlan|usb|rndis|wg)/ {print $4}' | cut -d/ -f1) | tr '\n' ','`
	lanRes, _, _, _ := ssh.RunSSHCmd(sshTarget, port, lanProbeCmd, false)
	var lanIPs []string
	lanSet := make(map[string]bool)
	for _, ip := range strings.Split(lanRes, ",") {
		if ip = strings.TrimSpace(ip); ip != "" && !strings.HasPrefix(ip, "127.") && !strings.HasPrefix(ip, "100.") && !lanSet[ip] {
			lanIPs = append(lanIPs, ip)
			lanSet[ip] = true
		}
	}
	if host != preferredIP && !lanSet[host] && !strings.HasPrefix(host, "127.") && !strings.HasPrefix(host, "100.") {
		lanIPs = append(lanIPs, host)
		lanSet[host] = true
	}

	targetAlias := strings.ToLower(strings.TrimSpace(aliasOverride))
	if targetAlias == "" {
		targetAlias = strings.ToLower(remoteHostname)
		targetAlias = strings.TrimPrefix(targetAlias, "orcl-")
	}

	reg := config.LoadRegistry()
	uniqueAlias := cluster.GetUniqueAlias(targetAlias, preferredEP, reg)
	idType := "Host"
	if port == 8022 {
		idType = "Codename"
	}

	reg[preferredEP] = &config.DeviceEntry{
		Alias:        uniqueAlias,
		NativeName:   strings.ToLower(remoteHostname),
		IdType:       idType,
		Deleted:      false,
		UpdatedAt:    float64(time.Now().UnixNano()) / 1e9,
		User:         remoteUser,
		LanIps:       lanIPs,
		LanUpdatedAt: float64(time.Now().UnixNano()) / 1e9,
	}

	_ = config.SaveRegistry(reg)
	cluster.ReconcileRegistry()
	cluster.BroadcastRegistryToOnlinePeers(true)

	fmt.Printf("[✅] Node successfully registered as '%s' (%s)\n", uniqueAlias, preferredEP)

	// 10. Initial Cluster Synchronization
	fmt.Printf("[🔄] Performing initial full cluster synchronization with '%s'...\n", uniqueAlias)
	_, finalHost, finalPort, finalSSH := config.ParseEndpoint(preferredEP, port)
	_ = finalHost
	SyncWithPeer(finalSSH, finalPort, uniqueAlias, false, false, nil, nil)

	cluster.ManageKeepalive("restart")
	fmt.Println()
	fmt.Printf("[🎉] Auto-deployment complete! Node '%s' is fully operational in the cluster.\n", uniqueAlias)
}

func CmdPair(args []string) {
	port := 8088
	if len(args) > 0 {
		if p, err := strconv.Atoi(args[0]); err == nil {
			port = p
		}
	}

	token := randomToken(8)
	timeout := 180

	localName := config.GetLocalDeviceName()
	localPubkey := ""
	if data, err := os.ReadFile(config.AuthKeysFile); err == nil {
		lines := strings.Split(string(data), "\n")
		if len(lines) > 0 && strings.TrimSpace(lines[0]) != "" {
			localPubkey = strings.TrimSpace(lines[0])
		}
	}
	if localPubkey == "" {
		if data, err := os.ReadFile(filepath.Join(config.HomeDir, ".ssh", "id_ed25519.pub")); err == nil {
			localPubkey = strings.TrimSpace(string(data))
		}
	}

	lanIPs := config.GetLocalLANIPs()
	tsIPs := config.GetTailscaleIPs()
	locEp := config.GetLocalEndpoint()
	_, locHost, _, _ := config.ParseEndpoint(locEp, 8022)
	if strings.HasPrefix(locHost, "100.") {
		hasLoc := false
		for _, tip := range tsIPs {
			if tip == locHost {
				hasLoc = true
				break
			}
		}
		if !hasLoc {
			tsIPs = append(tsIPs, locHost)
		}
	}

	fmt.Printf("\n[Cluster Pairing Service] (Active for %ds)\n", timeout)
	fmt.Println(strings.Repeat("─", 58))
	fmt.Println("👉 Option A — If agy-sync is already present on the new device:")
	if len(lanIPs) > 0 {
		for _, lip := range lanIPs {
			fmt.Printf("   • Via LAN/Wi-Fi:   agy-sync join http://%s:%d/join/%s\n", lip, port, token)
		}
	}
	if len(tsIPs) > 0 {
		for _, tip := range tsIPs {
			fmt.Printf("   • Via Tailnet:     agy-sync join http://%s:%d/join/%s\n", tip, port, token)
		}
	}

	fmt.Println("\n👉 Option B — Automated 1-line installation for clean/uninstalled devices:")
	if len(lanIPs) > 0 {
		for _, lip := range lanIPs {
			fmt.Printf("   • Via LAN/Wi-Fi:   curl -fsSL http://%s:%d/join/%s | bash\n", lip, port, token)
		}
	}
	if len(tsIPs) > 0 {
		for _, tip := range tsIPs {
			fmt.Printf("   • Via Tailnet:     curl -fsSL http://%s:%d/join/%s | bash\n", tip, port, token)
		}
	}
	fmt.Println(strings.Repeat("─", 58))
	fmt.Println("[⏳] Awaiting connection from new device...")

	mux := http.NewServeMux()
	var server *http.Server
	var pairedName string
	var pairedEP string
	var pairMu sync.Mutex
	var once sync.Once

	mux.HandleFunc(fmt.Sprintf("/join/%s", token), func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.Host)
		if host == "" {
			host = "127.0.0.1"
		}
		script := fmt.Sprintf(`#!/usr/bin/env bash
set -e
echo "[🚀] Enrolling device into Antigravity cluster via '%s'..."
if [ -d "/data/data/com.termux" ]; then
    pkg update -y >/dev/null 2>&1 || true
    pkg install -y openssh python tar curl golang make git >/dev/null 2>&1 || true
    sshd 2>/dev/null || true
elif command -v apt-get >/dev/null 2>&1; then
    sudo -n apt-get update -y >/dev/null 2>&1 || true
    sudo -n apt-get install -y openssh-client openssh-server python3 python3-sqlite3 tar curl iptables golang-go make git >/dev/null 2>&1 || true
    if command -v systemctl >/dev/null 2>&1; then
        sudo -n systemctl enable --now ssh 2>/dev/null || sudo -n systemctl enable --now sshd 2>/dev/null || true
    fi
fi

mkdir -p ~/.ssh && chmod 700 ~/.ssh
if [ ! -f ~/.ssh/id_ed25519.pub ]; then
    if [ -f ~/.ssh/id_ed25519 ]; then
        ssh-keygen -y -f ~/.ssh/id_ed25519 > ~/.ssh/id_ed25519.pub 2>/dev/null || true
    fi
    if [ ! -f ~/.ssh/id_ed25519.pub ]; then
        ssh-keygen -t ed25519 -N "" -f ~/.ssh/id_ed25519 -q
    fi
fi
chmod 600 ~/.ssh/id_ed25519 2>/dev/null || true
chmod 644 ~/.ssh/id_ed25519.pub 2>/dev/null || true
PUB="$(cat ~/.ssh/id_ed25519.pub)"
HNAME="$(uname -n 2>/dev/null || echo node)"
ARCH="$(uname -m 2>/dev/null || echo unknown)"
REM_USER="$(whoami 2>/dev/null || echo "$USER")"
REM_PORT="22"
if [ -d "/data/data/com.termux" ]; then
    REM_PORT="8022"
fi
LAN_IPS="$( (ifconfig 2>/dev/null | awk '/^[a-zA-Z0-9]/ {iface=$1} /inet (addr:)?/ {if (iface ~ /^(wlan|eth|en|ra|br|swlan|usb|rndis|wg)/) print $2}' | sed 's/addr://'; ip -o -4 addr show 2>/dev/null | awk '$2 ~ /^(wlan|eth|en|ra|br|swlan|usb|rndis|wg)/ {print $4}' | cut -d/ -f1) | tr '\n' ',')"

RESP="$(curl -fsSL --data-urlencode "pubkey=$PUB" --data-urlencode "name=$HNAME" --data-urlencode "lan_ips=$LAN_IPS" --data-urlencode "user=$REM_USER" --data-urlencode "port=$REM_PORT" "http://%s:%d/exchange/%s")"

mkdir -p ~/.gemini/antigravity-cli/bin ~/.local/bin ~/.ssh
echo "$RESP" | grep "^ALL_KEYS_B64:" | cut -d: -f2 | base64 -d > ~/.ssh/authorized_keys
chmod 600 ~/.ssh/authorized_keys

echo "$RESP" | grep "^DEVICES_JSON_B64:" | cut -d: -f2 | base64 -d > ~/.gemini/devices.json

echo "[⚡] Downloading agy-sync synchronizer engine..."
curl -# -fL "http://%s:%d/bin/agy-sync?arch=${ARCH}" -o ~/.gemini/antigravity-cli/bin/agy-sync 2>/dev/null || true
curl -fsSL "http://%s:%d/done" >/dev/null 2>&1 || true

if [ -s ~/.gemini/antigravity-cli/bin/agy-sync ] && [ "$(head -c 4 ~/.gemini/antigravity-cli/bin/agy-sync 2>/dev/null)" = $'\x7fELF' ]; then
    chmod 755 ~/.gemini/antigravity-cli/bin/agy-sync
    ln -sf ~/.gemini/antigravity-cli/bin/agy-sync ~/.local/bin/agy-sync 2>/dev/null || true
    if [ -d /data/data/com.termux/files/usr/bin ]; then
        ln -sf ~/.gemini/antigravity-cli/bin/agy-sync /data/data/com.termux/files/usr/bin/agy-sync 2>/dev/null || true
    elif [ -w /usr/local/bin ]; then
        ln -sf ~/.gemini/antigravity-cli/bin/agy-sync /usr/local/bin/agy-sync 2>/dev/null || true
    fi
    echo "[✅] agy-sync engine installed successfully."
else
    rm -f ~/.gemini/antigravity-cli/bin/agy-sync
    echo "[⚠️] Peer could not supply a pre-compiled binary for architecture '${ARCH}'."
    if command -v go >/dev/null 2>&1; then
        echo "[🔨] Local Go compiler found on this device."
    else
        echo "[❌] Architecture '${ARCH}' binary unavailable and 'go' compiler is missing."
        echo "    👉 To enable cross-architecture pairing: Install 'golang' on either device:"
        echo "       • Debian / Ubuntu : sudo apt-get install -y golang-go"
        echo "       • Arch Linux      : sudo pacman -S --noconfirm go"
        echo "       • Fedora / RHEL   : sudo dnf install -y golang"
        echo "       • Alpine Linux    : sudo apk add go"
        echo "       • Android Termux  : pkg install -y golang"
    fi
fi

if ! command -v agy >/dev/null 2>&1; then
    echo "[✨] Installing Antigravity CLI (agy)..."
    if [ -d "/data/data/com.termux" ]; then
        curl -fsSL https://raw.githubusercontent.com/wallentx/antigravity-cli-termux/dev/install.sh | bash >/dev/null 2>&1 || true
    else
        curl -fsSL https://antigravity.google/cli/install.sh | bash >/dev/null 2>&1 || true
    fi
fi

echo "[🎉] Successfully joined Antigravity cluster!"
`, localName, host, port, token, host, port, host, port)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(script))
	})

	mux.HandleFunc(fmt.Sprintf("/exchange/%s", token), func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		peerPub := strings.TrimSpace(r.FormValue("pubkey"))
		peerName := strings.TrimSpace(r.FormValue("name"))
		peerLanIPs := r.FormValue("lan_ips")
		peerUser := strings.TrimSpace(r.FormValue("user"))
		peerPortStr := strings.TrimSpace(r.FormValue("port"))

		if peerPub != "" && (strings.HasPrefix(peerPub, "ssh-") || strings.HasPrefix(peerPub, "ecdsa-")) {
			// 1. Add new peer's public key to local authorized_keys
			localKeys := make(map[string]bool)
			if data, err := os.ReadFile(config.AuthKeysFile); err == nil {
				for _, line := range strings.Split(string(data), "\n") {
					if k := strings.TrimSpace(line); k != "" {
						localKeys[k] = true
					}
				}
			}
			if !localKeys[peerPub] {
				localKeys[peerPub] = true
				var sortedKeys []string
				for k := range localKeys {
					sortedKeys = append(sortedKeys, k)
				}
				sort.Strings(sortedKeys)
				_ = config.WriteFileAtomic(config.AuthKeysFile, []byte(strings.Join(sortedKeys, "\n")+"\n"), 0600)
			}

			// 2. Broadcast new peer's public key across existing cluster peers
			cluster.BroadcastAuthorizedKeyToOnlinePeers(peerPub)

			peerName = regexp.MustCompile(`[^a-zA-Z0-9_\-\.]`).ReplaceAllString(peerName, "")
			if peerName == "" {
				peerName = "node"
			}
			reg := config.LoadRegistry()
			alias := cluster.GetUniqueAlias(peerName, peerPub, reg)

			var ips []string
			for _, ip := range strings.Split(peerLanIPs, ",") {
				if ip = strings.TrimSpace(ip); ip != "" {
					ips = append(ips, ip)
				}
			}

			clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
			peerPort := 8022
			if p, err := strconv.Atoi(peerPortStr); err == nil && p > 0 {
				peerPort = p
			} else if strings.TrimSpace(r.FormValue("port")) == "22" {
				peerPort = 22
			}
			idType := "Host"
			if peerPort == 8022 {
				idType = "Codename"
			}
			peerEP := fmt.Sprintf("%s:%d", clientIP, peerPort)
			if peerUser != "" && peerUser != os.Getenv("USER") {
				peerEP = fmt.Sprintf("%s@%s:%d", peerUser, clientIP, peerPort)
			}

			reg[peerEP] = &config.DeviceEntry{
				Alias:        alias,
				NativeName:   strings.ToLower(peerName),
				IdType:       idType,
				Deleted:      false,
				UpdatedAt:    float64(time.Now().UnixNano()) / 1e9,
				User:         peerUser,
				LanIps:       ips,
				LanUpdatedAt: float64(time.Now().UnixNano()) / 1e9,
			}
			_ = config.SaveRegistry(reg)
			cluster.ReconcileRegistry()
			cluster.BroadcastRegistryToOnlinePeers(true)

			pairMu.Lock()
			pairedName = fmt.Sprintf("%s (%s)", alias, peerName)
			pairedEP = peerEP
			pairMu.Unlock()

			allKeys, _ := os.ReadFile(config.AuthKeysFile)
			allKeysB64 := base64.StdEncoding.EncodeToString(allKeys)
			regData, _ := os.ReadFile(config.DevicesJSONFile)
			regB64 := base64.StdEncoding.EncodeToString(regData)

			resp := fmt.Sprintf("HOST_PUBKEY:%s\nALL_KEYS_B64:%s\nDEVICES_JSON_B64:%s\nASSIGNED_ALIAS:%s\n", localPubkey, allKeysB64, regB64, alias)
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(resp))
		}
	})

	mux.HandleFunc("/bin/agy-sync", func(w http.ResponseWriter, r *http.Request) {
		qArch := strings.ToLower(r.URL.Query().Get("arch"))
		qOS := strings.ToLower(r.URL.Query().Get("os"))
		if qOS == "" {
			qOS = "linux"
		}
		binData, err := GetOrBuildTargetBinary(qOS, qArch)
		clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
		if err != nil || len(binData) == 0 || !ValidateBinaryArchitecture(binData, qOS, qArch) {
			fmt.Printf("[⚠️] Client %s requested %s/%s, but no matching binary or Go compiler is available.\n", clientIP, qOS, qArch)
			http.Error(w, fmt.Sprintf("Binary for architecture '%s/%s' unavailable on peer", qOS, qArch), http.StatusNotFound)
			return
		}
		fmt.Printf("[⚡] Streaming agy-sync engine binary (%s/%s, %s) to %s...\n", qOS, qArch, terminal.FormatBytes(int64(len(binData))), clientIP)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(binData)))
		_, _ = w.Write(binData)
	})

	mux.HandleFunc("/done", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
		go func() {
			time.Sleep(1 * time.Second)
			once.Do(func() {
				if server != nil {
					_ = server.Close()
				}
			})
		}()
	})

	server = &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", port),
		Handler: mux,
	}

	go func() {
		time.Sleep(time.Duration(timeout) * time.Second)
		once.Do(func() {
			if server != nil {
				_ = server.Close()
			}
		})
	}()

	_ = server.ListenAndServe()
	pairMu.Lock()
	pName := pairedName
	pEP := pairedEP
	pairMu.Unlock()
	if pName != "" && pEP != "" {
		fmt.Printf("\n[✅] Successfully paired and enrolled new device '%s'! 🎉\n", pName)
		fmt.Printf("[🔄] Performing initial cluster synchronization with '%s'...\n", pName)
		_, _, finalPort, finalSSH := config.ParseEndpoint(pEP, 8022)
		parts := strings.Fields(pName)
		peerAlias := parts[0]
		SyncWithPeer(finalSSH, finalPort, peerAlias, false, false, nil, nil)
		fmt.Printf("[🎉] Initial synchronization with '%s' complete!\n", peerAlias)
	}
}
