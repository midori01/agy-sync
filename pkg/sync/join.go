package sync

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agy-sync/pkg/cluster"
	"agy-sync/pkg/config"
	"agy-sync/pkg/ssh"
)

// CmdJoin connects this device to an existing Antigravity cluster via a pairing URL or token.
func CmdJoin(args []string) {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Printf(`
Usage: agy-sync join <pairing-url>

Arguments:
  pairing-url           Full pairing URL provided by 'agy-sync pair' on another device
                        (e.g. http://100.91.192.125:8088/join/9c4e2a1b)

Examples:
  agy-sync join http://192.168.1.100:8088/join/a1b2c3d4
  agy-sync join http://100.87.24.6:8088/join/a1b2c3d4
`)
		return
	}

	rawURL := strings.TrimSpace(args[0])
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		rawURL = "http://" + rawURL
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		fmt.Printf("[❌] Invalid pairing URL '%s': %v\n", rawURL, err)
		os.Exit(1)
	}

	pathParts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(pathParts) < 2 || pathParts[0] != "join" {
		fmt.Printf("[❌] Invalid pairing path in URL. Expected '/join/<token>', got '%s'\n", parsed.Path)
		os.Exit(1)
	}
	token := pathParts[1]

	host := parsed.Hostname()
	port := 8088
	if pStr := parsed.Port(); pStr != "" {
		if p, err := strconv.Atoi(pStr); err == nil {
			port = p
		}
	}

	fmt.Printf("\n[🤝] Initiating mutual handshake with cluster peer at %s:%d...\n", host, port)

	// 1. Ensure local SSH key exists
	pubkey, err := EnsureLocalSSHKey()
	if err != nil {
		fmt.Printf("[❌] Failed to prepare local SSH identity: %v\n", err)
		os.Exit(1)
	}

	// 2. Collect local device identity
	localName := config.GetLocalCodename()
	if localName == "" {
		localName, _ = os.Hostname()
	}
	if localName == "" {
		localName = "client-node"
	}
	localUser := os.Getenv("USER")
	if localUser == "" {
		localUser = "user"
	}

	localPort := 22
	if _, err := os.Stat("/data/data/com.termux"); err == nil {
		localPort = 8022
	}

	localLanNets := config.GetLocalLANIPs()
	lanIPsStr := strings.Join(localLanNets, ",")

	// 3. Send mutual exchange payload
	exchangeURL := fmt.Sprintf("http://%s:%d/exchange/%s", host, port, token)
	formData := url.Values{
		"pubkey":  {pubkey},
		"name":    {localName},
		"lan_ips": {lanIPsStr},
		"user":    {localUser},
		"port":    {strconv.Itoa(localPort)},
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.PostForm(exchangeURL, formData)
	if err != nil {
		fmt.Printf("[❌] Handshake failed: Could not reach peer at %s: %v\n", exchangeURL, err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Printf("[❌] Peer rejected pairing request (HTTP %d): %s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("[❌] Failed to read exchange response: %v\n", err)
		os.Exit(1)
	}
	respStr := string(bodyBytes)

	// 4. Parse & save authorized_keys, devices.json, and custom_sync.json
	var authKeysData []byte
	var devicesJSONData []byte
	var customSyncData []byte

	for _, line := range strings.Split(respStr, "\n") {
		if strings.HasPrefix(line, "ALL_KEYS_B64:") {
			rawB64 := strings.TrimPrefix(line, "ALL_KEYS_B64:")
			authKeysData, _ = base64.StdEncoding.DecodeString(rawB64)
		} else if strings.HasPrefix(line, "DEVICES_JSON_B64:") {
			rawB64 := strings.TrimPrefix(line, "DEVICES_JSON_B64:")
			devicesJSONData, _ = base64.StdEncoding.DecodeString(rawB64)
		} else if strings.HasPrefix(line, "CUSTOM_SYNC_B64:") {
			rawB64 := strings.TrimPrefix(line, "CUSTOM_SYNC_B64:")
			customSyncData, _ = base64.StdEncoding.DecodeString(rawB64)
		}
	}

	if len(authKeysData) > 0 {
		_ = os.MkdirAll(filepath.Dir(config.AuthKeysFile), 0700)
		_ = config.WriteFileAtomic(config.AuthKeysFile, authKeysData, 0600)
		fmt.Println("[🔑] Merged cluster mutual SSH authorization keys.")
	}

	if len(devicesJSONData) > 0 {
		_ = os.MkdirAll(filepath.Dir(config.DevicesJSONFile), 0755)
		_ = config.WriteFileAtomic(config.DevicesJSONFile, devicesJSONData, 0644)
		fmt.Println("[🪪] Updated cluster topology devices registry.")
	}

	if len(customSyncData) > 0 {
		_ = os.MkdirAll(filepath.Dir(config.CustomSyncFile), 0755)
		_ = config.WriteFileAtomic(config.CustomSyncFile, customSyncData, 0644)
		fmt.Println("[📋] Synchronized custom sync configuration registry.")
	}

	// 5. Check if peer address is Tailscale IP (100.x.y.z) and record it
	if strings.HasPrefix(host, "100.") {
		reg := config.LoadRegistry()
		for ep, d := range reg {
			if !d.Deleted && (strings.Contains(ep, host) || strings.Contains(d.Alias, host) || d.TailscaleIP == host) {
				d.TailscaleIP = host
				_ = config.SaveRegistry(reg)
				break
			}
		}
	}

	// 6. Notify server that join is finalized
	doneURL := fmt.Sprintf("http://%s:%d/done", host, port)
	_, _ = client.Get(doneURL)

	// 7. Ensure local sshd is active
	isTermux := false
	if _, err := os.Stat("/data/data/com.termux"); err == nil {
		isTermux = true
		conn, err := net.DialTimeout("tcp", "127.0.0.1:8022", 80*time.Millisecond)
		if err != nil {
			if sshdPath, err := exec.LookPath("sshd"); err == nil {
				_ = exec.Command(sshdPath).Start()
			}
		} else {
			_ = conn.Close()
		}
	}

	// 8. Reconcile cluster configuration & SSH aliases
	cluster.ReconcileRegistry()
	ssh.ReconcileSSHConfig(config.LoadRegistry())

	// 9. Relocate executable to canonical directory
	_ = RunInstall()

	fmt.Printf("\n[🎉] Successfully joined Antigravity cluster!\n")
	fmt.Println("    • Mutual authentication established.")
	fmt.Println("    • Local identity and cluster peers synchronized.")

	if isTermux {
		fmt.Println("    • Termux wake-lock & sshd guard active.")
	}

	reader := bufio.NewReader(os.Stdin)
	fmt.Print("\nWould you like to perform the initial synchronization now? [Y/n]: ")
	ans, _ := reader.ReadString('\n')
	ans = strings.ToLower(strings.TrimSpace(ans))
	if ans == "" || ans == "y" || ans == "yes" {
		defAlias, defSSH, defPort := cluster.LoadDefaultTarget()
		tHost := defSSH
		if tHost == "" {
			tHost = defAlias
		}
		if tHost == "" {
			// Find peer from merged devices.json
			reg := config.LoadRegistry()
			for ep, d := range reg {
				if !d.Deleted && (strings.Contains(ep, host) || d.TailscaleIP == host) {
					tHost = ep
					defAlias = d.Alias
					_, _, p, _ := config.ParseEndpoint(ep, 8022)
					defPort = p
					break
				}
			}
		}
		if tHost == "" {
			tHost = host
			defAlias = host
			defPort = 22
			if _, err := os.Stat("/data/data/com.termux"); err == nil {
				defPort = 8022
			}
		}
		SyncWithPeer(tHost, defPort, defAlias, false, false, nil, nil)
	}
}
