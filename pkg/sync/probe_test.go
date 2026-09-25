package sync

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCompareFileParity(t *testing.T) {
	// Case 1: Identical size and exact identical mtime -> 0
	if res := CompareFileParity(1000, 500, 1000, 500, 0); res != 0 {
		t.Errorf("Expected 0 for identical file, got %d", res)
	}

	// Case 2: Size difference takes precedence regardless of timestamps
	if res := CompareFileParity(1000, 600, 2000, 500, 0); res != 1 {
		t.Errorf("Expected 1 when local size is larger, got %d", res)
	}
	if res := CompareFileParity(2000, 500, 1000, 600, 0); res != -1 {
		t.Errorf("Expected -1 when remote size is larger, got %d", res)
	}

	// Case 3: Same size, remote clock is 9.5s ahead, mtimes reflect clock skew -> 0 (absorbed)
	skew := 9.5
	remMTime := int64(1000 + int64(skew)) // 1009
	if res := CompareFileParity(1000, 500, remMTime, 500, skew); res != 0 {
		t.Errorf("Expected 0 when difference is within clock skew window, got %d", res)
	}

	// Case 4: Same size, genuine local update beyond tolerance -> 1
	locMTimeNew := int64(1000 + 20) // 20s newer
	if res := CompareFileParity(locMTimeNew, 500, remMTime, 500, skew); res != 1 {
		t.Errorf("Expected 1 for genuine local newer update, got %d", res)
	}

	// Case 5: Same size, genuine remote update beyond tolerance -> -1
	remMTimeNew := int64(1000 + int64(skew) + 20) // 20s newer on remote
	if res := CompareFileParity(1000, 500, remMTimeNew, 500, skew); res != -1 {
		t.Errorf("Expected -1 for genuine remote newer update, got %d", res)
	}
}

func TestGetFileHashCaching(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "agy_hash_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("hello antigravity sync"), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	hash1 := GetFileHash(testFile)
	if hash1 == "" {
		t.Fatal("Expected valid SHA256 hash, got empty string")
	}

	// Second read should hit localHashCache
	hash2 := GetFileHash(testFile)
	if hash1 != hash2 {
		t.Fatalf("Hash mismatch from cache: %s vs %s", hash1, hash2)
	}

	// Modifying content should produce different hash
	if err := os.WriteFile(testFile, []byte("hello antigravity sync v8.8"), 0644); err != nil {
		t.Fatalf("Failed to modify test file: %v", err)
	}
	hash3 := GetFileHash(testFile)
	if hash3 == hash1 {
		t.Fatalf("Hash did not update after file modification")
	}
}
