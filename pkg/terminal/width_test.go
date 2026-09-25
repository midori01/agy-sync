package terminal

import (
	"strings"
	"testing"
)

func TestStrWidth(t *testing.T) {
	tests := []struct {
		input    string
		expected int
	}{
		{"hello", 5},
		{"", 0},
		{"\x1b[32mhello\x1b[0m", 5},
		{"⚡", 2},
		{"🟢", 2},
		{"🔒", 2},
		{"🔧", 2},
		{"🎫", 2},
		{"📑", 2},
		{"🔖", 2},
		{"🧰", 2},
		{"你好世界", 8},
		{"Route: Tailnet Mesh (120ms)", 27},
	}

	for _, tc := range tests {
		actual := StrWidth(tc.input)
		if actual != tc.expected {
			t.Errorf("StrWidth(%q) = %d, expected %d", tc.input, actual, tc.expected)
		}
	}
}

func TestPadToExactWidth(t *testing.T) {
	targets := []int{10, 20, 35, 70}
	testStrings := []string{
		"short",
		"This is a longer string that will exceed ten columns",
		"中文测试字符串超过长度",
		"⚡ LAN Direct (50.2ms)",
		"🔒 SSH Client Config",
		"🔧 CLI Settings",
		"🎫 Antigravity Token",
		"📑 Conversation Index",
		"🔖 Session Annotations",
		"🧰 Custom Skills/Configs",
	}

	for _, target := range targets {
		for _, s := range testStrings {
			padded := PadTo(s, target)
			w := StrWidth(padded)
			if w != target {
				t.Errorf("PadTo(%q, %d) resulting visible width %d != target %d (padded=%q)", s, target, w, target, padded)
			}
		}
	}
}

func TestMakeTableDivider(t *testing.T) {
	colWidths := []int{10, 7, 13, 6, 9, 10}
	div := MakeTableDivider(colWidths)
	if !strings.HasPrefix(div, "├") || !strings.HasSuffix(div, "┤") {
		t.Errorf("MakeTableDivider produced invalid borders: %s", div)
	}
}

func TestTruncateWidth(t *testing.T) {
	longStr := "Syncing with node 'xiaomi' (100.103.188.14:8022) · Brain transcripts & artifacts"
	for maxW := 1; maxW <= 100; maxW++ {
		truncated := TruncateWidth(longStr, maxW)
		w := StrWidth(truncated)
		if w > maxW {
			t.Errorf("TruncateWidth(%d) gave width %d > maxW (str=%q)", maxW, w, truncated)
		}
	}
}

func TestGetTerminalWidth(t *testing.T) {
	w := GetTerminalWidth()
	if w <= 0 {
		t.Errorf("GetTerminalWidth returned non-positive value: %d", w)
	}
}

