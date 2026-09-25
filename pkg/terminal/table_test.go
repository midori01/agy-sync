package terminal

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func captureOutput(f func()) string {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	f()

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

func TestRenderClusterMatrixSyncedPresence(t *testing.T) {
	results := []*SyncResult{
		{
			PeerName:    "galaxy",
			RouteTier:   "Tailnet",
			RouteLat:    12.3,
			Success:     true,
			Elapsed:     5.2,
			BytesPushed: 1024 * 1024 * 3,
			BytesPulled: 512,
		},
	}

	out := captureOutput(func() {
		RenderClusterMatrix(results, 5.2, 1, 0)
	})

	if !strings.Contains(out, "🟢 Synced") {
		t.Errorf("Expected '🟢 Synced' in table output, but got:\n%s", out)
	}

	if strings.Contains(out, "🟢 Synce ") || strings.Contains(out, "🟢 Synce│") {
		t.Errorf("Found truncated '🟢 Synce' in table output:\n%s", out)
	}
}
