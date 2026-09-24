package terminal

import (
	"regexp"
	"strings"
)

var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*m`)

var wideEmojis = map[rune]bool{
	'⚡': true, '⭐': true, '🟢': true, '🔴': true, '💬': true, '📦': true,
	'🏷': true, '📜': true, '🧠': true, '🎟': true, '🗂': true, '👥': true,
	'📝': true, '🛠': true, '⚙': true, '📊': true, '✨': true, '🎉': true,
	'ℹ': true, '🚀': true, '🔑': true, '🛡': true, '🪪': true, '🏊': true,
	'🐹': true,
}

// SanitizeText replaces tabs and newlines with spaces and strips control chars.
func SanitizeText(s string) string {
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// StrWidth calculates visible terminal width accounting for wide chars and ANSI codes.
func StrWidth(s string) int {
	clean := ansiRegex.ReplaceAllString(s, "")
	clean = strings.ReplaceAll(clean, "\ufe0f", "")

	w := 0
	for _, ch := range clean {
		if ch == '\t' {
			w += 1
		} else if ch == '\r' || ch == '\n' || ch < 0x20 || (0x7F <= ch && ch <= 0x9F) {
			continue
		} else if wideEmojis[ch] {
			w += 2
		} else if (0x1100 <= ch && ch <= 0x115F) ||
			(0x2E80 <= ch && ch <= 0xA4CF) ||
			(0xAC00 <= ch && ch <= 0xD7A3) ||
			(0xF900 <= ch && ch <= 0xFAFF) ||
			(0xFE30 <= ch && ch <= 0xFE4F) ||
			(0xFF01 <= ch && ch <= 0xFF60) ||
			(0xFFE0 <= ch && ch <= 0xFFE6) ||
			(0x1F300 <= ch && ch <= 0x1FAFF) {
			w += 2
		} else {
			w += 1
		}
	}
	return w
}

// PadTo pads or aligns a string to targetWidth using visual terminal width.
func PadTo(s string, targetWidth int, align ...string) string {
	s = SanitizeText(s)
	currentW := StrWidth(s)
	pad := targetWidth - currentW
	if pad <= 0 {
		return s
	}

	mode := "left"
	if len(align) > 0 && align[0] != "" {
		mode = align[0]
	}

	switch mode {
	case "right":
		return strings.Repeat(" ", pad) + s
	case "center":
		leftPad := pad / 2
		rightPad := pad - leftPad
		return strings.Repeat(" ", leftPad) + s + strings.Repeat(" ", rightPad)
	default:
		return s + strings.Repeat(" ", pad)
	}
}

// MakeTableDivider creates divider row, e.g. ├────────────┼─────────┼...┤
func MakeTableDivider(colWidths []int, seps ...string) string {
	left := "├"
	mid := "┼"
	right := "┤"
	if len(seps) >= 3 {
		left, mid, right = seps[0], seps[1], seps[2]
	}

	sections := make([]string, len(colWidths))
	for i, w := range colWidths {
		sections[i] = strings.Repeat("─", w+2)
	}
	return left + strings.Join(sections, mid) + right
}
