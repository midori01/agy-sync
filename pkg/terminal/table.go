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
	PeerName    string
	RouteTier   string
	RouteLat    float64
	ClockSkew   float64
	Elapsed     float64
	Success     bool
	HasWarnings bool
	HasFailures bool
	Warnings    []ChangeItem
	Failures    []ChangeItem
	Error       string
	Changes     []ChangeItem
	Unchanged   []string
	DBPushed    int
	DBPulled    int
	ArtPushed   int
	ArtPulled   int
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
		cand := pStr[:12] + "..." + pStr[12+excess:]
		if StrWidth(cand) <= limit {
			return cand
		}
	}
	if StrWidth(pStr) > limit {
		runes := []rune(pStr)
		for len(runes) > 0 && StrWidth(string(runes)+"...") > limit {
			runes = runes[:len(runes)-1]
		}
		return string(runes) + "..."
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
	if StrWidth(rightMeta) > innerW-12 {
		runes := []rune(rightMeta)
		for len(runes) > 0 && StrWidth(string(runes)+"...") > innerW-12 {
			runes = runes[:len(runes)-1]
		}
		rightMeta = string(runes) + "..."
	}
	maxLeft := innerW - StrWidth(rightMeta) - 1
	if StrWidth(leftMeta) > maxLeft {
		runes := []rune(leftMeta)
		for len(runes) > 0 && StrWidth(string(runes)+"...") > maxLeft {
			runes = runes[:len(runes)-1]
		}
		leftMeta = string(runes) + "..."
	}
	spaceMeta := innerW - StrWidth(leftMeta) - StrWidth(rightMeta)
	if spaceMeta < 0 {
		spaceMeta = 0
	}
	metaRow := fmt.Sprintf("│ %s%s%s │", leftMeta, strings.Repeat(" ", spaceMeta), rightMeta)

	var lines []string
	lines = append(lines, top, metaRow)

	if !res.Success || res.HasFailures {
		lines = append(lines, divider)
		errMsg := res.Error
		if errMsg == "" && len(res.Failures) > 0 {
			errMsg = fmt.Sprintf("%s: %s", res.Failures[0].Name, res.Failures[0].Desc)
		} else if errMsg == "" {
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
			desc := ch.Desc
			if StrWidth(lStr) > innerW/2 {
				runes := []rune(lStr)
				for len(runes) > 0 && StrWidth(string(runes)+"...") > innerW/2 {
					runes = runes[:len(runes)-1]
				}
				lStr = string(runes) + "..."
			}
			maxDesc := innerW - StrWidth(lStr) - 1
			if StrWidth(desc) > maxDesc {
				runes := []rune(desc)
				for len(runes) > 0 && StrWidth(string(runes)+"...") > maxDesc {
					runes = runes[:len(runes)-1]
				}
				desc = string(runes) + "..."
			}
			sp := innerW - StrWidth(lStr) - StrWidth(desc)
			if sp < 0 {
				sp = 0
			}
			lines = append(lines, fmt.Sprintf("│ %s%s%s │", lStr, strings.Repeat(" ", sp), desc))
		}
		lines = append(lines, divider)
		footer := fmt.Sprintf("✨ All states synchronized (%d changed, %d identical)", len(res.Changes), len(res.Unchanged))
		if res.HasWarnings {
			footer = fmt.Sprintf("⚠️ Synchronized with warnings (%d changed, %d identical, %d diverged)", len(res.Changes)-len(res.Warnings), len(res.Unchanged), len(res.Warnings))
		}
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
		if res.Success && !res.HasFailures {
			if res.HasWarnings {
				resStr = "🟡 Diverg"
			} else {
				resStr = "🟢 Synced"
			}
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

	warnCnt := 0
	for _, res := range results {
		if res.Success && res.HasWarnings {
			warnCnt++
		}
	}

	lines = append(lines, MakeTableDivider(colWidths, "├", "┴", "┤"))
	footer := fmt.Sprintf("✨ Cluster Sync: %d/%d nodes synced in %.2fs (%d failed)", successCnt, len(results), totalElapsed, failCnt)
	if warnCnt > 0 && failCnt == 0 {
		footer = fmt.Sprintf("⚠️ Cluster Sync: %d/%d synced, %d diverged in %.2fs", successCnt-warnCnt, len(results), warnCnt, totalElapsed)
	} else if failCnt > 0 {
		footer = fmt.Sprintf("❌ Cluster Sync: %d/%d succeeded, %d failed in %.2fs", successCnt, len(results), failCnt, totalElapsed)
	}
	lines = append(lines, fmt.Sprintf("│ %s │", PadTo(footer, innerW)))
	lines = append(lines, fmt.Sprintf("└%s┘", strings.Repeat("─", totW-2)))

	fmt.Println(strings.Join(lines, "\n"))

	if warnCnt > 0 {
		fmt.Println()
		for _, res := range results {
			if res.Success && res.HasWarnings {
				pName := res.PeerName
				for _, w := range res.Warnings {
					fmt.Printf("[⚠️] Warning on node '%s' (%s): %s\n", pName, w.Name, w.Desc)
				}
			}
		}
	}

	if failCnt > 0 {
		fmt.Println()
		for _, res := range results {
			if !res.Success || res.HasFailures {
				pName := res.PeerName
				if pName == "" {
					pName = "unknown"
				}
				err := res.Error
				if err == "" && len(res.Failures) > 0 {
					err = fmt.Sprintf("%s: %s", res.Failures[0].Name, res.Failures[0].Desc)
				} else if err == "" {
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
