package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"agy-sync/pkg/config"
)

func TestCustomRegistryOperations(t *testing.T) {
	tempGemini, err := os.MkdirTemp("", "gemini_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempGemini)

	oldCustomSyncFile := config.CustomSyncFile
	config.CustomSyncFile = filepath.Join(tempGemini, "custom_sync.json")
	defer func() { config.CustomSyncFile = oldCustomSyncFile }()

	// 1. Initial load on empty
	reg := config.LoadCustomSyncRegistry()
	if len(reg) != 0 {
		t.Fatalf("expected empty registry, got %d items", len(reg))
	}

	// 2. Add items
	now := float64(time.Now().UnixNano()) / 1e9
	reg[".bashrc"] = &config.CustomItem{
		Path:      ".bashrc",
		Type:      "file",
		Deleted:   false,
		UpdatedAt: now,
	}
	reg["scripts"] = &config.CustomItem{
		Path:      "scripts",
		Type:      "dir",
		Deleted:   false,
		UpdatedAt: now,
	}

	if err := config.SaveCustomSyncRegistry(reg); err != nil {
		t.Fatalf("failed to save registry: %v", err)
	}

	// 3. Reload from disk
	reloaded := config.LoadCustomSyncRegistry()
	if len(reloaded) != 2 {
		t.Fatalf("expected 2 items, got %d", len(reloaded))
	}
	if reloaded[".bashrc"] == nil || reloaded[".bashrc"].Type != "file" {
		t.Fatalf("mismatch on .bashrc: %+v", reloaded[".bashrc"])
	}
	if reloaded["scripts"] == nil || reloaded["scripts"].Type != "dir" {
		t.Fatalf("mismatch on scripts: %+v", reloaded["scripts"])
	}

	// 4. Soft delete
	reloaded[".bashrc"].Deleted = true
	reloaded[".bashrc"].UpdatedAt = now + 1.0
	_ = config.SaveCustomSyncRegistry(reloaded)

	// 5. Verify active items
	finalReg := config.LoadCustomSyncRegistry()
	activeCount := 0
	for _, item := range finalReg {
		if !item.Deleted {
			activeCount++
		}
	}
	if activeCount != 1 {
		t.Fatalf("expected 1 active item, got %d", activeCount)
	}
	if !finalReg[".bashrc"].Deleted {
		t.Fatalf(".bashrc should be marked deleted")
	}
}
