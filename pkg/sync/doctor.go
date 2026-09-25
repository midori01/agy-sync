package sync

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"agy-sync/pkg/config"
)

// CheckAgyCLI checks if Antigravity CLI (agy) is installed and operational.
// If missing and autoInstall is true, it triggers platform-specific installation:
// - Android Termux: https://github.com/wallentx/antigravity-cli-termux
// - Standard Linux/macOS: Official Google installer
func CheckAgyCLI(autoInstall bool) (bool, string) {
	if path, err := exec.LookPath("agy"); err == nil && path != "" {
		if out, err := exec.Command(path, "--version").Output(); err == nil {
			v := strings.TrimSpace(string(out))
			if v != "" {
				return true, v
			}
		}
		return true, "installed"
	}

	if !autoInstall {
		return false, "not installed"
	}

	fmt.Println("\n[✨] Antigravity CLI (agy) is required for full agent synchronization.")
	isTermux := false
	if _, err := os.Stat("/data/data/com.termux"); err == nil {
		isTermux = true
	}

	var installCmd *exec.Cmd
	if isTermux {
		fmt.Println("[📦] Detected Android Termux environment.")
		fmt.Println("[⬇️] Installing Antigravity CLI via Termux port (wallentx/antigravity-cli-termux)...")
		script := `curl -fsSL https://raw.githubusercontent.com/wallentx/antigravity-cli-termux/dev/install.sh | bash`
		installCmd = exec.Command("bash", "-c", script)
	} else {
		fmt.Println("[📦] Detected standard Linux environment.")
		fmt.Println("[⬇️] Installing official Antigravity CLI binary...")
		script := `curl -fsSL https://antigravity.google/cli/install.sh | bash`
		installCmd = exec.Command("bash", "-c", script)
	}

	installCmd.Stdout = os.Stdout
	installCmd.Stderr = os.Stderr
	installCmd.Stdin = os.Stdin

	if err := installCmd.Run(); err != nil {
		return false, fmt.Sprintf("installation failed: %v", err)
	}

	if path, err := exec.LookPath("agy"); err == nil && path != "" {
		if out, err := exec.Command(path, "--version").Output(); err == nil {
			v := strings.TrimSpace(string(out))
			return true, v
		}
		return true, "installed"
	}

	return false, "installed but not found in PATH"
}

// CheckPython checks if Python 3 with sqlite3 module is available for ACID DB merges.
func CheckPython() (bool, string) {
	pyPath, err := exec.LookPath("python3")
	if err != nil {
		pyPath, err = exec.LookPath("python")
	}
	if err != nil || pyPath == "" {
		return false, "missing python3"
	}

	// Verify sqlite3 module
	cmd := exec.Command(pyPath, "-c", "import sqlite3, sys; print('.'.join(map(str, sys.version_info[:2])))")
	out, err := cmd.Output()
	if err != nil {
		return false, "python3 present but sqlite3 module unavailable"
	}
	return true, fmt.Sprintf("Python %s (sqlite3 ready)", strings.TrimSpace(string(out)))
}

// CheckSSH checks SSH client availability and local SSH daemon listening status.
func CheckSSH() (clientOk bool, daemonOk bool, details string) {
	if _, err := exec.LookPath("ssh"); err == nil {
		clientOk = true
	}

	isTermux := false
	sshPort := 22
	if _, err := os.Stat("/data/data/com.termux"); err == nil {
		isTermux = true
		sshPort = 8022
	}

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", sshPort), 100*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		daemonOk = true
	} else if isTermux {
		// Attempt to auto-start sshd on Termux
		if sshdPath, err := exec.LookPath("sshd"); err == nil {
			_ = exec.Command(sshdPath).Start()
			time.Sleep(150 * time.Millisecond)
			if c2, err2 := net.DialTimeout("tcp", "127.0.0.1:8022", 150*time.Millisecond); err2 == nil {
				_ = c2.Close()
				daemonOk = true
			}
		}
	}

	var dParts []string
	if clientOk {
		dParts = append(dParts, "client ready")
	} else {
		dParts = append(dParts, "client missing")
	}
	if daemonOk {
		dParts = append(dParts, fmt.Sprintf("sshd listening on :%d", sshPort))
	} else {
		dParts = append(dParts, fmt.Sprintf("sshd not listening on :%d", sshPort))
	}

	return clientOk, daemonOk, strings.Join(dParts, ", ")
}

// EnsureLocalSSHKey verifies that ~/.ssh/id_ed25519 exists, generating one if absent.
func EnsureLocalSSHKey() (string, error) {
	sshDir := filepath.Join(config.HomeDir, ".ssh")
	_ = os.MkdirAll(sshDir, 0700)
	pubFile := filepath.Join(sshDir, "id_ed25519.pub")
	privFile := filepath.Join(sshDir, "id_ed25519")

	if data, err := os.ReadFile(pubFile); err == nil && len(data) > 0 {
		return strings.TrimSpace(string(data)), nil
	}

	// Generate keypair
	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", privFile, "-q")
	if err := cmd.Run(); err != nil {
		return "", err
	}

	data, err := os.ReadFile(pubFile)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// CmdDoctor executes a comprehensive system and environment diagnostics report.
func CmdDoctor() {
	fmt.Printf("\n┌─ [Antigravity Environment Doctor · agy-sync v%s] ────────────────┐\n", config.Version)

	// 1. Antigravity CLI
	agyOk, agyVer := CheckAgyCLI(false)
	if agyOk {
		fmt.Printf("│  Antigravity CLI (agy)     : 🟢 Ready (%s)\n", agyVer)
	} else {
		fmt.Println("│  Antigravity CLI (agy)     : 🟡 Missing (Required for agent sessions)")
	}

	// 2. Python & SQLite
	pyOk, pyDet := CheckPython()
	if pyOk {
		fmt.Printf("│  Python Runtime (SQLite)   : 🟢 Ready (%s)\n", pyDet)
	} else {
		fmt.Printf("│  Python Runtime (SQLite)   : ❌ %s\n", pyDet)
	}

	// 3. OpenSSH & SSHD
	sshCli, sshDae, sshDet := CheckSSH()
	if sshCli && sshDae {
		fmt.Printf("│  OpenSSH & Daemon (sshd)   : 🟢 Ready (%s)\n", sshDet)
	} else if sshCli {
		fmt.Printf("│  OpenSSH & Daemon (sshd)   : 🟡 Client ready, %s\n", sshDet)
	} else {
		fmt.Printf("│  OpenSSH & Daemon (sshd)   : ❌ Missing ssh tools (%s)\n", sshDet)
	}

	// 4. Ed25519 Keypair
	pubKey, keyErr := EnsureLocalSSHKey()
	if keyErr == nil && pubKey != "" {
		keyParts := strings.Fields(pubKey)
		shortKey := pubKey
		if len(keyParts) >= 2 {
			if len(keyParts[1]) > 16 {
				shortKey = keyParts[0] + " " + keyParts[1][:12] + "..." + keyParts[1][len(keyParts[1])-6:]
			}
		}
		fmt.Printf("│  Cluster Identity Key      : 🟢 Ready (%s)\n", shortKey)
	} else {
		fmt.Printf("│  Cluster Identity Key      : ❌ Error: %v\n", keyErr)
	}

	// 5. Tailnet & Virtual Networking
	tsActive := config.IsTailnetActiveLocally()
	tsIPs := config.GetTailscaleIPs()
	if tsActive {
		ipDesc := "Active"
		if len(tsIPs) > 0 {
			ipDesc = fmt.Sprintf("Active (%s)", strings.Join(tsIPs, ", "))
		} else {
			ipDesc = "Active (sing-box / user-space Mesh detected)"
		}
		fmt.Printf("│  Tailnet Virtual Network   : 🌐 %s\n", ipDesc)
	} else {
		fmt.Println("│  Tailnet Virtual Network   : ⚪ Inactive (LAN and direct IP routing will be used)")
	}

	// 6. Cluster Devices
	reg := config.LoadRegistry()
	activeCount := 0
	for _, d := range reg {
		if !d.Deleted {
			activeCount++
		}
	}
	fmt.Printf("│  Cluster Registry          : 🪪 %d node(s) registered in devices.json\n", activeCount)
	fmt.Printf("│  Host Architecture         : 💻 %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Printf("└─────────────────────────────────────────────────────────────────────────┘\n\n")

	// Recommendations
	if !agyOk {
		reader := bufio.NewReader(os.Stdin)
		fmt.Print("Would you like to install Antigravity CLI now? [Y/n]: ")
		text, _ := reader.ReadString('\n')
		text = strings.ToLower(strings.TrimSpace(text))
		if text == "" || text == "y" || text == "yes" {
			CheckAgyCLI(true)
		}
	}
}
