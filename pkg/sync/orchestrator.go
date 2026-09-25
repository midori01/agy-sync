package sync

import (
	"encoding/json"
	"fmt"
	"math"
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

var ipv4Regex = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)

type PreResolvedRoute struct {
	Host      string
	Port      int
	RouteTier string
	Latency   float64
}

func SyncWithPeer(host string, port int, peerName string, verbose bool, isBatch bool, preResolved *PreResolvedRoute, extSpinner *terminal.Spinner, force ...bool) *terminal.SyncResult {
	isForce := len(force) > 0 && force[0]
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
		} else if verbose {
			fmt.Printf("    [⏱️ +%.2fs] %s...\n", time.Since(t0).Seconds(), msg)
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

	// Launch async background ping for single-node sync if latency wasn't pre-resolved (zero-cost)
	var bgPingChan chan float64
	if routeLat == 0.0 {
		bgPingChan = make(chan float64, 1)
		probeTarget := host
		_, ph, pp, _ := config.ParseEndpoint(host, port)
		if ph != "" {
			probeTarget = ph
		}
		probePort := port
		if pp != 0 {
			probePort = pp
		}
		go func() {
			if ok, lat, _ := network.ProbeSSHBanner(probeTarget, probePort, 2000*time.Millisecond); ok {
				bgPingChan <- lat
			} else {
				bgPingChan <- 0.0
			}
		}()
	}

	if verbose {
		if host != origHost || port != origPort {
			fmt.Printf("[⚡] Route optimized: %s -> %s:%d (%.1fms)\n", routeTier, host, port, routeLat)
		} else {
			fmt.Printf("[🌐] Active route: %s -> %s:%d\n", routeTier, host, port)
		}
	}

	// 1. Pipeline Handshake
	stepStatus("Pipeline handshake")
	handshakeCmd := fmt.Sprintf("%s echo READY && echo \"HOME:$HOME\" && echo \"USER:$(whoami 2>/dev/null || echo \"$USER\")\" && echo \"OS:$(uname -s 2>/dev/null || echo Linux)\" && echo \"ARCH:$(uname -m 2>/dev/null || echo unknown)\" && echo \"COMMIT:$(command -v agy-sync >/dev/null 2>&1 && agy-sync --commit 2>/dev/null || echo NONE)\" && echo \"TS_IP:$(command -v tailscale >/dev/null 2>&1 && tailscale ip -4 2>/dev/null || true)\" && echo \"LAN_IPS:$((ifconfig 2>/dev/null | awk '/^[a-zA-Z0-9]/ {iface=$1} /inet (addr:)?/ {if (iface ~ /^(wlan|eth|en|ra|br|swlan|usb|rndis|wg)/) print $2}' | sed 's/addr://'; ip -o -4 addr show 2>/dev/null | awk '$2 ~ /^(wlan|eth|en|ra|br|swlan|usb|rndis|wg)/ {print $4}' | cut -d/ -f1) | tr '\\n' ',')\" && (date +%%s.%%N 2>/dev/null || date +%%s)", config.PathPrefix)

	isLanDirect := (routeTier == "LAN Direct")
	connectTo := 8
	if isLanDirect {
		connectTo = 4
	}

	stdout, stderr, _, err := ssh.RunSSHCmd(host, port, handshakeCmd, false, connectTo)
	t1 := time.Now()
	rtt := t1.Sub(t0).Seconds()

	if err != nil || !strings.Contains(stdout, "READY") {
		if isLanDirect {
			if verbose {
				fmt.Printf("[🔄] LAN direct test unconfirmed, falling back to Tailnet Mesh %s:%d...\n", origHost, origPort)
			}
			ssh.CloseControlMaster(host, port)
			ssh.CloseControlMaster(origHost, origPort)
			fallbackHost := origHost
			if strings.Contains(host, "@") && !strings.Contains(fallbackHost, "@") {
				parts := strings.SplitN(host, "@", 2)
				fallbackHost = fmt.Sprintf("%s@%s", parts[0], fallbackHost)
			}
			host, port = fallbackHost, origPort
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
				if bgPingChan != nil {
					select {
					case lat := <-bgPingChan:
						if lat > 0 {
							routeLat = lat
						}
					default:
					}
				}
				res := &terminal.SyncResult{
					Success:   false,
					PeerName:  peerName,
					RouteTier: routeTier,
					RouteLat:  routeLat,
					Elapsed:   math.Round(time.Since(t0).Seconds()*100) / 100.0,
					Error:     errMsg,
				}
				if !isBatch {
					terminal.RenderSingleNodeCard(res)
				}
				return res
			}
		} else {
			// If banner exchange or connection timed out on a remote/mobile node, retry once with fresh socket to allow wake-up
			if strings.Contains(stderr, "timed out") || strings.Contains(stderr, "Connection timed out") || strings.Contains(stderr, "Connection refused") {
				stepStatus("Retrying handshake (waking node)")
				ssh.CloseControlMaster(host, port)
				time.Sleep(1 * time.Second)
				stdoutRetry, stderrRetry, _, errRetry := ssh.RunSSHCmd(host, port, handshakeCmd, false, 12)
				if errRetry == nil && strings.Contains(stdoutRetry, "READY") {
					stdout = stdoutRetry
					stderr = stderrRetry
					err = nil
					t1 = time.Now()
					rtt = t1.Sub(t0).Seconds()
				}
			}

			if err != nil || !strings.Contains(stdout, "READY") {
				if createdSpinner && activeSpinner != nil {
					activeSpinner.Stop()
				}
				errMsg := strings.TrimSpace(stderr)
				if errMsg == "" {
					errMsg = "Handshake check failed"
				}
				if bgPingChan != nil {
					select {
					case lat := <-bgPingChan:
						if lat > 0 {
							routeLat = lat
						}
					default:
					}
				}
				res := &terminal.SyncResult{
					Success:   false,
					PeerName:  peerName,
					RouteTier: routeTier,
					RouteLat:  routeLat,
					Elapsed:   math.Round(time.Since(t0).Seconds()*100) / 100.0,
					Error:     errMsg,
				}
				if !isBatch {
					terminal.RenderSingleNodeCard(res)
				}
				return res
			}
		}
	}

	remHome := config.HomeDir
	remUser := ""
	remOS := "linux"
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
		} else if strings.HasPrefix(line, "OS:") {
			o := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "OS:")))
			if strings.Contains(o, "darwin") {
				remOS = "darwin"
			} else {
				remOS = "linux"
			}
		} else if strings.HasPrefix(line, "ARCH:") {
			remArch = strings.TrimSpace(strings.TrimPrefix(line, "ARCH:"))
		} else if strings.HasPrefix(line, "COMMIT:") {
			remCommit = strings.TrimSpace(strings.TrimPrefix(line, "COMMIT:"))
		} else if strings.HasPrefix(line, "TS_IP:") {
			remTsIP = strings.TrimSpace(strings.TrimPrefix(line, "TS_IP:"))
		} else if strings.HasPrefix(line, "LAN_IPS:") {
			raw := strings.TrimSpace(strings.TrimPrefix(line, "LAN_IPS:"))
			seen := make(map[string]bool)
			for _, token := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
				token = strings.TrimSpace(token)
				if token != "" && token != "127.0.0.1" && token != "localhost" && token != "0.0.0.0" && token != "::1" && !strings.HasPrefix(token, "100.") {
					if ipv4Regex.MatchString(token) && !seen[token] {
						seen[token] = true
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
			// Automatically capture and persist remote Tailscale IP (pure IP address without user@ prefix)
			cleanOrigHost := origHost
			if idx := strings.Index(cleanOrigHost, "@"); idx != -1 {
				cleanOrigHost = cleanOrigHost[idx+1:]
			}
			cleanRemTsIP := remTsIP
			if idx := strings.Index(cleanRemTsIP, "@"); idx != -1 {
				cleanRemTsIP = cleanRemTsIP[idx+1:]
			}

			if cleanRemTsIP != "" && strings.HasPrefix(cleanRemTsIP, "100.") && d.TailscaleIP != cleanRemTsIP {
				d.TailscaleIP = cleanRemTsIP
				regUpdated = true
			} else if strings.HasPrefix(cleanOrigHost, "100.") && d.TailscaleIP != cleanOrigHost {
				d.TailscaleIP = cleanOrigHost
				regUpdated = true
			}
			if regUpdated {
				d.UpdatedAt = float64(time.Now().UnixNano()) / 1e9
			}
		}
	}
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
		config.ToRemotePath(config.SummariesDBFile, remHome),
		config.ToRemotePath(config.DevicesJSONFile, remHome),
		config.ToRemotePath(config.AuthKeysFile, remHome),
		config.ToRemotePath(config.OAuthTokenFile, remHome),
		remPoolCfg,
		config.ToRemotePath(config.SettingsFile, remHome),
		config.ToRemotePath(config.ProtoSummariesFile, remHome),
		config.ToRemotePath(config.HistoryFile, remHome),
		config.ToRemotePath(filepath.Join(config.PoolDir, "bin", "agy-pool"), remHome),
	}
	probeCache := BatchProbeRemoteFiles(host, port, batchTargets)

	tracker := NewByteTracker()

	var changes []terminal.ChangeItem
	var unchanged []string
	var warnings []terminal.ChangeItem
	var failures []terminal.ChangeItem

	recordAsset := func(name, desc, icon string, isChanged bool) {
		lowerDesc := strings.ToLower(desc)
		isFail := strings.Contains(lowerDesc, "failed") || strings.Contains(lowerDesc, "error")
		isWarn := strings.Contains(lowerDesc, "dirty") || strings.Contains(lowerDesc, "conflict") ||
			strings.Contains(lowerDesc, "safety guard") || strings.Contains(lowerDesc, "diverged") ||
			strings.Contains(lowerDesc, "skipped")

		if isFail {
			failures = append(failures, terminal.ChangeItem{Name: name, Desc: desc, Icon: "❌"})
			changes = append(changes, terminal.ChangeItem{Name: name, Desc: desc, Icon: "❌"})
		} else if isWarn {
			warnings = append(warnings, terminal.ChangeItem{Name: name, Desc: desc, Icon: "⚠️"})
			changes = append(changes, terminal.ChangeItem{Name: name, Desc: desc, Icon: "⚠️"})
		} else if isChanged {
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
	regStat := SyncClusterRegistry(host, port, clockSkew, remHome, probeCache, tracker)
	recordAsset("Cluster Registry", regStat, "🪪", regStat == "pushed" || regStat == "pulled" || strings.Contains(regStat, "merged"))

	// 1.6 Sync SSH Authorized Keys
	authStat := SyncAuthorizedKeys(host, port, remHome, probeCache, tracker)
	recordAsset("SSH Authorized Keys", authStat, "🔑", authStat == "pushed" || authStat == "pulled" || strings.Contains(authStat, "merged"))

	// 2. Sync Rules
	stepStatus("Constitutional rules & memory")
	rulesStat := SyncSingleFile(config.RulesFile, config.ToRemotePath(config.RulesFile, remHome), host, port, "GEMINI.md", clockSkew, probeCache, tracker)
	recordAsset("Constitutional Rules", rulesStat, "📜", rulesStat == "pushed" || rulesStat == "pulled")

	// 3. Sync Memory Relay
	handoffStat := SyncSingleFile(config.HandoffFile, config.ToRemotePath(config.HandoffFile, remHome), host, port, "session_handoff.md", clockSkew, probeCache, tracker)
	recordAsset("Memory Relay", handoffStat, "🧠", handoffStat == "pushed" || handoffStat == "pulled")

	// 3.4 Sync Go Synchronizer Source Code (determines git version parity & direction)
	stepStatus("Go synchronizer source")
	goSrcStat := SyncAgySyncGo(host, port, remHome, clockSkew, isForce, tracker)
	pulledNewVersion := false
	if goSrcStat != "none" {
		recordAsset("Go Synchronizer Source", goSrcStat, "🐹", goSrcStat == "pushed" || goSrcStat == "pulled" || goSrcStat == "installed & ready")
		if goSrcStat == "pulled" || goSrcStat == "pulled & ready" {
			pulledNewVersion = true
		}
	}

	// 3.5 Sync Engine (uses goSrcStat to safely determine binary build/push/rebuild)
	stepStatus("Synchronizer engine")
	binStat := SyncEngine(host, port, remHome, remArch, remCommit, goSrcStat, clockSkew, probeCache, isForce, remOS, tracker)
	recordAsset("Synchronizer Engine", binStat, "⚡", binStat == "pushed" || binStat == "pulled" || strings.Contains(binStat, "rebuilt") || strings.Contains(binStat, "compiled"))
	if strings.Contains(binStat, "rebuilt") || strings.Contains(binStat, "pulled") {
		pulledNewVersion = true
	}

	// 3.6 Sync Token
	stepStatus("Account pool & tokens")
	remToken := config.ToRemotePath(config.OAuthTokenFile, remHome)
	hasToken := false
	if _, err := os.Stat(config.OAuthTokenFile); err == nil || probeCache[remToken] != nil {
		hasToken = true
	}
	if hasToken {
		tokStat := SyncSingleFile(config.OAuthTokenFile, remToken, host, port, "antigravity-oauth-token", clockSkew, probeCache, tracker, "600")
		recordAsset("Antigravity Token", tokStat, "🎫", tokStat == "pushed" || tokStat == "pulled")
	}

	// 3.7 Sync Account Pool Config
	hasPoolCfg := false
	if _, err := os.Stat(config.PoolAccountsFile); err == nil || probeCache[remPoolCfg] != nil {
		hasPoolCfg = true
	}
	if hasPoolCfg {
		poolStat := SyncPoolAccounts(host, port, remHome, clockSkew, probeCache, tracker)
		if poolStat != "none" {
			recordAsset("Account Pool", poolStat, "👥", strings.Contains(poolStat, "pushed") || strings.Contains(poolStat, "pulled") || strings.Contains(poolStat, "merged"))
		}
	}

	// 3.8 Sync Account Pool Engine
	engineStat := SyncPoolEngine(host, port, remHome, clockSkew, probeCache, tracker)
	if engineStat != "none" {
		recordAsset("Account Pool Engine", engineStat, "🏊", engineStat == "pushed" || engineStat == "pulled" || engineStat == "installed & ready")
	}

	// 4. Sync CLI Settings & Proto Summaries
	stepStatus("CLI settings & summaries")
	remSettings := config.ToRemotePath(config.SettingsFile, remHome)
	hasSettings := false
	if _, err := os.Stat(config.SettingsFile); err == nil || probeCache[remSettings] != nil {
		hasSettings = true
	}
	if hasSettings {
		setStat := SyncSingleFile(config.SettingsFile, remSettings, host, port, "settings.json", clockSkew, probeCache, tracker, "600")
		recordAsset("CLI Settings", setStat, "🔧", setStat == "pushed" || setStat == "pulled")
	}

	if _, err := os.Stat(config.ProtoSummariesFile); err == nil {
		protoStat := SyncSingleFile(config.ProtoSummariesFile, config.ToRemotePath(config.ProtoSummariesFile, remHome), host, port, "jetbox_summaries_proto.pb", clockSkew, probeCache, tracker)
		recordAsset("Jetbox Summaries", protoStat, "📊", protoStat == "pushed" || protoStat == "pulled")
	}

	// 5. Sync Conversation Summaries
	stepStatus("Conversation index")
	sumsStat := SyncConversationSummaries(host, port, clockSkew, remHome, probeCache, tracker)
	recordAsset("Conversation Index", sumsStat, "📑", strings.Contains(sumsStat, "pushed") || strings.Contains(sumsStat, "pulled") || strings.Contains(sumsStat, "merged"))

	// 5.8 Batch-probe Remote Directory Inventories in 1 single RTT (eliminates 3 network round-trips)
	stepStatus("Scanning directory inventories")
	dirInv := BatchProbeDirectoryInventories(host, port, remHome)
	var preprobedDBs map[string][2]int64
	var preprobedAnns map[string]int64
	var preprobedBrain map[string][2]int64
	var preprobedCfg map[string][2]int64
	if dirInv != nil {
		preprobedDBs = dirInv.ConvDBs
		preprobedAnns = dirInv.Anns
		preprobedBrain = dirInv.Brain
		preprobedCfg = dirInv.Configs
	}

	// 6. Sync Conversation Databases
	stepStatus("Conversation databases")
	convPushed, convPulled = SyncConversationDBs(host, port, clockSkew, remHome, tracker, preprobedDBs, stepStatus)
	convChanged := (convPushed > 0 || convPulled > 0)
	convDesc := "identical"
	if convChanged {
		convDesc = fmt.Sprintf("+%d pushed, +%d pulled", convPushed, convPulled)
	}
	recordAsset("Conversation Databases", convDesc, "💬", convChanged)

	// 7. Sync Annotations
	stepStatus("Conversation annotations")
	annPushed, annPulled := SyncAnnotations(host, port, clockSkew, remHome, tracker, preprobedAnns, stepStatus)
	annChanged := (annPushed > 0 || annPulled > 0)
	annDesc := "identical"
	if annChanged {
		annDesc = fmt.Sprintf("+%d pushed, +%d pulled", annPushed, annPulled)
	}
	recordAsset("Conversation Annotations", annDesc, "🔖", annChanged)

	// 8. Sync Brain Artifacts & Transcripts
	stepStatus("Brain transcripts & artifacts")
	brainPushed, brainPulled = SyncBrainSessions(host, port, clockSkew, remHome, tracker, preprobedBrain, stepStatus)
	brainChanged := (brainPushed > 0 || brainPulled > 0)
	brainDesc := "identical"
	if brainChanged {
		brainDesc = fmt.Sprintf("+%d pushed, +%d pulled", brainPushed, brainPulled)
	}
	recordAsset("Brain Transcripts & Artifacts", brainDesc, "📦", brainChanged)

	// 9. Sync History Lines
	stepStatus("Prompt history")
	histStat := SyncHistoryJsonl(host, port, remHome, probeCache, tracker)
	recordAsset("Prompt History", histStat, "📝", strings.Contains(histStat, "pushed") || strings.Contains(histStat, "pulled") || strings.Contains(histStat, "merged"))

	// 10. Sync Configs & Skills
	stepStatus("Skills & configurations")
	cfgPushed, cfgPulled := SyncConfigsAndSkills(host, port, clockSkew, remHome, tracker, preprobedCfg, stepStatus)
	cfgChanged := (cfgPushed > 0 || cfgPulled > 0)
	cfgDesc := "identical"
	if cfgChanged {
		cfgDesc = fmt.Sprintf("+%d pushed, +%d pulled", cfgPushed, cfgPulled)
	}
	recordAsset("Skills & Configs", cfgDesc, "🧰", cfgChanged)

	stepStatus("Finalizing")
	elapsed := math.Round(time.Since(t0).Seconds()*100) / 100.0

	if createdSpinner && activeSpinner != nil {
		activeSpinner.Stop()
	}

	if bgPingChan != nil {
		select {
		case lat := <-bgPingChan:
			if lat > 0 {
				routeLat = lat
			}
		default:
		}
	}

	hasFailures := len(failures) > 0
	hasWarnings := len(warnings) > 0
	success := !hasFailures

	result := &terminal.SyncResult{
		Success:          success,
		HasWarnings:      hasWarnings,
		HasFailures:      hasFailures,
		PulledNewVersion: pulledNewVersion,
		Warnings:         warnings,
		Failures:         failures,
		PeerName:         peerName,
		RouteTier:        routeTier,
		RouteLat:         routeLat,
		ClockSkew:        clockSkew,
		Elapsed:          elapsed,
		Changes:          changes,
		Unchanged:        unchanged,
		DBPushed:         convPushed,
		DBPulled:         convPulled,
		ArtPushed:        brainPushed,
		ArtPulled:        brainPulled,
		BytesPushed:      tracker.BytesPushed,
		BytesPulled:      tracker.BytesPulled,
	}

	if !isBatch {
		terminal.RenderSingleNodeCard(result)
	}
	return result
}

func SyncAllPeers(verbose bool, force ...bool) {
	isForce := len(force) > 0 && force[0]
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
	var bgPingMap sync.Map

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

			// Launch background async ping probe for non-LAN / Tailnet nodes
			// Runs in parallel while Phase 1 finishes and Phase 2 starts, adding 0.00s delay!
			if rLat == 0 {
				go func(peerName, host string, p int) {
					_, rawH, rawP, _ := config.ParseEndpoint(host, p)
					if ok, bannerLat, _ := network.ProbeSSHBanner(rawH, rawP, 2500*time.Millisecond); ok {
						bgPingMap.Store(peerName, bannerLat)
					}
				}(name, effH, effP)
			}
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
		latI := probedRoutes[i].RouteLat
		latJ := probedRoutes[j].RouteLat
		if latI > 0 && latJ > 0 && latI != latJ {
			return latI < latJ
		}
		if latI > 0 && latJ == 0 {
			return true
		}
		if latI == 0 && latJ > 0 {
			return false
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
		curLat := r.RouteLat
		if curLat == 0 {
			if val, ok := bgPingMap.Load(r.Name); ok {
				curLat = val.(float64)
			}
		}

		latStr := ""
		if curLat > 0 {
			latStr = fmt.Sprintf(" · %.1fms", curLat)
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
			Latency:   curLat,
		}

		res := SyncWithPeer(r.OrigHost, r.OrigPort, r.Name, verbose, true, route, nodeSpinner, isForce)
		results = append(results, res)

		var finalStatus string
		if res.Success && !res.HasFailures {
			if res.HasWarnings {
				finalStatus = fmt.Sprintf("\033[33m⚠\033[0m %s... diverged \033[90m(%.2fs)\033[0m", nodePrefix, res.Elapsed)
			} else {
				successCnt++
				finalStatus = fmt.Sprintf("\033[32m✓\033[0m %s... done \033[90m(%.2fs)\033[0m", nodePrefix, res.Elapsed)
			}
		} else {
			failCnt++
			finalStatus = fmt.Sprintf("\033[31m✗\033[0m %s... failed \033[90m(%.2fs)\033[0m", nodePrefix, res.Elapsed)
		}

		if nodeSpinner != nil {
			nodeSpinner.Stop(finalStatus)
		} else if !verbose {
			fmt.Println(finalStatus)
		}

		// Self-healing multi-peer back-propagation:
		// If local pulled a newer version from this peer, all earlier peers (0..idx-1)
		// were synced while local was on the old version.
		// Immediately re-sync those earlier peers so they receive the new version!
		if res.PulledNewVersion && idx > 0 {
			if verbose {
				fmt.Printf("\n[🚀] In-flight upgrade from '%s' detected! Back-propagating new release to %d earlier peer(s)...\n", r.Name, idx)
			}
			for prevIdx := 0; prevIdx < idx; prevIdx++ {
				prevR := probedRoutes[prevIdx]
				prevLat := prevR.RouteLat
				if prevLat == 0 {
					if val, ok := bgPingMap.Load(prevR.Name); ok {
						prevLat = val.(float64)
					}
				}
				bpPrefix := fmt.Sprintf("[↩️ Back-prop] Upgrading %s to latest release", prevR.Name)
				var bpSpinner *terminal.Spinner
				if !verbose {
					bpSpinner = terminal.NewSpinner(bpPrefix, true)
					bpSpinner.Start()
				} else {
					fmt.Printf("[↩️ Back-prop] Upgrading %s to latest release...\n", prevR.Name)
				}
				bpRoute := &PreResolvedRoute{
					Host:      prevR.EffHost,
					Port:      prevR.EffPort,
					RouteTier: prevR.RouteTier,
					Latency:   prevLat,
				}
				bpRes := SyncWithPeer(prevR.OrigHost, prevR.OrigPort, prevR.Name, verbose, true, bpRoute, bpSpinner, isForce)
				results[prevIdx] = bpRes
				if bpSpinner != nil {
					bpFinalStatus := fmt.Sprintf("\033[32m✓\033[0m %s... converged \033[90m(%.2fs)\033[0m", bpPrefix, bpRes.Elapsed)
					if bpRes.HasWarnings {
						bpFinalStatus = fmt.Sprintf("\033[33m⚠\033[0m %s... diverged \033[90m(%.2fs)\033[0m", bpPrefix, bpRes.Elapsed)
					} else if !bpRes.Success || bpRes.HasFailures {
						bpFinalStatus = fmt.Sprintf("\033[31m✗\033[0m %s... failed \033[90m(%.2fs)\033[0m", bpPrefix, bpRes.Elapsed)
					}
					bpSpinner.Stop(bpFinalStatus)
				} else if !verbose {
					bpFinalStatus := fmt.Sprintf("\033[32m✓\033[0m %s... converged \033[90m(%.2fs)\033[0m", bpPrefix, bpRes.Elapsed)
					if bpRes.HasWarnings {
						bpFinalStatus = fmt.Sprintf("\033[33m⚠\033[0m %s... diverged \033[90m(%.2fs)\033[0m", bpPrefix, bpRes.Elapsed)
					} else if !bpRes.Success || bpRes.HasFailures {
						bpFinalStatus = fmt.Sprintf("\033[31m✗\033[0m %s... failed \033[90m(%.2fs)\033[0m", bpPrefix, bpRes.Elapsed)
					}
					fmt.Println(bpFinalStatus)
				}
			}
		}
	}

	totalElapsed := time.Since(totalT0).Seconds()
	fmt.Println()

	// Backfill any remaining background pings into results
	for _, res := range results {
		if res.RouteLat == 0 {
			if val, ok := bgPingMap.Load(res.PeerName); ok {
				res.RouteLat = val.(float64)
			}
		}
	}

	// Sort results for final dashboard: LAN first, then lowest latency, then alphabetical
	sort.Slice(results, func(i, j int) bool {
		lanI := strings.Contains(results[i].RouteTier, "LAN")
		lanJ := strings.Contains(results[j].RouteTier, "LAN")
		if lanI && !lanJ {
			return true
		}
		if !lanI && lanJ {
			return false
		}
		latI := results[i].RouteLat
		latJ := results[j].RouteLat
		if latI > 0 && latJ > 0 && latI != latJ {
			return latI < latJ
		}
		if latI > 0 && latJ == 0 {
			return true
		}
		if latI == 0 && latJ > 0 {
			return false
		}
		return results[i].PeerName < results[j].PeerName
	})

	// Recompute counts accurately from results
	successCnt = 0
	failCnt = 0
	for _, res := range results {
		if res.Success && !res.HasFailures && !res.HasWarnings {
			successCnt++
		} else if !res.Success || res.HasFailures {
			failCnt++
		}
	}

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

	fmt.Printf("[🔧] Pushing local settings.json to %d peer(s)...\n", len(peersToPush))
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
			tmpRemote := fmt.Sprintf("%s.tmp_push_%d_%s", remSettings, time.Now().UnixNano(), randomToken(3))

			pushCmd := fmt.Sprintf("mkdir -p '%s' && (cat > '%s' && chmod 600 '%s' && (touch -d @%d '%s' 2>/dev/null || true) && mv -f '%s' '%s' && (touch -d @%d '%s' 2>/dev/null || true) || (rm -f '%s' 2>/dev/null; false))",
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

func PushPoolToPeers(targetAlias string) {
	if targetAlias == "-h" || targetAlias == "--help" {
		fmt.Println(`
Usage: agy-sync push-pool [alias]

Aliases:
  agy-sync push-accounts [alias]
  agy-sync pool-push [alias]

Arguments:
  alias                 Optional specific peer alias (e.g. sjc1, galaxy).
                        If omitted, force-pushes local agy-pool accounts.json to all remote peers in cluster.`)
		return
	}

	if _, err := os.Stat(config.PoolAccountsFile); err != nil {
		fmt.Println("[❌] No local agy-pool-accounts.json found to push.")
		return
	}

	now := time.Now()
	_ = os.Chtimes(config.PoolAccountsFile, now, now)
	_ = os.Chmod(config.PoolAccountsFile, 0600)

	accountCount := 0
	if data, err := os.ReadFile(config.PoolAccountsFile); err == nil {
		var parsed map[string]interface{}
		if err := json.Unmarshal(data, &parsed); err == nil {
			if accs, ok := parsed["accounts"].([]interface{}); ok {
				accountCount = len(accs)
			}
		}
	}

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
		fmt.Println("[⚠️] No remote peers to push account pool to.")
		return
	}

	fmt.Printf("[🏊] Force-pushing local account pool (%d accounts) to %d peer(s)...\n", accountCount, len(peersToPush))
	spinner := terminal.NewSpinner(fmt.Sprintf("Pushing agy-pool accounts (%d accounts) across %d peer(s)", accountCount, len(peersToPush)), true)
	spinner.Start()

	t0 := time.Now()
	localContent, _ := os.ReadFile(config.PoolAccountsFile)
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
			remPool := config.ToRemotePath(config.PoolAccountsFile, remHome)
			remTargetMTime := localMTime + int64(skew)
			remParent := filepath.Dir(remPool)
			tmpRemote := fmt.Sprintf("%s.tmp_push_pool_%d_%s", remPool, time.Now().UnixNano(), randomToken(3))

			pushCmd := fmt.Sprintf("mkdir -p '%s' && (cat > '%s' && chmod 600 '%s' && (touch -d @%d '%s' 2>/dev/null || true) && mv -f '%s' '%s' && (touch -d @%d '%s' 2>/dev/null || true) || (rm -f '%s' 2>/dev/null; false))",
				remParent, tmpRemote, tmpRemote, remTargetMTime, tmpRemote, tmpRemote, remPool, remTargetMTime, remPool, tmpRemote)

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
	spinner.Stop(fmt.Sprintf("\033[32m✓\033[0m Force-pushed account pool (%d accounts) to %d/%d peers \033[90m(%.2fs)\033[0m", accountCount, successCount, len(peersToPush), elapsed))
}

func PushEngineToPeers(targetAlias string) {
	if targetAlias == "-h" || targetAlias == "--help" {
		fmt.Println(`
Usage: agy-sync push-engine [alias]

Aliases:
  agy-sync push-pool-engine [alias]
  agy-sync engine-push [alias]

Arguments:
  alias                 Optional specific peer alias (e.g. sjc1, galaxy, mac).
                        If omitted, force-pushes local ~/agy-pool engine and installs to all remote peers in cluster.`)
		return
	}
	ForcePushPoolEngine(targetAlias)
}

func ForcePushPoolEngine(targetAlias string) {
	if fi, err := os.Stat(config.PoolDir); err != nil || !fi.IsDir() {
		fmt.Println("[❌] No local ~/agy-pool found to push.")
		return
	}

	localBin := filepath.Join(config.PoolDir, "bin", "agy-pool")
	if _, err := os.Stat(localBin); err != nil {
		fmt.Println("[❌] No local ~/agy-pool/bin/agy-pool executable found to push.")
		return
	}

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
		fmt.Println("[⚠️] No remote peers to push agy-pool engine to.")
		return
	}

	fmt.Printf("[🏊] Force-pushing agy-pool engine to %d peer(s)...\n", len(peersToPush))
	spinner := terminal.NewSpinner(fmt.Sprintf("Pushing agy-pool engine across %d peer(s)", len(peersToPush)), true)
	spinner.Start()

	t0 := time.Now()
	successCount := 0
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, p := range peersToPush {
		wg.Add(1)
		go func(peer pushPeer) {
			defer wg.Done()
			effH, effP, _, _ := network.ResolveFastestEndpoint(peer.Host, peer.Port, peer.Alias)
			stdout, _, _, err := ssh.RunSSHCmd(effH, effP, "echo \"HOME:$HOME\"", false)
			if err != nil {
				return
			}
			remHome := config.HomeDir
			for _, line := range strings.Split(stdout, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "HOME:") {
					remHome = strings.TrimSpace(strings.TrimPrefix(line, "HOME:"))
				}
			}

			p1 := exec.Command("tar", "-C", config.HomeDir, "--exclude=.git", "--exclude=__pycache__", "--exclude=*.pyc", "--exclude=.DS_Store", "-czf", "-", "agy-pool")
			sshArgs := ssh.GetSSHCmd(effH, effP, fmt.Sprintf("tar -C '%s' --no-same-owner -xzf - 2>/dev/null", remHome), true)
			p2 := exec.Command("ssh", sshArgs...)
			_, err = RunPipedCommandsWithBytes(p1, p2)
			if err == nil {
				remPoolDir := fmt.Sprintf("%s/agy-pool", remHome)
				postCmd := fmt.Sprintf(`export PATH="/data/data/com.termux/files/usr/bin:/usr/local/bin:$HOME/.local/bin:$HOME/bin:$PATH";
chmod +x '%s/bin/agy-pool' '%s/bin/agy-raw' '%s/install.sh' 2>/dev/null || true;
if [ -f '%s/install.sh' ]; then
    bash '%s/install.sh' >/dev/null 2>&1 || true
fi;
if [ -x '%s/bin/agy-pool' ]; then
    '%s/bin/agy-pool' restart >/dev/null 2>&1 || true
elif command -v agy-pool >/dev/null 2>&1; then
    agy-pool restart >/dev/null 2>&1 || true
fi`, remPoolDir, remPoolDir, remPoolDir, remPoolDir, remPoolDir, remPoolDir, remPoolDir)
				_, _, _, _ = ssh.RunSSHCmd(effH, effP, postCmd, false)

				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}(p)
	}

	wg.Wait()
	elapsed := time.Since(t0).Seconds()
	spinner.Stop(fmt.Sprintf("\033[32m✓\033[0m Force-pushed agy-pool engine to %d/%d peers \033[90m(%.2fs)\033[0m", successCount, len(peersToPush), elapsed))
}
