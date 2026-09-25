package sync

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"agy-sync/pkg/cluster"
	"agy-sync/pkg/config"
)

// RunSmartWizard launches an interactive, user-friendly onboarding wizard in English.
// It is triggered on the first run of agy-sync or via 'agy-sync init' / 'agy-sync setup'.
func RunSmartWizard() {
	fmt.Printf("\n╭────────────────────────────────────────────────────────────╮\n")
	fmt.Printf("│  ✨ Antigravity Cross-Device Synchronizer (agy-sync v%s)   │\n", config.Version)
	fmt.Printf("╰────────────────────────────────────────────────────────────╯\n")
	fmt.Println("No Antigravity cluster peers are configured on this device yet.")
	fmt.Println("\nPlease choose an initialization option:")
	fmt.Println("  [1] 🔗 Join an existing cluster (Pair with your other phone/laptop/server)")
	fmt.Println("  [2] 🌟 Initialize this device as a primary cluster seed node")
	fmt.Println("  [3] 🛠️ Run environment diagnostic & doctor check")
	fmt.Println("  [4] 📦 Install agy-sync to canonical system PATH (~/.gemini/antigravity-cli/bin)")
	fmt.Println("  [5] 🚪 Exit")

	reader := bufio.NewReader(os.Stdin)
	fmt.Print("\nSelect an option [1-5] (default: 1): ")
	choice, _ := reader.ReadString('\n')
	choice = strings.TrimSpace(choice)
	if choice == "" {
		choice = "1"
	}

	switch choice {
	case "1":
		fmt.Println("\nTo join, run 'agy-sync pair' on your existing device, then paste the URL below.")
		fmt.Print("Enter pairing URL (e.g. http://100.x.y.z:8088/join/xxxx): ")
		urlInput, _ := reader.ReadString('\n')
		urlInput = strings.TrimSpace(urlInput)
		if urlInput == "" {
			fmt.Println("[❌] No URL provided. Aborting.")
			return
		}
		CmdJoin([]string{urlInput})

	case "2":
		fmt.Println("\n[🌟] Initializing device as a primary cluster seed...")
		// 1. Check/offer agy-cli installation
		agyOk, _ := CheckAgyCLI(false)
		if !agyOk {
			fmt.Print("Antigravity CLI (agy) not found. Would you like to install it now? [Y/n]: ")
			ans, _ := reader.ReadString('\n')
			ans = strings.ToLower(strings.TrimSpace(ans))
			if ans == "" || ans == "y" || ans == "yes" {
				CheckAgyCLI(true)
			}
		}

		// 2. Ensure local identity key
		pubkey, err := EnsureLocalSSHKey()
		if err != nil {
			fmt.Printf("[❌] Failed to generate local SSH identity key: %v\n", err)
			return
		}
		_ = pubkey

		// 3. Relocate and install binary to canonical path
		_ = RunInstall()

		// 4. Seed registry
		_ = config.SeedRegistryFromConfig()
		cluster.ReconcileRegistry()

		// 5. Ensure SSH daemon is active
		_, _, _ = CheckSSH()

		fmt.Println("\n[🎉] Primary device initialized successfully!")
		fmt.Println("    • Canonical directory : ~/.gemini/antigravity-cli/bin/agy-sync")
		fmt.Println("    • Identity key        : ~/.ssh/id_ed25519.pub")
		fmt.Println("\n👉 To connect another device (Android, Linux, or Mac), simply run:")
		fmt.Println("   \033[1;36magy-sync pair\033[0m")
		fmt.Println()

	case "3":
		CmdDoctor()

	case "4":
		if err := RunInstall(); err != nil {
			fmt.Printf("[❌] Installation failed: %v\n", err)
		}

	case "5", "q", "exit":
		fmt.Println("Exiting.")
		return

	default:
		fmt.Printf("[❌] Unknown option '%s'. Exiting.\n", choice)
	}
}
