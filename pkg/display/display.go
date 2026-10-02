package display

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"agy-sync/pkg/cluster"
	"agy-sync/pkg/config"
	"agy-sync/pkg/network"
	"agy-sync/pkg/ssh"
	"agy-sync/pkg/terminal"
)

type DeviceProbeRes struct {
	Online   bool
	Latency  float64
	IdType   string
	Hardware string
	User     string
	LanIPs   []string
}

type DeviceRouteInfo struct {
	EffHost   string
	EffPort   int
	RouteTier string
	RouteLat  float64
}

type DeviceDisplayItem struct {
	Endpoint string
	Data     *config.DeviceEntry
	IsLocal  bool
	Alias    string
}

func truncateVisual(s string, maxW int) string {
	if terminal.StrWidth(s) <= maxW {
		return s
	}
	runes := []rune(s)
	for len(runes) > 0 && terminal.StrWidth(string(runes)) > maxW {
		runes = runes[:len(runes)-1]
	}
	return string(runes)
}

func ShowDevices(verbose bool) {
	registry := config.LoadRegistry()
	localIdType, localIdVal := config.GetLocalIdentity()
	defaultAlias, _, _ := cluster.LoadDefaultTarget()

	var activeDevices []DeviceDisplayItem
	for ep, d := range registry {
		if d.Deleted {
			continue
		}
		_, h, _, _ := config.ParseEndpoint(ep, 8022)
		if h == "127.0.0.1" || h == "localhost" || h == "0.0.0.0" || h == "::1" {
			continue
		}
		isLocal := config.IsLocalEndpoint(ep, d)
		al := strings.ToLower(strings.TrimSpace(d.Alias))
		if al == "" {
			al = strings.ToLower(strings.TrimSpace(d.NativeName))
			if al == "" {
				al = ep
			}
		}
		activeDevices = append(activeDevices, DeviceDisplayItem{
			Endpoint: ep,
			Data:     d,
			IsLocal:  isLocal,
			Alias:    al,
		})
	}

	curUser := os.Getenv("USER")
	if u, err := user.Current(); err == nil && u.Username != "" {
		curUser = u.Username
	}

	sort.Slice(activeDevices, func(i, j int) bool {
		if activeDevices[i].IsLocal != activeDevices[j].IsLocal {
			return activeDevices[i].IsLocal // local first
		}
		return activeDevices[i].Alias < activeDevices[j].Alias
	})

	var remoteItems []DeviceDisplayItem
	for _, item := range activeDevices {
		if !item.IsLocal {
			remoteItems = append(remoteItems, item)
		}
	}

	type probeEntry struct {
		Probe DeviceProbeRes
		Route DeviceRouteInfo
	}
	probeResults := make(map[string]probeEntry)
	var mu sync.Mutex

	if len(remoteItems) > 0 {
		var spinner *terminal.Spinner
		if !verbose {
			spinner = terminal.NewSpinner(fmt.Sprintf("Probing %d remote cluster nodes", len(remoteItems)), true)
			spinner.Start()
		}

		var wg sync.WaitGroup
		for _, item := range remoteItems {
			wg.Add(1)
			go func(it DeviceDisplayItem) {
				defer wg.Done()
				_, host, port, _ := config.ParseEndpoint(it.Endpoint, 8022)
				effH, effP, rTier, rLat := network.ResolveFastestEndpoint(host, port, it.Alias)

				cachedHw := strings.TrimSpace(it.Data.NativeName)
				cachedIdType := strings.TrimSpace(it.Data.IdType)
				if cachedIdType == "" {
					cachedIdType = "Host"
				}
				cachedUser := strings.TrimSpace(it.Data.User)

				var pRes DeviceProbeRes
				if !verbose && cachedHw != "" && !strings.EqualFold(cachedHw, "unknown") {
					if strings.Contains(rTier, "LAN") {
						pRes = DeviceProbeRes{
							Online:   true,
							Latency:  rLat,
							IdType:   cachedIdType,
							Hardware: cachedHw,
							User:     cachedUser,
							LanIPs:   it.Data.LanIps,
						}
					} else {
						_, rawH, rawP, _ := config.ParseEndpoint(effH, effP)
						ok, bannerLat, _ := network.ProbeSSHBanner(rawH, rawP, 1800*time.Millisecond)
						if ok {
							if rLat == 0 {
								rLat = bannerLat
							}
							pRes = DeviceProbeRes{
								Online:   true,
								Latency:  bannerLat,
								IdType:   cachedIdType,
								Hardware: cachedHw,
								User:     cachedUser,
								LanIPs:   it.Data.LanIps,
							}
						} else {
							pRes = DeviceProbeRes{
								Online:   false,
								Latency:  0.0,
								IdType:   cachedIdType,
								Hardware: "offline",
								User:     cachedUser,
								LanIPs:   nil,
							}
						}
					}
				} else {
					// Fallback deep probe
					_, rawH, rawP, _ := config.ParseEndpoint(effH, effP)
					ok, bannerLat, banner := network.ProbeSSHBanner(rawH, rawP, 2500*time.Millisecond)
					pRes = DeviceProbeRes{
						Online:   ok,
						Latency:  bannerLat,
						IdType:   cachedIdType,
						Hardware: cachedHw,
						User:     cachedUser,
						LanIPs:   it.Data.LanIps,
					}
					_ = banner
				}

				mu.Lock()
				probeResults[it.Endpoint] = probeEntry{
					Probe: pRes,
					Route: DeviceRouteInfo{EffHost: effH, EffPort: effP, RouteTier: rTier, RouteLat: rLat},
				}
				mu.Unlock()
			}(item)
		}

		wg.Wait()
		if spinner != nil {
			spinner.Stop()
		}
	}

	// Re-sort activeDevices to align with topology: Local first, then LAN Direct (lowest latency), then Tailnet (lowest latency)
	sort.Slice(activeDevices, func(i, j int) bool {
		if activeDevices[i].IsLocal != activeDevices[j].IsLocal {
			return activeDevices[i].IsLocal
		}
		tierI := ""
		tierJ := ""
		latI := 0.0
		latJ := 0.0
		if pI, ok := probeResults[activeDevices[i].Endpoint]; ok {
			tierI = pI.Route.RouteTier
			latI = pI.Route.RouteLat
		}
		if pJ, ok := probeResults[activeDevices[j].Endpoint]; ok {
			tierJ = pJ.Route.RouteTier
			latJ = pJ.Route.RouteLat
		}
		lanI := strings.Contains(tierI, "LAN")
		lanJ := strings.Contains(tierJ, "LAN")
		if lanI && !lanJ {
			return true
		}
		if !lanI && lanJ {
			return false
		}
		if latI > 0 && latJ > 0 && latI != latJ {
			return latI < latJ
		}
		return activeDevices[i].Alias < activeDevices[j].Alias
	})

	registryChanged := false
	var tableRows [][]string

	type detailCard struct {
		Name     string
		Role     string
		DispEP   string
		LanIPs   []string
		Route    string
		Hardware string
		Status   string
	}
	var detailCards []detailCard

	for _, item := range activeDevices {
		ep := item.Endpoint
		data := item.Data
		name := item.Alias
		userStr, host, port, _ := config.ParseEndpoint(ep, 8022)
		isLocal := item.IsLocal
		isDefault := strings.EqualFold(name, defaultAlias)

		var nodeDisp, role, routeDisp, pingStr, hwVal, statusStr string
		var dispEP, routeDesc, idDesc string

		if isLocal {
			role = "Local"
			statusStr = "🟢 Active"
			hwVal = truncateVisual(localIdVal, 9)
			nodeDisp = fmt.Sprintf("%s ⚡", truncateVisual(name, 7))
			pingStr = "—"
			routeDisp = "Loopback"
			uDisp := userStr
			if uDisp == "" {
				uDisp = curUser
			}
			dispEP = fmt.Sprintf("%s@%s:%d", uDisp, host, port)
			localLanNets := network.GetLocalLanNetworks()
			var localLanIPs []string
			for _, n := range localLanNets {
				localLanIPs = append(localLanIPs, n.IP)
			}
			if len(localLanIPs) > 0 {
				sort.Strings(localLanIPs)
				curLans := make([]string, len(data.LanIps))
				copy(curLans, data.LanIps)
				sort.Strings(curLans)
				if strings.Join(localLanIPs, ",") != strings.Join(curLans, ",") {
					data.LanIps = localLanIPs
					data.LanUpdatedAt = float64(time.Now().UnixNano()) / 1e9
					registryChanged = true
				}
			}
			if localIdType == "" {
				localIdType = "Host"
			}
			routeDesc = fmt.Sprintf("Local Loopback (127.0.0.1:%d)", port)
			idDesc = fmt.Sprintf("%-10s%s", localIdType+":", localIdVal)
		} else {
			if isDefault {
				role = "Default"
				nodeDisp = fmt.Sprintf("%s ⭐", truncateVisual(name, 7))
			} else {
				role = "Peer"
				nodeDisp = truncateVisual(name, 10)
			}

			pEntry, ok := probeResults[ep]
			if !ok {
				idType := strings.TrimSpace(data.IdType)
				if idType == "" {
					idType = "Host"
				}
				pEntry = probeEntry{
					Probe: DeviceProbeRes{Online: false, Hardware: "offline", IdType: idType},
					Route: DeviceRouteInfo{EffHost: host, EffPort: port, RouteTier: "Tailnet Mesh"},
				}
			}

			uDisp := userStr
			if uDisp == "" {
				uDisp = pEntry.Probe.User
			}
			if uDisp == "" {
				uDisp = data.User
			}
			if uDisp != "" {
				dispEP = fmt.Sprintf("%s@%s:%d", uDisp, host, port)
			} else {
				dispEP = fmt.Sprintf("%s:%d", host, port)
			}

			if len(pEntry.Probe.LanIPs) > 0 {
				oldSet := strings.Join(data.LanIps, ",")
				newSet := strings.Join(pEntry.Probe.LanIPs, ",")
				if oldSet != newSet {
					data.LanIps = pEntry.Probe.LanIPs
					data.LanUpdatedAt = float64(time.Now().UnixNano()) / 1e9
					registryChanged = true
				}
			}

			if pEntry.Probe.Online {
				statusStr = "🟢 Online"
				hwVal = truncateVisual(pEntry.Probe.Hardware, 9)
				if strings.Contains(pEntry.Route.RouteTier, "LAN") {
					routeDisp = "⚡ LAN Direct"
					if 0 < pEntry.Route.RouteLat && pEntry.Route.RouteLat < 100 {
						pingStr = fmt.Sprintf("%.1fms", pEntry.Route.RouteLat)
					} else if pEntry.Route.RouteLat >= 100 {
						pingStr = fmt.Sprintf("%dms", int(pEntry.Route.RouteLat))
					} else {
						pingStr = "—"
					}
				} else {
					routeDisp = "🌐 Tailnet"
					if 0 < pEntry.Probe.Latency && pEntry.Probe.Latency < 100 {
						pingStr = fmt.Sprintf("%.1fms", pEntry.Probe.Latency)
					} else if pEntry.Probe.Latency >= 100 {
						pingStr = fmt.Sprintf("%dms", int(pEntry.Probe.Latency))
					} else {
						pingStr = "—"
					}
				}

				if pEntry.Probe.User != "" && data.User != pEntry.Probe.User {
					data.User = pEntry.Probe.User
					registryChanged = true
				}
			} else {
				statusStr = "🔴 Offline"
				routeDisp = "🌐 Tailnet"
				if strings.Contains(pEntry.Route.RouteTier, "LAN") {
					routeDisp = "⚡ LAN Direct"
				}
				pingStr = "—"
				hwVal = data.NativeName
				if hwVal == "" {
					hwVal = "unknown"
				}
				hwVal = truncateVisual(hwVal, 9)
			}

			rawHw := pEntry.Probe.Hardware
			if rawHw == "" || rawHw == "offline" {
				rawHw = data.NativeName
				if rawHw == "" {
					rawHw = "unknown"
				}
			}
			idType := pEntry.Probe.IdType
			if idType == "" {
				idType = data.IdType
				if idType == "" {
					idType = "Host"
				}
			}
			routeDesc = fmt.Sprintf("%s (%s:%d)", pEntry.Route.RouteTier, pEntry.Route.EffHost, pEntry.Route.EffPort)
			idDesc = fmt.Sprintf("%-10s%s", idType+":", rawHw)
		}

		tableRows = append(tableRows, []string{nodeDisp, role, routeDisp, pingStr, hwVal, statusStr})
		if verbose {
			detailCards = append(detailCards, detailCard{
				Name:     name,
				Role:     role,
				DispEP:   dispEP,
				LanIPs:   data.LanIps,
				Route:    routeDesc,
				Hardware: idDesc,
				Status:   statusStr,
			})
		}
	}

	if registryChanged {
		_ = config.SaveRegistry(registry)
		ssh.ReconcileSSHConfig(registry)
	}

	// Bento Matrix Table
	colWidths := []int{10, 7, 13, 6, 9, 10}
	termW := terminal.GetTerminalWidth()
	if termW > 0 && termW < 74 {
		if termW < 60 {
			colWidths = []int{8, 5, 9, 5, 7, 8} // ~57 cols
		} else {
			colWidths = []int{9, 6, 11, 5, 8, 9} // ~65 cols
		}
	}
	totW := 0
	for _, w := range colWidths {
		totW += w
	}
	totW += len(colWidths)*2 + (len(colWidths) + 1)
	innerW := totW - 4

	title := fmt.Sprintf("Cluster Devices · %d Nodes", len(activeDevices))
	if terminal.StrWidth("["+title+"]") > totW-8 {
		title = terminal.TruncateWidth(title, totW-10)
	}
	barLen := totW - 5 - terminal.StrWidth("["+title+"]")
	if barLen < 2 {
		barLen = 2
	}
	top := fmt.Sprintf("┌─ [%s] %s┐", title, strings.Repeat("─", barLen))
	headers := []string{"Node", "Role", "Route", "Ping", "Hardware", "Status"}

	var headerCells []string
	for i, h := range headers {
		headerCells = append(headerCells, terminal.PadTo(h, colWidths[i]))
	}
	headerRow := fmt.Sprintf("│ %s │", strings.Join(headerCells, " │ "))

	var lines []string
	lines = append(lines, top, headerRow, terminal.MakeTableDivider(colWidths))

	for _, row := range tableRows {
		var rowCells []string
		for i, v := range row {
			rowCells = append(rowCells, terminal.PadTo(v, colWidths[i]))
		}
		lines = append(lines, fmt.Sprintf("│ %s │", strings.Join(rowCells, " │ ")))
	}

	lines = append(lines, terminal.MakeTableDivider(colWidths, "├", "┴", "┤"))
	quickLine := "Quick: agy-sync · agy-sync all · agy-sync <peer> · agy-sync ssh <peer>"
	lines = append(lines, fmt.Sprintf("│ %s │", terminal.PadTo(quickLine, innerW)))
	lines = append(lines, fmt.Sprintf("└%s┘", strings.Repeat("─", totW-2)))

	fmt.Printf("\n%s\n\n", strings.Join(lines, "\n"))

	if verbose && len(detailCards) > 0 {
		fmt.Println("[Device Network Details]")
		for _, card := range detailCards {
			cTitle := fmt.Sprintf("%s · %s", card.Name, card.Role)
			cBarLen := totW - 5 - terminal.StrWidth("["+cTitle+"]")
			if cBarLen < 2 {
				cBarLen = 2
			}
			cTop := fmt.Sprintf("┌─ [%s] %s┐", cTitle, strings.Repeat("─", cBarLen))
			var cLines []string
			cLines = append(cLines, cTop)
			cLines = append(cLines, fmt.Sprintf("│ %s │", terminal.PadTo(fmt.Sprintf("Endpoint: %s", card.DispEP), innerW)))
			if len(card.LanIPs) > 0 {
				cLines = append(cLines, fmt.Sprintf("│ %s │", terminal.PadTo(fmt.Sprintf("LAN IPs:  %s", strings.Join(card.LanIPs, ", ")), innerW)))
			}
			cLines = append(cLines, fmt.Sprintf("│ %s │", terminal.PadTo(fmt.Sprintf("Route:    %s", card.Route), innerW)))
			cLines = append(cLines, fmt.Sprintf("│ %s │", terminal.PadTo(card.Hardware, innerW)))
			cLines = append(cLines, fmt.Sprintf("│ %s │", terminal.PadTo(fmt.Sprintf("Status:   %s", card.Status), innerW)))
			cLines = append(cLines, fmt.Sprintf("└%s┘", strings.Repeat("─", totW-2)))
			fmt.Println(strings.Join(cLines, "\n"))
		}
	}
}

func ShowItems(verbose ...bool) {
	isVerbose := false
	if len(verbose) > 0 && verbose[0] {
		isVerbose = true
	}

	type manifestItem struct {
		Name   string
		Path   string
		Engine string
	}

	items := []manifestItem{
		{"📜 Constitutional Rules", config.RulesFile, "SHA-256 + NTP Skew"},
		{"🧠 Session Memory Relay", config.HandoffFile, "SHA-256 + NTP Skew"},
		{"⚡ Synchronizer Engine", config.AgySyncScript, "SHA-256 + NTP Skew"},
		{"🌐 Device Registry", config.DevicesJSONFile, "LWW + NTP Skew"},
		{"🔑 SSH Authorized Keys", config.AuthKeysFile, "Set Union"},
		{"🔒 SSH Client Config", config.SSHConfigFile, "Block Auto-Sync"},
		{"🔧 CLI Settings", config.SettingsFile, "SHA-256 + NTP Skew"},
		{"📊 Jetbox Summaries", config.ProtoSummariesFile, "Stream + SHA-256"},
	}

	if fi, err := os.Stat(config.OAuthTokenFile); err == nil && !fi.IsDir() {
		items = append(items, manifestItem{"🎫 Antigravity Token", config.OAuthTokenFile, "SHA-256 + 0600"})
	}

	if fi, err := os.Stat(config.PoolAccountsFile); err == nil && !fi.IsDir() {
		items = append(items, manifestItem{"👥 Account Pool Config", config.PoolAccountsFile, "Superset Union"})
	}
	if fi, err := os.Stat(config.PoolDir); err == nil && fi.IsDir() {
		items = append(items, manifestItem{"👥 Account Pool Engine", config.PoolDir, "Tar Stream + Auto"})
	}
	if fi, err := os.Stat(config.AgySyncGoDir); err == nil && fi.IsDir() {
		items = append(items, manifestItem{"🐹 Go Engine Source", config.AgySyncGoDir, "Tar Stream + Git"})
	}

	items = append(items, []manifestItem{
		{"📑 Conversation Index", config.SummariesDBFile, "SQLite Union"},
		{"💬 Conversation DBs", config.ConvDir, "WAL Snapshot"},
		{"🔖 Session Annotations", config.AnnDir, "Diff Stream"},
		{"📦 Brain Transcripts", config.BrainDir, "Compressed Tar"},
		{"📝 Prompt History", config.HistoryFile, "Chrono Union"},
		{"🧰 Custom Skills/Configs", config.ConfigDir, "Full-Duplex Tar"},
	}...)

	customReg := config.LoadCustomSyncRegistry()
	var activeCustom []*config.CustomItem
	for _, it := range customReg {
		if !it.Deleted {
			activeCustom = append(activeCustom, it)
		}
	}
	sort.Slice(activeCustom, func(i, j int) bool {
		return activeCustom[i].Path < activeCustom[j].Path
	})
	for _, it := range activeCustom {
		icon := "📄"
		eng := "Custom File (SHA-256)"
		if it.Type == "dir" {
			icon = "📁"
			eng = "Custom Dir (Tar Mesh)"
		}
		locP := filepath.Join(config.HomeDir, it.Path)
		items = append(items, manifestItem{
			Name:   fmt.Sprintf("%s Custom: ~/%s", icon, it.Path),
			Path:   locP,
			Engine: eng,
		})
	}

	colWidths := []int{24, 27, 13}
	termW := terminal.GetTerminalWidth()
	if termW > 0 && termW < 74 {
		if termW < 60 {
			colWidths = []int{18, 19, 10} // ~54 cols
		} else {
			colWidths = []int{21, 23, 11} // ~62 cols
		}
	}
	totW := 0
	for _, w := range colWidths {
		totW += w
	}
	totW += len(colWidths)*2 + (len(colWidths) + 1)
	innerW := totW - 4

	title := fmt.Sprintf("Synchronized Assets Manifest · %d Groups", len(items))
	if terminal.StrWidth("["+title+"]") > totW-8 {
		title = terminal.TruncateWidth(title, totW-10)
	}
	barLen := totW - 5 - terminal.StrWidth("["+title+"]")
	if barLen < 2 {
		barLen = 2
	}
	top := fmt.Sprintf("┌─ [%s] %s┐", title, strings.Repeat("─", barLen))
	headers := []string{"Asset Category", "Local Path / Target", "Status / Size"}

	var headerCells []string
	for i, h := range headers {
		headerCells = append(headerCells, terminal.PadTo(h, colWidths[i]))
	}
	headerRow := fmt.Sprintf("│ %s │", strings.Join(headerCells, " │ "))

	var lines []string
	lines = append(lines, top, headerRow, terminal.MakeTableDivider(colWidths))

	for _, item := range items {
		pathDisp := terminal.CompactPath(item.Path, colWidths[1])
		statusStr := "✗ Missing"

		if fi, err := os.Stat(item.Path); err == nil {
			if !fi.IsDir() {
				sz := fi.Size()
				szStr := fmt.Sprintf("%.1f KB", float64(sz)/1024.0)
				if sz >= 1024*1024 {
					szStr = fmt.Sprintf("%.1f MB", float64(sz)/(1024.0*1024.0))
				}
				statusStr = fmt.Sprintf("✓ %s", szStr)
			} else {
				if entries, err := os.ReadDir(item.Path); err == nil {
					statusStr = fmt.Sprintf("✓ %d items", len(entries))
				} else {
					statusStr = "✓ In Place"
				}
			}
		}

		row := []string{item.Name, pathDisp, statusStr}
		var rowCells []string
		for i, v := range row {
			rowCells = append(rowCells, terminal.PadTo(v, colWidths[i]))
		}
		lines = append(lines, fmt.Sprintf("│ %s │", strings.Join(rowCells, " │ ")))
	}

	lines = append(lines, terminal.MakeTableDivider(colWidths, "├", "┴", "┤"))
	footerText := "✨ Engine: Hybrid SHA256 + NTP Skew · SQLite WAL · Tar Mesh"
	lines = append(lines, fmt.Sprintf("│ %s │", terminal.PadTo(footerText, innerW)))
	lines = append(lines, fmt.Sprintf("└%s┘", strings.Repeat("─", totW-2)))

	fmt.Printf("\n%s\n\n", strings.Join(lines, "\n"))

	if isVerbose && len(items) > 0 {
		fmt.Println("[Synchronized Asset Details]")
		for _, item := range items {
			cTitle := item.Name
			cBarLen := totW - 5 - terminal.StrWidth("["+cTitle+"]")
			if cBarLen < 2 {
				cBarLen = 2
			}
			cTop := fmt.Sprintf("┌─ [%s] %s┐", cTitle, strings.Repeat("─", cBarLen))
			var cLines []string
			cLines = append(cLines, cTop)

			dispPath := item.Path
			if strings.HasPrefix(dispPath, config.HomeDir) {
				dispPath = "~" + strings.TrimPrefix(dispPath, config.HomeDir)
			}
			dispPath = terminal.CompactPath(dispPath, innerW-10)

			cLines = append(cLines, fmt.Sprintf("│ %s │", terminal.PadTo(fmt.Sprintf("%-10s%s", "Target:", dispPath), innerW)))
			cLines = append(cLines, fmt.Sprintf("│ %s │", terminal.PadTo(fmt.Sprintf("%-10s%s", "Engine:", item.Engine), innerW)))

			fi, err := os.Stat(item.Path)
			if err != nil {
				cLines = append(cLines, fmt.Sprintf("│ %s │", terminal.PadTo(fmt.Sprintf("%-10s🔴 Missing (not created)", "Status:"), innerW)))
			} else {
				if fi.IsDir() {
					entries, err := os.ReadDir(item.Path)
					count := 0
					if err == nil {
						count = len(entries)
					}
					cLines = append(cLines, fmt.Sprintf("│ %s │", terminal.PadTo(fmt.Sprintf("%-10s🟢 Directory (%d entries, mode %04o)", "Status:", count, fi.Mode().Perm()), innerW)))
				} else {
					sz := fi.Size()
					szStr := fmt.Sprintf("%.1f KB", float64(sz)/1024.0)
					if sz >= 1024*1024 {
						szStr = fmt.Sprintf("%.1f MB", float64(sz)/(1024.0*1024.0))
					}
					cLines = append(cLines, fmt.Sprintf("│ %s │", terminal.PadTo(fmt.Sprintf("%-10s🟢 File (%s, mode %04o)", "Status:", szStr, fi.Mode().Perm()), innerW)))
				}
				mTimeStr := fi.ModTime().Format("2006-01-02 15:04:05 MST")
				cLines = append(cLines, fmt.Sprintf("│ %s │", terminal.PadTo(fmt.Sprintf("%-10s%s", "Updated:", mTimeStr), innerW)))
			}

			cLines = append(cLines, fmt.Sprintf("└%s┘", strings.Repeat("─", totW-2)))
			fmt.Println(strings.Join(cLines, "\n"))
		}
	}
}
