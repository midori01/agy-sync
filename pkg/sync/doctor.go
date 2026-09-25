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
		_ = os.Chmod(privFile, 0600)
		_ = os.Chmod(pubFile, 0644)
		return strings.TrimSpace(string(data)), nil
	}

	// If private key already exists, derive public key to avoid interactive overwrite prompt
	if _, err := os.Stat(privFile); err == nil {
		_ = os.Chmod(privFile, 0600)
		cmd := exec.Command("ssh-keygen", "-y", "-f", privFile)
		if out, err := cmd.Output(); err == nil && len(out) > 0 {
			pubStr := strings.TrimSpace(string(out))
			_ = os.WriteFile(pubFile, []byte(pubStr+"\n"), 0644)
			return pubStr, nil
		}
	}

	// Generate keypair
	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", privFile, "-q")
	if err := cmd.Run(); err != nil {
		return "", err
	}
	_ = os.Chmod(privFile, 0600)
	_ = os.Chmod(pubFile, 0644)

	data, err := os.ReadFile(pubFile)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// GetGoInstallAdvice returns the detected platform name and exact command to install Go compiler.
func GetGoInstallAdvice() (string, string) {
	if _, err := os.Stat("/data/data/com.termux"); err == nil {
		return "Termux", "pkg install -y golang"
	}
	isRoot := os.Geteuid() == 0
	sudoPrefix := "sudo "
	if isRoot {
		sudoPrefix = ""
	}

	if _, err := exec.LookPath("apt-get"); err == nil {
		return "Debian/Ubuntu", fmt.Sprintf("%sapt-get install -y golang-go", sudoPrefix)
	}
	if _, err := exec.LookPath("pacman"); err == nil {
		return "Arch Linux", fmt.Sprintf("%spacman -S --noconfirm go", sudoPrefix)
	}
	if _, err := exec.LookPath("dnf"); err == nil {
		return "Fedora/RHEL", fmt.Sprintf("%sdnf install -y golang", sudoPrefix)
	}
	if _, err := exec.LookPath("apk"); err == nil {
		return "Alpine Linux", fmt.Sprintf("%sapk add go", sudoPrefix)
	}
	if _, err := exec.LookPath("zypper"); err == nil {
		return "openSUSE", fmt.Sprintf("%szypper install -y go", sudoPrefix)
	}
	if runtime.GOOS == "darwin" {
		return "macOS", "brew install go"
	}
	return "Linux", "curl -fsSL https://go.dev/dl/ | tar -C /usr/local"
}
// CheckGoCompiler detects local Go compiler availability, version, and cross-compilation capability.
func CheckGoCompiler() (bool, string) {
	goPath, err := exec.LookPath("go")
	if err != nil || goPath == "" {
		return false, "not installed"
	}
	out, err := exec.Command(goPath, "version").Output()
	if err != nil {
		return true, "installed (version unknown)"
	}
	verStr := strings.TrimSpace(string(out))
	parts := strings.Fields(verStr)
	v := verStr
	if len(parts) >= 3 {
		v = parts[2]
	}
	return true, fmt.Sprintf("%s (JIT cross-compile ready)", v)
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

	// 7. Go Compiler & Cross-Architecture Engine
	goOk, goVer := CheckGoCompiler()
	if goOk {
		fmt.Printf("│  Go Cross-Build Toolchain  : 🟢 Ready (%s)\n", goVer)
	} else {
		platformName, installGoCmd := GetGoInstallAdvice()
		fmt.Printf("│  Go Cross-Build Toolchain  : ⚪ Optional (%s: '%s')\n", platformName, installGoCmd)
	}

	canonDir := filepath.Dir(config.AgySyncScript)
	hasArm64 := false
	hasAmd64 := false
	if data, err := os.ReadFile(filepath.Join(canonDir, "agy-sync-linux-arm64")); err == nil && len(data) > 0 {
		hasArm64 = true
	} else if runtime.GOARCH == "arm64" {
		hasArm64 = true
	}
	if data, err := os.ReadFile(filepath.Join(canonDir, "agy-sync-linux-amd64")); err == nil && len(data) > 0 {
		hasAmd64 = true
	} else if runtime.GOARCH == "amd64" {
		hasAmd64 = true
	}

	platformName, installGoCmd := GetGoInstallAdvice()
	if goOk && hasArm64 && hasAmd64 {
		fmt.Println("│  Cross-Arch Deployment     : 🟢 Full (Dual-arch cached + JIT cross-compiler)")
	} else if goOk {
		fmt.Println("│  Cross-Arch Deployment     : 🟢 Full (JIT compiler ready for all architectures)")
	} else if hasArm64 && hasAmd64 {
		fmt.Println("│  Cross-Arch Deployment     : 🟢 Ready (Dual-arch cached: arm64, amd64)")
	} else {
		fmt.Printf("│  Cross-Arch Deployment     : 🟡 Local %s only (%s: '%s')\n", runtime.GOARCH, platformName, installGoCmd)
	}
	fmt.Printf("└─────────────────────────────────────────────────────────────────────────┘\n\n")

	// Recommendations
	reader := bufio.NewReader(os.Stdin)
	if !agyOk {
		fmt.Print("Would you like to install Antigravity CLI now? [Y/n]: ")
		text, _ := reader.ReadString('\n')
		text = strings.ToLower(strings.TrimSpace(text))
		if text == "" || text == "y" || text == "yes" {
			CheckAgyCLI(true)
		}
	}

	if !goOk && !(hasArm64 && hasAmd64) {
		fmt.Printf("Would you like to install Go compiler for cross-architecture deployment (%s)? [y/N]: ", installGoCmd)
		text, _ := reader.ReadString('\n')
		text = strings.ToLower(strings.TrimSpace(text))
		if text == "y" || text == "yes" {
			fmt.Printf("[⬇️] Installing Go compiler via: %s\n", installGoCmd)
			cmd := exec.Command("sh", "-c", installGoCmd)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			cmd.Stdin = os.Stdin
			if err := cmd.Run(); err == nil {
				fmt.Println("[✅] Go compiler installed successfully.")
			} else {
				fmt.Printf("[❌] Installation failed: %v\n", err)
			}
		}
	}
}
