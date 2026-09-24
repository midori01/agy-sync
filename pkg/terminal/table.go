package terminal

import (
	"fmt"
	"strings"

	"agy-sync/pkg/config"
)

type ChangeItem struct {
	Name string
	Desc string
	Icon string
}

type SyncResult struct {
	PeerName   string
	RouteTier  string
	RouteLat   float64
	ClockSkew  float64
	Elapsed    float64
	Success    bool
	Error      string
	Changes    []ChangeItem
	Unchanged  []string
	DBPushed   int
	DBPulled   int
	ArtPushed  int
	ArtPulled  int
}

func CompactPath(pStr string, maxW ...int) string {
	limit := 27
	if len(maxW) > 0 && maxW[0] > 0 {
		limit = maxW[0]
	}

	pStr = strings.ReplaceAll(pStr, config.HomeDir, "~")
	if StrWidth(pStr) <= limit {
		return pStr
	}
	pStr = strings.ReplaceAll(pStr, "~/.gemini/antigravity-cli/", "~/.gemini/.../")
	if StrWidth(pStr) <= limit {
		return pStr
	}
	excess := StrWidth(pStr) - limit + 3
	if excess > 0 && len(pStr) > 12+excess {
		return pStr[:12] + "..." + pStr[12+excess:]
	}
	return pStr
}

func RenderSingleNodeCard(res *SyncResult, totalW ...int) {
	totW := 74
	if len(totalW) > 0 && totalW[0] > 0 {
		totW = totalW[0]
	}
	innerW := totW - 4

	peerName := res.PeerName
	if peerName == "" {
		peerName = "peer"
	}
	title := fmt.Sprintf("[Sync Dashboard · %s]", peerName)
	barLen := totW - 5 - StrWidth(title)
	if barLen < 2 {
		barLen = 2
	}
	top := fmt.Sprintf("┌─ %s %s┐", title, strings.Repeat("─", barLen))
	divider := fmt.Sprintf("├%s┤", strings.Repeat("─", totW-2))
	bottom := fmt.Sprintf("└%s┘", strings.Repeat("─", totW-2))

	routeTier := res.RouteTier
	if routeTier == "" {
		routeTier = "Tailnet Mesh"
	}
	latStr := ""
	if res.RouteLat > 0 {
		latStr = fmt.Sprintf(" (%.1fms)", res.RouteLat)
	}
	skewSign := ""
	if res.ClockSkew >= 0 {
		skewSign = "+"
	}

	leftMeta := fmt.Sprintf("Route: %s%s", routeTier, latStr)
	rightMeta := fmt.Sprintf("Duration: %.2fs · Skew: %s%.3fs", res.Elapsed, skewSign, res.ClockSkew)
	spaceMeta := innerW - StrWidth(leftMeta) - StrWidth(rightMeta)
	if spaceMeta < 2 {
		spaceMeta = 2
	}
	metaRow := fmt.Sprintf("│ %s%s%s │", leftMeta, strings.Repeat(" ", spaceMeta), rightMeta)

	var lines []string
	lines = append(lines, top, metaRow)

	if !res.Success {
		lines = append(lines, divider)
		errMsg := res.Error
		if errMsg == "" {
			errMsg = "Sync interrupted or failed"
		}
		lines = append(lines, fmt.Sprintf("│ %s │", PadTo("❌ "+errMsg, innerW)))
	} else if len(res.Changes) == 0 {
		footer := fmt.Sprintf("✨ 100%% In Sync (%d assets verified identical)", len(res.Unchanged))
		lines = append(lines, fmt.Sprintf("│ %s │", PadTo(footer, innerW)))
	} else {
		lines = append(lines, divider)
		for _, ch := range res.Changes {
			lStr := fmt.Sprintf("%s %s", ch.Icon, ch.Name)
			sp := innerW - StrWidth(lStr) - StrWidth(ch.Desc)
			if sp < 2 {
				sp = 2
			}
			lines = append(lines, fmt.Sprintf("│ %s%s%s │", lStr, strings.Repeat(" ", sp), ch.Desc))
		}
		lines = append(lines, divider)
		footer := fmt.Sprintf("✨ All states synchronized (%d changed, %d identical)", len(res.Changes), len(res.Unchanged))
		lines = append(lines, fmt.Sprintf("│ %s │", PadTo(footer, innerW)))
	}

	lines = append(lines, bottom)
	fmt.Println(strings.Join(lines, "\n"))
}

func RenderClusterMatrix(results []*SyncResult, totalElapsed float64, successCnt, failCnt int) {
	colWidths := []int{8, 13, 6, 9, 6, 6, 4}
	headers := []string{"Node", "Route", "Ping", "Result", "DBs", "Arts", "Time"}

	totW := 0
	for _, w := range colWidths {
		totW += w
	}
	totW += len(colWidths)*2 + (len(colWidths) + 1) // 74
	innerW := totW - 4

	title := "[Cluster Sync Dashboard]"
	barLen := totW - 5 - StrWidth(title)
	if barLen < 2 {
		barLen = 2
	}
	top := fmt.Sprintf("┌─ %s %s┐", title, strings.Repeat("─", barLen))

	var headerCells []string
	for i, h := range headers {
		headerCells = append(headerCells, PadTo(h, colWidths[i]))
	}
	headerRow := fmt.Sprintf("│ %s │", strings.Join(headerCells, " │ "))

	var lines []string
	lines = append(lines, top, headerRow, MakeTableDivider(colWidths))

	for _, res := range results {
		pName := res.PeerName
		if len(pName) > 8 {
			pName = pName[:8]
		}
		rTier := res.RouteTier
		rStr := "🌐 Tailnet"
		if strings.Contains(rTier, "LAN") {
			rStr = "⚡ LAN Direct"
		}
		lat := res.RouteLat
		pingStr := "—"
		if 0 < lat && lat < 100 {
			pingStr = fmt.Sprintf("%.1fms", lat)
		} else if lat >= 100 {
			pingStr = fmt.Sprintf("%dms", int(lat))
		}

		resStr := "❌ Failed"
		if res.Success {
			resStr = "🟢 Synced"
		}

		dbStr := "—"
		if res.Success {
			if res.DBPushed == 0 && res.DBPulled == 0 {
				dbStr = "ident"
			} else {
				s := fmt.Sprintf("+%d/+%d", res.DBPushed, res.DBPulled)
				if len(s) > 6 {
					s = fmt.Sprintf("+%d/%d", res.DBPushed, res.DBPulled)
				}
				dbStr = s
			}
		}

		artStr := "—"
		if res.Success {
			if res.ArtPushed == 0 && res.ArtPulled == 0 {
				artStr = "ident"
			} else {
				s := fmt.Sprintf("+%d/+%d", res.ArtPushed, res.ArtPulled)
				if len(s) > 6 {
					s = fmt.Sprintf("+%d/%d", res.ArtPushed, res.ArtPulled)
				}
				artStr = s
			}
		}

		timeStr := fmt.Sprintf("%ds", int(res.Elapsed))
		if res.Elapsed < 10 {
			timeStr = fmt.Sprintf("%.1fs", res.Elapsed)
		}

		rowVals := []string{pName, rStr, pingStr, resStr, dbStr, artStr, timeStr}
		var rowCells []string
		for i, v := range rowVals {
			rowCells = append(rowCells, PadTo(v, colWidths[i]))
		}
		lines = append(lines, fmt.Sprintf("│ %s │", strings.Join(rowCells, " │ ")))
	}

	lines = append(lines, MakeTableDivider(colWidths, "├", "┴", "┤"))
	footer := fmt.Sprintf("✨ Cluster Sync: %d/%d nodes synced in %.2fs (%d failed)", successCnt, len(results), totalElapsed, failCnt)
	lines = append(lines, fmt.Sprintf("│ %s │", PadTo(footer, innerW)))
	lines = append(lines, fmt.Sprintf("└%s┘", strings.Repeat("─", totW-2)))

	fmt.Println(strings.Join(lines, "\n"))

	if failCnt > 0 {
		fmt.Println()
		for _, res := range results {
			if !res.Success {
				pName := res.PeerName
				if pName == "" {
					pName = "unknown"
				}
				err := res.Error
				if err == "" {
					err = "Unknown error"
				}
				fmt.Printf("[❌] Failed node '%s': %s\n", pName, err)
			}
		}
	}
}

func RenderAliasCard(title, target, mappingStr, endpoint string, bRes map[string]bool) {
	totW := 74
	innerW := totW - 4

	barLen := totW - 5 - StrWidth("["+title+"]")
	if barLen < 2 {
		barLen = 2
	}
	top := fmt.Sprintf("┌─ [%s] %s┐", title, strings.Repeat("─", barLen))

	var lines []string
	lines = append(lines, top)
	lines = append(lines, fmt.Sprintf("│ %s │", PadTo(fmt.Sprintf("Target:   %s", target), innerW)))
	lines = append(lines, fmt.Sprintf("│ %s │", PadTo(fmt.Sprintf("Mapping:  %s", mappingStr), innerW)))
	lines = append(lines, fmt.Sprintf("│ %s │", PadTo(fmt.Sprintf("Endpoint: %s", endpoint), innerW)))

	if len(bRes) > 0 {
		succCnt := 0
		for _, ok := range bRes {
			if ok {
				succCnt++
			}
		}
		lines = append(lines, fmt.Sprintf("├%s┤", strings.Repeat("─", totW-2)))
		statusMsg := fmt.Sprintf("Mesh:     %d/%d online peers updated", succCnt, len(bRes))
		lines = append(lines, fmt.Sprintf("│ %s │", PadTo(statusMsg, innerW)))
	}

	lines = append(lines, fmt.Sprintf("└%s┘", strings.Repeat("─", totW-2)))
	fmt.Println(strings.Join(lines, "\n"))
}
