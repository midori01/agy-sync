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

func TestFilterActiveLRUSessions(t *testing.T) {
	allKeys := map[string]bool{
		"sess1": true,
		"sess2": true,
		"sess3": true,
		"sess4": true,
		"sess5": true,
	}
	localMap := map[string][2]int64{
		"sess1": {100, 10},
		"sess2": {500, 10}, // #1
		"sess3": {300, 10}, // #3
	}
	remoteMap := map[string][2]int64{
		"sess3": {200, 10},
		"sess4": {400, 10}, // #2
		"sess5": {50, 10},  // #5
	}

	// Case 1: Limit 3 should keep sess2 (500), sess4 (400), sess3 (300)
	active := filterActiveLRUSessions(allKeys, localMap, remoteMap, 3)
	if len(active) != 3 {
		t.Fatalf("Expected 3 active sessions, got %d", len(active))
	}
	if !active["sess2"] || !active["sess4"] || !active["sess3"] {
		t.Fatalf("Expected top 3 sessions [sess2, sess4, sess3], got %+v", active)
	}
	if active["sess1"] || active["sess5"] {
		t.Fatalf("Expected older sessions sess1 and sess5 to be excluded, got %+v", active)
	}

	// Case 2: Limit exceeds count -> all keys preserved
	activeAll := filterActiveLRUSessions(allKeys, localMap, remoteMap, 10)
	if len(activeAll) != 5 {
		t.Fatalf("Expected all 5 sessions preserved, got %d", len(activeAll))
	}

	// Case 3: Commutative deterministic tie-breaker on identical mtime
	// When mtimes are identical, sorting by key (descending) must be strictly identical regardless of
	// which device executes the sync (localMap vs remoteMap inverted).
	tieKeys := map[string]bool{"a_sess": true, "z_sess": true}
	tieLoc := map[string][2]int64{"a_sess": {1000, 100}}
	tieRem := map[string][2]int64{"z_sess": {1000, 100}}
	// Perspective from Device A (local=tieLoc, remote=tieRem):
	activeA := filterActiveLRUSessions(tieKeys, tieLoc, tieRem, 1)
	// Perspective from Device B (local=tieRem, remote=tieLoc):
	activeB := filterActiveLRUSessions(tieKeys, tieRem, tieLoc, 1)

	if len(activeA) != 1 || !activeA["z_sess"] {
		t.Fatalf("Expected deterministic winner 'z_sess' for Device A, got %+v", activeA)
	}
	if len(activeB) != 1 || !activeB["z_sess"] {
		t.Fatalf("Expected deterministic winner 'z_sess' for Device B, got %+v", activeB)
	}
}
