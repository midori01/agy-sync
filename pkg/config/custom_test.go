package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeCustomPath(t *testing.T) {
	tempHome, err := os.MkdirTemp("", "test_home_*")
	if err != nil {
		t.Fatalf("failed to create temp home: %v", err)
	}
	defer os.RemoveAll(tempHome)

	oldHome := HomeDir
	HomeDir = tempHome
	defer func() { HomeDir = oldHome }()

	// Create test file and test directory inside tempHome
	testFile := filepath.Join(tempHome, ".bashrc")
	_ = os.WriteFile(testFile, []byte("echo hi"), 0644)

	testDir := filepath.Join(tempHome, "scripts")
	_ = os.MkdirAll(testDir, 0755)

	tests := []struct {
		name       string
		input      string
		wantRel    string
		wantIsDir  bool
		wantErr    bool
	}{
		{
			name:      "tilde file",
			input:     "~/.bashrc",
			wantRel:   ".bashrc",
			wantIsDir: false,
			wantErr:   false,
		},
		{
			name:      "tilde directory",
			input:     "~/scripts",
			wantRel:   "scripts",
			wantIsDir: true,
			wantErr:   false,
		},
		{
			name:      "absolute path under home",
			input:     testFile,
			wantRel:   ".bashrc",
			wantIsDir: false,
			wantErr:   false,
		},
		{
			name:      "nested path",
			input:     "~/scripts/sub/test.txt",
			wantRel:   "scripts/sub/test.txt",
			wantIsDir: false,
			wantErr:   false,
		},
		{
			name:      "reject entire home tilde",
			input:     "~",
			wantErr:   true,
		},
		{
			name:      "reject entire home path",
			input:     tempHome,
			wantErr:   true,
		},
		{
			name:      "reject outside home",
			input:     "/tmp/foo_bar_outside",
			wantErr:   true,
		},
		{
			name:      "reject internal gemini",
			input:     "~/.gemini/some_file",
			wantErr:   true,
		},
		{
			name:      "reject constitutional rules",
			input:     "~/GEMINI.md",
			wantErr:   true,
		},
		{
			name:      "reject authorized keys",
			input:     "~/.ssh/authorized_keys",
			wantErr:   true,
		},
		{
			name:      "empty path",
			input:     "",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rel, isDir, err := NormalizeCustomPath(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("NormalizeCustomPath(%q) expected error, got nil (rel=%s)", tt.input, rel)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeCustomPath(%q) unexpected error: %v", tt.input, err)
			}
			if rel != tt.wantRel {
				t.Errorf("NormalizeCustomPath(%q) rel = %q, want %q", tt.input, rel, tt.wantRel)
			}
			if isDir != tt.wantIsDir {
				t.Errorf("NormalizeCustomPath(%q) isDir = %v, want %v", tt.input, isDir, tt.wantIsDir)
			}
		})
	}
}

func TestMergeCustomSyncRegistries(t *testing.T) {
	loc := CustomSyncRegistry{
		".bashrc": {
			Path:      ".bashrc",
			Type:      "file",
			Deleted:   false,
			UpdatedAt: 100.0,
		},
		"scripts": {
			Path:      "scripts",
			Type:      "dir",
			Deleted:   false,
			UpdatedAt: 150.0,
		},
		"old_notes": {
			Path:      "old_notes",
			Type:      "file",
			Deleted:   true,
			UpdatedAt: 200.0,
		},
	}

	rem := CustomSyncRegistry{
		".bashrc": {
			Path:      ".bashrc",
			Type:      "file",
			Deleted:   false,
			UpdatedAt: 90.0,
		},
		"remote_tool": {
			Path:      "remote_tool",
			Type:      "dir",
			Deleted:   false,
			UpdatedAt: 120.0,
		},
		"old_notes": {
			Path:      "old_notes",
			Type:      "file",
			Deleted:   false,
			UpdatedAt: 180.0, // Older than local deletion!
		},
	}

	merged, locChg, remChg := MergeCustomSyncRegistries(loc, rem, 0.0)

	if !locChg {
		t.Errorf("expected localChanged to be true (remote_tool added)")
	}
	if !remChg {
		t.Errorf("expected remoteChanged to be true (.bashrc updated, old_notes deleted)")
	}

	if item, ok := merged[".bashrc"]; !ok || item.UpdatedAt != 100.0 {
		t.Errorf(".bashrc merged item mismatch: %+v", item)
	}
	if item, ok := merged["remote_tool"]; !ok || item.Path != "remote_tool" {
		t.Errorf("remote_tool missing in merged: %+v", item)
	}
	if item, ok := merged["old_notes"]; !ok || !item.Deleted {
		t.Errorf("old_notes should be deleted in merged: %+v", item)
	}
}
