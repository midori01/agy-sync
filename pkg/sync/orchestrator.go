package sync

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"agy-sync/pkg/cluster"
	"agy-sync/pkg/config"
	"agy-sync/pkg/network"
	"agy-sync/pkg/ssh"
	"agy-sync/pkg/terminal"
)

type PreResolvedRoute struct {
	Host      string
	Port      int
	RouteTier string
	Latency   float64
}

func SyncWithPeer(host string, port int, peerName string, verbose bool, isBatch bool, preResolved *PreResolvedRoute, extSpinner *terminal.Spinner) *terminal.SyncResult {
	title := fmt.Sprintf("node '%s' (%s:%d)", peerName, host, port)
	if peerName == "" {
		title = fmt.Sprintf("%s:%d", host, port)
	}

	activeSpinner := extSpinner
	createdSpinner := false

	if !isBatch {
		if verbose {
			fmt.Printf("[🚀] Initiating Antigravity bidirectional sync with %s...\n", title)
		} else {
			activeSpinner = terminal.NewSpinner(fmt.Sprintf("Syncing with %s", title), true)
			activeSpinner.Start()
			createdSpinner = true
		}
	}

	t0 := time.Now()
	stepStatus := func(msg string) {
		if activeSpinner != nil {
			activeSpinner.SetStatus(msg)
		}
	}

	stepStatus("Resolving route")
	origHost, origPort := host, port
	var effHost string
	var effPort int
	var routeTier string
	var routeLat float64

	if preResolved != nil {
		effHost = preResolved.Host
		effPort = preResolved.Port
		routeTier = preResolved.RouteTier
		routeLat = preResolved.Latency
	} else {
		effHost, effPort, routeTier, routeLat = network.ResolveFastestEndpoint(host, port, peerName)
	}
	host, port = effHost, effPort

	if verbose {
		if host != origHost || port != origPort {
			fmt.Printf("[⚡] Route optimized: %s -> %s:%d (%.1fms)\n", routeTier, host, port, routeLat)
		} else {
			fmt.Printf("[🌐] Active route: %s -> %s:%d\n", routeTier, host, port)
		}
	}

	// 1. Pipeline Handshake
	stepStatus("Pipeline handshake")
	handshakeCmd := `echo READY && echo "HOME:$HOME" && echo "USER:$(whoami 2>/dev/null || echo "$USER")" && echo "ARCH:$(uname -m 2>/dev/null || echo unknown)" && echo "COMMIT:$(command -v agy-sync >/dev/null 2>&1 && agy-sync --commit 2>/dev/null || echo NONE)" && echo "TS_IP:$(command -v tailscale >/dev/null 2>&1 && tailscale ip -4 2>/dev/null || true)" && echo "LAN_IPS:$((ifconfig 2>/dev/null | awk '/^[a-zA-Z0-9]/ {iface=$1} /inet (addr:)?/ {if (iface ~ /^(wlan|eth|en|ra|br|swlan|usb|rndis)/) print $2}' | sed 's/addr://'; ip -o -4 addr show 2>/dev/null | awk '$2 ~ /^(wlan|eth|en|ra|br|swlan|usb|rndis)/ {print $4}' | cut -d/ -f1) | tr '\n' ',')" && (date +%s.%N 2>/dev/null || date +%s)`

	connectTo := 8
	if host != origHost {
		connectTo = 3
	}

	stdout, stderr, _, err := ssh.RunSSHCmd(host, port, handshakeCmd, false, connectTo)
	t1 := time.Now()
	rtt := t1.Sub(t0).Seconds()

	if err != nil || !strings.Contains(stdout, "READY") {
		if host != origHost {
			if verbose {
				fmt.Printf("[🔄] LAN direct test unconfirmed, falling back to Tailnet Mesh %s:%d...\n", origHost, origPort)
			}
			host, port = origHost, origPort
			routeTier = "Tailnet Mesh"
			routeLat = 0.0
			stepStatus("Handshake fallback to Tailnet")

			stdout, stderr, _, err = ssh.RunSSHCmd(host, port, handshakeCmd, false, 8)
			t1 = time.Now()
			rtt = t1.Sub(t0).Seconds()
			if err != nil || !strings.Contains(stdout, "READY") {
				if createdSpinner && activeSpinner != nil {
					activeSpinner.Stop()
				}
				errMsg := strings.TrimSpace(stderr)
				if errMsg == "" {
					errMsg = "Handshake check failed"
				}
				res := &terminal.SyncResult{
					Success:   false,
					PeerName:  peerName,
					RouteTier: routeTier,
					RouteLat:  routeLat,
					Elapsed:   time.Since(t0).Seconds(),
					Error:     errMsg,
				}
				if !isBatch {
					terminal.RenderSingleNodeCard(res)
				}
				return res
			}
		} else {
			if createdSpinner && activeSpinner != nil {
				activeSpinner.Stop()
			}
			errMsg := strings.TrimSpace(stderr)
			if errMsg == "" {
				errMsg = "Handshake check failed"
			}
			res := &terminal.SyncResult{
				Success:   false,
				PeerName:  peerName,
				RouteTier: routeTier,
				RouteLat:  routeLat,
				Elapsed:   time.Since(t0).Seconds(),
				Error:     errMsg,
			}
			if !isBatch {
				terminal.RenderSingleNodeCard(res)
			}
			return res
		}
	}

	remHome := config.HomeDir
	remUser := ""
	remArch := "aarch64"
	remCommit := ""
	remTsIP := ""
	var remLanIPs []string
	var remoteTS *float64

	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "HOME:") {
			h := strings.TrimSpace(strings.TrimPrefix(line, "HOME:"))
			if h != "" {
				remHome = h
			}
		} else if strings.HasPrefix(line, "USER:") {
			remUser = strings.TrimSpace(strings.TrimPrefix(line, "USER:"))
		} else if strings.HasPrefix(line, "ARCH:") {
			remArch = strings.TrimSpace(strings.TrimPrefix(line, "ARCH:"))
		} else if strings.HasPrefix(line, "COMMIT:") {
			remCommit = strings.TrimSpace(strings.TrimPrefix(line, "COMMIT:"))
		} else if strings.HasPrefix(line, "TS_IP:") {
			remTsIP = strings.TrimSpace(strings.TrimPrefix(line, "TS_IP:"))
		} else if strings.HasPrefix(line, "LAN_IPS:") {
			raw := strings.TrimSpace(strings.TrimPrefix(line, "LAN_IPS:"))
			for _, token := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
				token = strings.TrimSpace(token)
				if token != "" && token != "127.0.0.1" && token != "localhost" && token != "0.0.0.0" && token != "::1" {
					if regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`).MatchString(token) {
						remLanIPs = append(remLanIPs, token)
					}
				}
			}
		} else if line != "READY" {
			if f, err := strconv.ParseFloat(line, 64); err == nil {
				remoteTS = &f
			}
		}
	}

	clockSkew := 0.0
	if remoteTS != nil {
		localMidpoint := float64(t0.UnixNano())/1e9 + (rtt / 2.0)
		clockSkew = float64(int((*remoteTS-localMidpoint)*1000+0.5)) / 1000.0
	}

	// Update peer's user identity & LAN IPs in registry
	registry := config.LoadRegistry()
	regUpdated := false
	for epKey, d := range registry {
		_, eHost, _, _ := config.ParseEndpoint(epKey, 8022)
		if eHost == origHost || epKey == fmt.Sprintf("%s:%d", origHost, origPort) || (peerName != "" && d.Alias == peerName) {
			if remUser != "" && d.User != remUser {
				d.User = remUser
				regUpdated = true
			}
			if len(remLanIPs) > 0 {
				oldSet := strings.Join(d.LanIps, ",")
				newSet := strings.Join(remLanIPs, ",")
				if oldSet != newSet {
					d.LanIps = remLanIPs
					d.LanUpdatedAt = float64(time.Now().UnixNano()) / 1e9
					regUpdated = true
				}
			}
			if regUpdated {
				d.UpdatedAt = float64(time.Now().UnixNano()) / 1e9
			}
		}
	}
	_ = remTsIP
	if regUpdated {
		_ = config.SaveRegistry(registry)
		cluster.ReconcileRegistry()
	}

	// 1.3 Batch-probe Tracked Single Files in 1 RTT
	stepStatus("Probing single files")
	remPoolCfg := config.ToRemotePath(config.PoolAccountsFile, remHome)
	batchTargets := []string{
		config.ToRemotePath(config.RulesFile, remHome),
		config.ToRemotePath(config.HandoffFile, remHome),
		config.ToRemotePath(config.AgySyncScript, remHome),
		config.ToRemotePath(config.DevicesJSONFile, remHome),
		config.ToRemotePath(config.AuthKeysFile, remHome),
		config.ToRemotePath(config.OAuthTokenFile, remHome),
		remPoolCfg,
		config.ToRemotePath(config.SettingsFile, remHome),
		config.ToRemotePath(config.ProtoSummariesFile, remHome),
		config.ToRemotePath(config.HistoryFile, remHome),
	}
	probeCache := BatchProbeRemoteFiles(host, port, batchTargets)

	var changes []terminal.ChangeItem
	var unchanged []string

	recordAsset := func(name, desc, icon string, isChanged bool) {
		if isChanged {
			changes = append(changes, terminal.ChangeItem{Name: name, Desc: desc, Icon: icon})
		} else {
			unchanged = append(unchanged, name)
		}
	}

	convPushed := 0
	convPulled := 0
	brainPushed := 0
	brainPulled := 0

	// 1.5 Sync Cluster Device Registry
	stepStatus("Cluster registry & keys")
	regStat := SyncClusterRegistry(host, port, clockSkew, remHome, probeCache)
	recordAsset("Cluster Registry", regStat, "🪪", regStat == "pushed" || regStat == "pulled" || strings.Contains(regStat, "merged"))

	// 1.6 Sync SSH Authorized Keys
	authStat := SyncAuthorizedKeys(host, port, remHome, probeCache)
	recordAsset("SSH Authorized Keys", authStat, "🔑", authStat == "pushed" || authStat == "pulled" || strings.Contains(authStat, "merged"))

	// 2. Sync Rules
	stepStatus("Constitutional rules & memory")
	rulesStat := SyncSingleFile(config.RulesFile, config.ToRemotePath(config.RulesFile, remHome), host, port, "GEMINI.md", clockSkew, probeCache)
	recordAsset("Constitutional Rules", rulesStat, "📜", rulesStat == "pushed" || rulesStat == "pulled")

	// 3. Sync Memory Relay
	handoffStat := SyncSingleFile(config.HandoffFile, config.ToRemotePath(config.HandoffFile, remHome), host, port, "session_handoff.md", clockSkew, probeCache)
	recordAsset("Memory Relay", handoffStat, "🧠", handoffStat == "pushed" || handoffStat == "pulled")

	// 3.5 Sync Engine
	stepStatus("Synchronizer engine")
	binStat := SyncEngine(host, port, remHome, remArch, remCommit, clockSkew, probeCache)
	recordAsset("Synchronizer Engine", binStat, "⚡", binStat == "pushed" || binStat == "pulled")

	// 3.6 Sync Token
	stepStatus("Account pool & tokens")
	remToken := config.ToRemotePath(config.OAuthTokenFile, remHome)
	hasToken := false
	if _, err := os.Stat(config.OAuthTokenFile); err == nil || probeCache[remToken] != nil {
		hasToken = true
	}
	if hasToken {
		tokStat := SyncSingleFile(config.OAuthTokenFile, remToken, host, port, "antigravity-oauth-token", clockSkew, probeCache, "600")
		recordAsset("Antigravity Token", tokStat, "🎟️", tokStat == "pushed" || tokStat == "pulled")
	}

	// 3.7 Sync Account Pool Config
	hasPoolCfg := false
	if _, err := os.Stat(config.PoolAccountsFile); err == nil || probeCache[remPoolCfg] != nil {
		hasPoolCfg = true
	}
	if hasPoolCfg {
		poolStat := SyncPoolAccounts(host, port, remHome, clockSkew, probeCache)
		if poolStat != "none" {
			recordAsset("Account Pool", poolStat, "👥", strings.Contains(poolStat, "pushed") || strings.Contains(poolStat, "pulled") || strings.Contains(poolStat, "merged"))
		}
	}

	// 3.8 Sync Account Pool Engine
	engineStat := SyncPoolEngine(host, port, remHome, clockSkew)
	if engineStat != "none" {
		recordAsset("Account Pool Engine", engineStat, "🏊", engineStat == "pushed" || engineStat == "pulled" || engineStat == "installed & ready")
	}

	// 3.9 Sync Go Synchronizer Source Code
	goSrcStat := SyncAgySyncGo(host, port, remHome, clockSkew)
	if goSrcStat != "none" {
		recordAsset("Go Synchronizer Source", goSrcStat, "🐹", goSrcStat == "pushed" || goSrcStat == "pulled" || goSrcStat == "installed & ready")
	}

	// 4. Sync CLI Settings & Proto Summaries
	stepStatus("CLI settings & summaries")
	remSettings := config.ToRemotePath(config.SettingsFile, remHome)
	hasSettings := false
	if _, err := os.Stat(config.SettingsFile); err == nil || probeCache[remSettings] != nil {
		hasSettings = true
	}
	if hasSettings {
		setStat := SyncSingleFile(config.SettingsFile, remSettings, host, port, "settings.json", clockSkew, probeCache, "600")
		recordAsset("CLI Settings", setStat, "⚙️", setStat == "pushed" || setStat == "pulled")
	}

	if _, err := os.Stat(config.ProtoSummariesFile); err == nil {
		protoStat := SyncSingleFile(config.ProtoSummariesFile, config.ToRemotePath(config.ProtoSummariesFile, remHome), host, port, "jetbox_summaries_proto.pb", clockSkew, probeCache)
		recordAsset("Jetbox Summaries", protoStat, "📊", protoStat == "pushed" || protoStat == "pulled")
	}

	// 5. Sync Conversation Summaries
	stepStatus("Conversation index")
	sumsStat := SyncConversationSummaries(host, port, clockSkew, remHome)
	recordAsset("Conversation Index", sumsStat, "🗂️", strings.Contains(sumsStat, "pushed") || strings.Contains(sumsStat, "pulled") || strings.Contains(sumsStat, "merged"))

	// 6. Sync Conversation Databases
	stepStatus("Conversation databases")
	convPushed, convPulled = SyncConversationDBs(host, port, clockSkew, remHome)
	convChanged := (convPushed > 0 || convPulled > 0)
	convDesc := "identical"
	if convChanged {
		convDesc = fmt.Sprintf("+%d pushed, +%d pulled", convPushed, convPulled)
	}
	recordAsset("Conversation Databases", convDesc, "💬", convChanged)

	// 7. Sync Annotations
	stepStatus("Conversation annotations")
	annPushed, annPulled := SyncAnnotations(host, port, clockSkew, remHome)
	annChanged := (annPushed > 0 || annPulled > 0)
	annDesc := "identical"
	if annChanged {
		annDesc = fmt.Sprintf("+%d pushed, +%d pulled", annPushed, annPulled)
	}
	recordAsset("Conversation Annotations", annDesc, "🏷️", annChanged)

	// 8. Sync Brain Artifacts & Transcripts
	stepStatus("Brain transcripts & artifacts")
	brainPushed, brainPulled = SyncBrainSessions(host, port, clockSkew, remHome)
	brainChanged := (brainPushed > 0 || brainPulled > 0)
	brainDesc := "identical"
	if brainChanged {
		brainDesc = fmt.Sprintf("+%d pushed, +%d pulled", brainPushed, brainPulled)
	}
	recordAsset("Brain Transcripts & Artifacts", brainDesc, "📦", brainChanged)

	// 9. Sync History Lines
	stepStatus("Prompt history")
	histStat := SyncHistoryJsonl(host, port, remHome, probeCache)
	recordAsset("Prompt History", histStat, "📝", strings.Contains(histStat, "pushed") || strings.Contains(histStat, "pulled") || strings.Contains(histStat, "merged"))

	// 10. Sync Configs & Skills
	stepStatus("Skills & configurations")
	cfgPushed, cfgPulled := SyncConfigsAndSkills(host, port, clockSkew, remHome)
	cfgChanged := (cfgPushed > 0 || cfgPulled > 0)
	cfgDesc := "identical"
	if cfgChanged {
		cfgDesc = fmt.Sprintf("+%d pushed, +%d pulled", cfgPushed, cfgPulled)
	}
	recordAsset("Skills & Configs", cfgDesc, "🛠️", cfgChanged)

	stepStatus("Finalizing")
	elapsed := float64(int(time.Since(t0).Seconds()*100+0.5)) / 100.0

	if createdSpinner && activeSpinner != nil {
		activeSpinner.Stop()
	}

	result := &terminal.SyncResult{
		Success:   true,
		PeerName:  peerName,
		RouteTier: routeTier,
		RouteLat:  routeLat,
		ClockSkew: clockSkew,
		Elapsed:   elapsed,
		Changes:   changes,
		Unchanged: unchanged,
		DBPushed:  convPushed,
		DBPulled:  convPulled,
		ArtPushed: brainPushed,
		ArtPulled: brainPulled,
	}

	if !isBatch {
		terminal.RenderSingleNodeCard(result)
	}
	return result
}

func SyncAllPeers(verbose bool) {
	devices := cluster.LoadDevices()
	localName := config.GetLocalDeviceName()
	localEp := config.GetLocalEndpoint()

	var remoteDevices []struct {
		Name      string
		TargetSSH string
		Port      int
	}
	for name, dev := range devices {
		if !config.IsLocalTarget(dev.TargetSSH, dev.Port, localEp, localName) {
			remoteDevices = append(remoteDevices, struct {
				Name      string
				TargetSSH string
				Port      int
			}{Name: name, TargetSSH: dev.TargetSSH, Port: dev.Port})
		}
	}

	if len(remoteDevices) == 0 {
		fmt.Println("[⚠️] No remote peers registered in cluster.")
		return
	}
	fmt.Printf("[🌐] Starting multi-device cluster sync across %d remote peer(s)...\n", len(remoteDevices))

	// Phase 1: Micro-Concurrency Network Probes
	tProbeStart := time.Now()
	var probeSpinner *terminal.Spinner
	if !verbose {
		probeSpinner = terminal.NewSpinner("Probing fastest routes concurrently", true)
		probeSpinner.Start()
	}

	type probedRoute struct {
		Name      string
		OrigHost  string
		OrigPort  int
		EffHost   string
		EffPort   int
		RouteTier string
		RouteLat  float64
	}

	var probedRoutes []probedRoute
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, d := range remoteDevices {
		wg.Add(1)
		go func(name, targetSSH string, port int) {
			defer wg.Done()
			effH, effP, rTier, rLat := network.ResolveFastestEndpoint(targetSSH, port, name)
			mu.Lock()
			probedRoutes = append(probedRoutes, probedRoute{
				Name:      name,
				OrigHost:  targetSSH,
				OrigPort:  port,
				EffHost:   effH,
				EffPort:   effP,
				RouteTier: rTier,
				RouteLat:  rLat,
			})
			mu.Unlock()
		}(d.Name, d.TargetSSH, d.Port)
	}

	wg.Wait()
	probeElapsed := time.Since(tProbeStart).Seconds()
	if probeSpinner != nil {
		probeSpinner.Stop(fmt.Sprintf("\033[32m✓\033[0m Probing fastest routes concurrently... done \033[90m(%.2fs)\033[0m", probeElapsed))
	}

	// Sort peers: LAN first, then lowest latency, then alphabetical
	sort.Slice(probedRoutes, func(i, j int) bool {
		lanI := strings.Contains(probedRoutes[i].RouteTier, "LAN")
		lanJ := strings.Contains(probedRoutes[j].RouteTier, "LAN")
		if lanI && !lanJ {
			return true
		}
		if !lanI && lanJ {
			return false
		}
		if probedRoutes[i].RouteLat != probedRoutes[j].RouteLat {
			return probedRoutes[i].RouteLat < probedRoutes[j].RouteLat
		}
		return probedRoutes[i].Name < probedRoutes[j].Name
	})

	for _, r := range probedRoutes {
		latStr := ""
		if r.RouteLat > 0 {
			latStr = fmt.Sprintf(" (%.1fms)", r.RouteLat)
		}
		fmt.Printf("    • %s: %s%s\n", r.Name, r.RouteTier, latStr)
	}

	// Phase 2: Safe Sequential Synchronization
	var results []*terminal.SyncResult
	totalT0 := time.Now()
	successCnt := 0
	failCnt := 0
	totalNodes := len(probedRoutes)

	for idx, r := range probedRoutes {
		latStr := ""
		if r.RouteLat > 0 {
			latStr = fmt.Sprintf(" · %.1fms", r.RouteLat)
		}
		nodePrefix := fmt.Sprintf("[%d/%d] Syncing %s (%s%s)", idx+1, totalNodes, r.Name, r.RouteTier, latStr)

		var nodeSpinner *terminal.Spinner
		if !verbose {
			nodeSpinner = terminal.NewSpinner(nodePrefix, true)
			nodeSpinner.Start()
		} else {
			fmt.Printf("\n[🔄] ═══ [%d/%d] Syncing node '%s' (%s%s) ═══\n", idx+1, totalNodes, r.Name, r.RouteTier, latStr)
		}

		route := &PreResolvedRoute{
			Host:      r.EffHost,
			Port:      r.EffPort,
			RouteTier: r.RouteTier,
			Latency:   r.RouteLat,
		}

		res := SyncWithPeer(r.OrigHost, r.OrigPort, r.Name, verbose, true, route, nodeSpinner)
		results = append(results, res)

		var finalStatus string
		if res.Success {
			successCnt++
			finalStatus = fmt.Sprintf("\033[32m✓\033[0m %s... done \033[90m(%.2fs)\033[0m", nodePrefix, res.Elapsed)
		} else {
			failCnt++
			finalStatus = fmt.Sprintf("\033[31m✗\033[0m %s... failed \033[90m(%.2fs)\033[0m", nodePrefix, res.Elapsed)
		}

		if nodeSpinner != nil {
			nodeSpinner.Stop(finalStatus)
		} else if !verbose {
			fmt.Println(finalStatus)
		}
	}

	totalElapsed := time.Since(totalT0).Seconds()
	fmt.Println()

	// Phase 3: Bento Matrix Table Dashboard
	terminal.RenderClusterMatrix(results, totalElapsed, successCnt, failCnt)
}

func PushSettingsToPeers(targetAlias string) {
	if targetAlias == "-h" || targetAlias == "--help" {
		fmt.Println(`
Usage: agy-sync push-settings [alias]

Arguments:
  alias                 Optional specific peer alias (e.g. sjc1, galaxy).
                        If omitted, pushes settings.json to all remote peers in cluster.`)
		return
	}

	if _, err := os.Stat(config.SettingsFile); err != nil {
		fmt.Println("[❌] No local settings.json found to push.")
		return
	}

	now := time.Now()
	_ = os.Chtimes(config.SettingsFile, now, now)
	_ = os.Chmod(config.SettingsFile, 0600)

	devices := cluster.LoadDevices()
	localEp := config.GetLocalEndpoint()
	localName := config.GetLocalDeviceName()

	type pushPeer struct {
		Alias string
		Host  string
		Port  int
	}
	var peersToPush []pushPeer

	if targetAlias != "" {
		ep, matchedAlias := ssh.FindEndpoint(targetAlias)
		if ep != "" {
			if config.IsLocalEndpoint(ep, nil) {
				fmt.Printf("[⚠️] Target '%s' is this local device.\n", targetAlias)
				return
			}
			_, _, p, tSSH := config.ParseEndpoint(ep, 8022)
			peersToPush = append(peersToPush, pushPeer{Alias: matchedAlias, Host: tSSH, Port: p})
		} else if dev, ok := devices[targetAlias]; ok {
			peersToPush = append(peersToPush, pushPeer{Alias: targetAlias, Host: dev.TargetSSH, Port: dev.Port})
		} else {
			fmt.Printf("[❌] Peer '%s' not found in cluster.\n", targetAlias)
			return
		}
	} else {
		for alias, dev := range devices {
			if !config.IsLocalTarget(dev.TargetSSH, dev.Port, localEp, localName) {
				peersToPush = append(peersToPush, pushPeer{Alias: alias, Host: dev.TargetSSH, Port: dev.Port})
			}
		}
	}

	if len(peersToPush) == 0 {
		fmt.Println("[⚠️] No remote peers to push settings to.")
		return
	}

	fmt.Printf("[⚙️] Pushing local settings.json to %d peer(s)...\n", len(peersToPush))
	spinner := terminal.NewSpinner(fmt.Sprintf("Pushing settings.json baseline across %d peer(s)", len(peersToPush)), true)
	spinner.Start()

	t0 := time.Now()
	localContent, _ := os.ReadFile(config.SettingsFile)
	localMTime := now.Unix()
	successCount := 0
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, p := range peersToPush {
		wg.Add(1)
		go func(peer pushPeer) {
			defer wg.Done()
			effH, effP, _, _ := network.ResolveFastestEndpoint(peer.Host, peer.Port, peer.Alias)
			cmdStr := fmt.Sprintf("%s echo \"HOME:$HOME\" && (date +%%s.%%N 2>/dev/null || date +%%s)", config.PathPrefix)
			stdout, _, _, err := ssh.RunSSHCmd(effH, effP, cmdStr, false)
			if err != nil {
				return
			}
			remHome := config.HomeDir
			skew := 0.0
			for _, line := range strings.Split(stdout, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "HOME:") {
					remHome = strings.TrimSpace(strings.TrimPrefix(line, "HOME:"))
				} else {
					if f, err := strconv.ParseFloat(line, 64); err == nil {
						skew = f - float64(time.Now().UnixNano())/1e9
					}
				}
			}
			remSettings := config.ToRemotePath(config.SettingsFile, remHome)
			remTargetMTime := localMTime + int64(skew)
			remParent := filepath.Dir(remSettings)
			tmpRemote := fmt.Sprintf("%s.tmp_push_%d_%s", remSettings, time.Now().Unix(), randomToken(3))

			pushCmd := fmt.Sprintf("mkdir -p '%s' && (cat > '%s' && chmod 600 '%s' && touch -d @%d '%s' && mv -f '%s' '%s' && touch -d @%d '%s' || (rm -f '%s' 2>/dev/null; false))",
				remParent, tmpRemote, tmpRemote, remTargetMTime, tmpRemote, tmpRemote, remSettings, remTargetMTime, remSettings, tmpRemote)

			sshArgs := ssh.GetSSHCmd(effH, effP, pushCmd, true)
			proc := exec.Command("ssh", sshArgs...)
			proc.Stdin = strings.NewReader(string(localContent))
			if err := proc.Run(); err == nil {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}(p)
	}

	wg.Wait()
	elapsed := time.Since(t0).Seconds()
	spinner.Stop(fmt.Sprintf("\033[32m✓\033[0m Pushed settings.json to %d/%d peers \033[90m(%.2fs)\033[0m", successCount, len(peersToPush), elapsed))
}
