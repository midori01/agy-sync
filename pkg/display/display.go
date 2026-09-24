package display

import (
	"fmt"
	"os"
	"os/user"
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
			nodeDisp = fmt.Sprintf("%s ⚡", name)
			statusStr = "🟢 Active"
			hwVal = localIdVal
			if len(hwVal) > 9 {
				hwVal = hwVal[:9]
			}
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
			routeDesc = fmt.Sprintf("Local Loopback (127.0.0.1:%d)", port)
			idDesc = fmt.Sprintf("%s: %s", localIdType, hwVal)
		} else {
			if isDefault {
				role = "Default"
				nodeDisp = fmt.Sprintf("%s ⭐", name)
			} else {
				role = "Peer"
				nodeDisp = name
			}

			pEntry, ok := probeResults[ep]
			if !ok {
				pEntry = probeEntry{
					Probe: DeviceProbeRes{Online: false, Hardware: "offline"},
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
				hwVal = pEntry.Probe.Hardware
				if len(hwVal) > 9 {
					hwVal = hwVal[:9]
				}
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
				if len(hwVal) > 9 {
					hwVal = hwVal[:9]
				}
			}

			routeDesc = fmt.Sprintf("%s (%s:%d)", pEntry.Route.RouteTier, pEntry.Route.EffHost, pEntry.Route.EffPort)
			idDesc = fmt.Sprintf("%s: %s", pEntry.Probe.IdType, hwVal)
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

	// 74-col Bento Matrix Table
	colWidths := []int{10, 7, 13, 6, 9, 10}
	totW := 0
	for _, w := range colWidths {
		totW += w
	}
	totW += len(colWidths)*2 + (len(colWidths) + 1) // 74
	innerW := totW - 4

	title := fmt.Sprintf("Cluster Devices · %d Nodes", len(activeDevices))
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

func ShowItems() {
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
		{"🛡️ SSH Client Config", config.SSHConfigFile, "Block Auto-Sync"},
		{"⚙️ CLI Settings", config.SettingsFile, "SHA-256 + NTP Skew"},
		{"📊 Jetbox Summaries", config.ProtoSummariesFile, "Stream + SHA-256"},
	}

	if fi, err := os.Stat(config.OAuthTokenFile); err == nil && !fi.IsDir() {
		items = append(items, manifestItem{"🎟️ Antigravity Token", config.OAuthTokenFile, "SHA-256 + 0600"})
	}

	if fi, err := os.Stat(config.PoolAccountsFile); err == nil && !fi.IsDir() {
		items = append(items, manifestItem{"👥 Account Pool Config", config.PoolAccountsFile, "Superset Union"})
	}
	if fi, err := os.Stat(config.PoolDir); err == nil && fi.IsDir() {
		items = append(items, manifestItem{"👥 Account Pool Engine", config.PoolDir, "Tar Stream + Auto"})
	}

	items = append(items, []manifestItem{
		{"🗂️ Conversation Index", config.SummariesDBFile, "SQLite Union"},
		{"💬 Conversation DBs", config.ConvDir, "WAL Snapshot"},
		{"🏷️ Session Annotations", config.AnnDir, "Diff Stream"},
		{"📦 Brain Transcripts", config.BrainDir, "Compressed Tar"},
		{"📝 Prompt History", config.HistoryFile, "Chrono Union"},
		{"🛠️ Custom Skills/Configs", config.ConfigDir, "Full-Duplex Tar"},
	}...)

	colWidths := []int{24, 27, 13}
	totW := 0
	for _, w := range colWidths {
		totW += w
	}
	totW += len(colWidths)*2 + (len(colWidths) + 1) // 74
	innerW := totW - 4

	title := fmt.Sprintf("Synchronized Assets Manifest · %d Groups", len(items))
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
}
