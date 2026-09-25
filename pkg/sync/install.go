package sync

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"agy-sync/pkg/config"
)

// RunInstall installs the current agy-sync executable to its canonical single-source-of-truth
// location (~/.gemini/antigravity-cli/bin/agy-sync) and establishes system-wide symlinks.
func RunInstall() error {
	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("unable to determine current executable path: %w", err)
	}

	canonicalBin := config.AgySyncScript
	canonicalDir := filepath.Dir(canonicalBin)
	if err := os.MkdirAll(canonicalDir, 0755); err != nil {
		return fmt.Errorf("failed to create canonical directory '%s': %w", canonicalDir, err)
	}

	// 1. Copy executable to canonical path if not already running from there
	selfAbs, _ := filepath.Abs(selfPath)
	canonAbs, _ := filepath.Abs(canonicalBin)

	if selfAbs != canonAbs {
		fmt.Printf("[📦] Installing executable to canonical location:\n    %s\n", canonicalBin)
		in, err := os.Open(selfAbs)
		if err != nil {
			return fmt.Errorf("failed to read source binary: %w", err)
		}
		defer in.Close()

		tmpBin := fmt.Sprintf("%s.tmp_inst_%d", canonicalBin, os.Getpid())
		out, err := os.OpenFile(tmpBin, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			return fmt.Errorf("failed to create destination file: %w", err)
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = out.Close()
			_ = os.Remove(tmpBin)
			return fmt.Errorf("failed to copy binary: %w", err)
		}
		_ = out.Sync()
		_ = out.Close()

		if err := os.Rename(tmpBin, canonicalBin); err != nil {
			_ = os.Remove(tmpBin)
			return fmt.Errorf("failed to atomically relocate binary: %w", err)
		}
		_ = os.Chmod(canonicalBin, 0755)
	} else {
		_ = os.Chmod(canonicalBin, 0755)
	}

	// Copy companion cross-architecture binaries if present in source directory or repo
	srcDir := filepath.Dir(selfAbs)
	for _, archName := range []string{"arm64", "amd64"} {
		compName := fmt.Sprintf("agy-sync-linux-%s", archName)
		srcComp := filepath.Join(srcDir, compName)
		dstComp := filepath.Join(canonicalDir, compName)
		if srcAbs, err := filepath.Abs(srcComp); err == nil && srcAbs != dstComp {
			if data, err := os.ReadFile(srcComp); err == nil && len(data) > 0 {
				_ = os.WriteFile(dstComp, data, 0755)
			}
		}
		repoComp := filepath.Join(config.AgySyncGoDir, "bin", compName)
		if data, err := os.ReadFile(repoComp); err == nil && len(data) > 0 {
			_ = os.WriteFile(dstComp, data, 0755)
		}
	}

	// 2. Establish Global Symlinks
	var createdLinks []string
	isTermux := false
	if _, err := os.Stat("/data/data/com.termux/files/usr/bin"); err == nil {
		isTermux = true
		termuxBin := "/data/data/com.termux/files/usr/bin/agy-sync"
		_ = os.Remove(termuxBin)
		if err := os.Symlink(canonicalBin, termuxBin); err == nil {
			createdLinks = append(createdLinks, termuxBin)
		}
	} else {
		localBinDir := filepath.Join(config.HomeDir, ".local", "bin")
		_ = os.MkdirAll(localBinDir, 0755)
		localSym := filepath.Join(localBinDir, "agy-sync")
		_ = os.Remove(localSym)
		if err := os.Symlink(canonicalBin, localSym); err == nil {
			createdLinks = append(createdLinks, localSym)
		}

		// Try /usr/local/bin if writable or running as root
		usrLocalSym := "/usr/local/bin/agy-sync"
		if err := os.Remove(usrLocalSym); err == nil || os.IsNotExist(err) {
			if err := os.Symlink(canonicalBin, usrLocalSym); err == nil {
				createdLinks = append(createdLinks, usrLocalSym)
			}
		}
	}

	// 3. Ensure PATH Environment is Configured
	if !isTermux {
		pathEnv := os.Getenv("PATH")
		localBinDir := filepath.Join(config.HomeDir, ".local", "bin")
		if !strings.Contains(pathEnv, localBinDir) && !strings.Contains(pathEnv, filepath.Dir(canonicalBin)) {
			// Append to ~/.bashrc or ~/.zshrc
			rcFile := filepath.Join(config.HomeDir, ".bashrc")
			if shell := os.Getenv("SHELL"); strings.Contains(shell, "zsh") {
				rcFile = filepath.Join(config.HomeDir, ".zshrc")
			}
			exportLine := fmt.Sprintf("\nexport PATH=\"%s:$PATH\"\n", localBinDir)
			if f, err := os.OpenFile(rcFile, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644); err == nil {
				_, _ = f.WriteString(exportLine)
				_ = f.Close()
			}
		}
	}

	fmt.Printf("[✅] Successfully installed agy-sync (v%s)!\n", config.Version)
	if len(createdLinks) > 0 {
		for _, link := range createdLinks {
			fmt.Printf("    • Global shortcut: %s -> %s\n", link, canonicalBin)
		}
	}
	fmt.Println("    • You can now run 'agy-sync' from any terminal directory.")
	return nil
}
