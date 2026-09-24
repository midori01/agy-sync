package cluster

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"agy-sync/pkg/config"
	"agy-sync/pkg/network"
	"agy-sync/pkg/ssh"
	"agy-sync/pkg/terminal"
)

var safeHostCharRegex = regexp.MustCompile(`[^a-zA-Z0-9_\-\.]`)

func ValidateAlias(alias string) (bool, string) {
	a := strings.ToLower(strings.TrimSpace(alias))
	if a == "" {
		return false, "Alias cannot be empty."
	}
	if len(a) > 32 {
		return false, "Alias must be at most 32 characters."
	}
	if !config.SafeAliasRegex.MatchString(a) {
		return false, "Alias may only contain letters, digits, '.', '-', and '_'."
	}
	if config.ReservedAliases[a] {
		return false, fmt.Sprintf("'%s' is a reserved keyword and cannot be used as an alias.", a)
	}
	if _, err := strconv.Atoi(a); err == nil {
		return false, "Alias cannot be purely numeric."
	}
	return true, a
}

func GetUniqueAlias(baseAlias, endpoint string, reg config.Registry) string {
	prefix := strings.ToLower(strings.TrimSpace(baseAlias))
	if prefix == "" {
		prefix = "node"
	}
	if config.ReservedAliases[prefix] {
		prefix = fmt.Sprintf("node-%s", prefix)
	}

	exists := false
	for ep, d := range reg {
		if ep != endpoint && !d.Deleted && strings.EqualFold(d.Alias, prefix) {
			exists = true
			break
		}
	}
	if !exists {
		return prefix
	}

	idx := 2
	for {
		cand := fmt.Sprintf("%s-%d", prefix, idx)
		found := false
		for ep, d := range reg {
			if ep != endpoint && !d.Deleted && strings.EqualFold(d.Alias, cand) {
				found = true
				break
			}
		}
		if !found {
			return cand
		}
		idx++
	}
}

func ReconcileRegistry() {
	if _, err := os.Stat(config.DevicesJSONFile); err != nil {
		return
	}
	registry := config.LoadRegistry()
	cfg := config.LoadConfig()
	if _, ok := cfg["devices"]; !ok {
		cfg["devices"] = make(map[string]string)
	}

	localEp := config.GetLocalEndpoint()
	localAlias := ""

	registryDirty := false
	assignedAliases := make(map[string]bool)
	newDevices := make(map[string]string)

	for ep, data := range registry {
		_, host, _, _ := config.ParseEndpoint(ep, 8022)
		if host == "127.0.0.1" || host == "localhost" || host == "0.0.0.0" || host == "::1" {
			if !data.Deleted {
				data.Deleted = true
				data.UpdatedAt = float64(time.Now().UnixNano()) / 1e9
				registryDirty = true
			}
			continue
		}

		if data.Deleted {
			continue
		}

		alias := strings.ToLower(strings.TrimSpace(data.Alias))
		if alias == "" {
			alias = strings.ToLower(strings.TrimSpace(data.NativeName))
			if alias == "" {
				alias = ep
			}
		}

		if assignedAliases[alias] {
			unique := GetUniqueAlias(alias, ep, registry)
			data.Alias = unique
			data.UpdatedAt = float64(time.Now().UnixNano()) / 1e9
			alias = unique
			registryDirty = true
		}

		assignedAliases[alias] = true
		newDevices[alias] = ep

		if ep == localEp {
			localAlias = alias
		} else if config.IsLocalEndpoint(ep, data) && localAlias == "" {
			localAlias = alias
		}
	}

	cfg["devices"] = newDevices

	if _, ok := cfg["local"]; !ok {
		cfg["local"] = make(map[string]string)
	}
	cfg["local"]["endpoint"] = localEp
	if localAlias != "" {
		cfg["local"]["name"] = localAlias
	}

	if syncSec, ok := cfg["sync"]; ok {
		curDef := strings.ToLower(strings.TrimSpace(syncSec["default"]))
		defEp := cfg["devices"][curDef]
		if _, exists := cfg["devices"][curDef]; !exists || (defEp != "" && config.IsLocalEndpoint(defEp, nil)) {
			cfg["sync"]["default"] = ""
			cfg["sync"]["target"] = ""
			cfg["sync"]["port"] = "8022"

			for alias, ep := range cfg["devices"] {
				_, _, p, tSSH := config.ParseEndpoint(ep, 8022)
				if !config.IsLocalEndpoint(ep, nil) {
					cfg["sync"]["default"] = alias
					cfg["sync"]["target"] = tSSH
					cfg["sync"]["port"] = strconv.Itoa(p)
					break
				}
			}
		}
	}

	localLanNets := network.GetLocalLanNetworks()
	var localLanIPs []string
	for _, n := range localLanNets {
		localLanIPs = append(localLanIPs, n.IP)
	}

	if d, ok := registry[localEp]; ok && len(localLanIPs) > 0 {
		sort.Strings(localLanIPs)
		curLans := make([]string, len(d.LanIps))
		copy(curLans, d.LanIps)
		sort.Strings(curLans)
		if strings.Join(localLanIPs, ",") != strings.Join(curLans, ",") {
			d.LanIps = localLanIPs
			d.UpdatedAt = float64(time.Now().UnixNano()) / 1e9
			registryDirty = true
		}
	}

	_ = config.SaveConfig(cfg)
	if registryDirty {
		_ = config.SaveRegistry(registry)
	}

	ssh.ReconcileSSHConfig(registry)
	ssh.ReconcileShellIntegration(registry)
}

func MergeRegistries(regA, regB config.Registry, skewB float64) (config.Registry, bool, bool) {
	allEps := make(map[string]bool)
	for ep := range regA {
		allEps[ep] = true
	}
	for ep := range regB {
		allEps[ep] = true
	}

	merged := make(config.Registry)
	aChanged := false
	bChanged := false
	now := float64(time.Now().UnixNano()) / 1e9

	for ep := range allEps {
		recA := regA[ep]
		recB := regB[ep]
		_, host, _, _ := config.ParseEndpoint(ep, 8022)

		if host == "127.0.0.1" || host == "localhost" || host == "0.0.0.0" || host == "::1" {
			var baseRec *config.DeviceEntry
			if recA != nil {
				cp := *recA
				baseRec = &cp
			} else if recB != nil {
				cp := *recB
				baseRec = &cp
			}
			if baseRec != nil && !baseRec.Deleted {
				baseRec.Deleted = true
				baseRec.UpdatedAt = now
				if recA != nil && !recA.Deleted {
					aChanged = true
				}
				if recB != nil && !recB.Deleted {
					bChanged = true
				}
			}
			if baseRec != nil {
				merged[ep] = baseRec
			}
			continue
		}

		if recA != nil && recB == nil {
			merged[ep] = recA
			bChanged = true
		} else if recB != nil && recA == nil {
			merged[ep] = recB
			aChanged = true
		} else {
			tA := recA.UpdatedAt
			tB := recB.UpdatedAt - skewB

			unionLanIPs := func(primary, secondary []string) []string {
				seen := make(map[string]bool)
				var res []string
				for _, ip := range primary {
					ip = strings.TrimSpace(ip)
					if ip != "" && !seen[ip] {
						seen[ip] = true
						res = append(res, ip)
					}
				}
				for _, ip := range secondary {
					ip = strings.TrimSpace(ip)
					if ip != "" && !seen[ip] {
						seen[ip] = true
						res = append(res, ip)
					}
				}
				sort.Strings(res)
				return res
			}

			if tA >= tB {
				cp := *recA
				cp.LanIps = unionLanIPs(recA.LanIps, recB.LanIps)
				merged[ep] = &cp
				if tA > tB || len(cp.LanIps) != len(recB.LanIps) {
					bChanged = true
				}
			} else {
				cp := *recB
				cp.LanIps = unionLanIPs(recB.LanIps, recA.LanIps)
				merged[ep] = &cp
				aChanged = true
			}
		}
	}
	return merged, aChanged, bChanged
}

type DeviceTarget struct {
	TargetSSH string
	Port      int
}

func LoadDevices() map[string]DeviceTarget {
	registry := config.LoadRegistry()
	devices := make(map[string]DeviceTarget)
	for ep, data := range registry {
		if data.Deleted {
			continue
		}
		alias := strings.ToLower(strings.TrimSpace(data.Alias))
		if alias == "" {
			alias = strings.ToLower(strings.TrimSpace(data.NativeName))
			if alias == "" {
				alias = ep
			}
		}
		_, _, p, targetSSH := config.ParseEndpoint(ep, 8022)
		devices[alias] = DeviceTarget{TargetSSH: targetSSH, Port: p}
	}
	return devices
}

func SaveDevice(name, host string, port int) {
	ep := fmt.Sprintf("%s:%d", host, port)
	if strings.Contains(host, ":") {
		ep = host
	}
	registry := config.LoadRegistry()
	uniqueName := GetUniqueAlias(strings.ToLower(strings.TrimSpace(name)), ep, registry)
	registry[ep] = &config.DeviceEntry{
		Alias:      uniqueName,
		NativeName: strings.ToLower(strings.TrimSpace(name)),
		IdType:     "Host",
		Deleted:    false,
		UpdatedAt:  float64(time.Now().UnixNano()) / 1e9,
	}
	_ = config.SaveRegistry(registry)
	ReconcileRegistry()
	BroadcastRegistryToOnlinePeers(false)
	ManageKeepalive("restart")
}

func RemoveDevice(name string) (bool, string) {
	ep, curr := ssh.FindEndpoint(name)
	if ep == "" {
		return false, fmt.Sprintf("Device '%s' not found in cluster registry.", name)
	}
	if config.IsLocalEndpoint(ep, nil) {
		return false, fmt.Sprintf("Cannot remove local device '%s' from the cluster.", name)
	}
	registry := config.LoadRegistry()
	if d, ok := registry[ep]; ok {
		d.Deleted = true
		d.UpdatedAt = float64(time.Now().UnixNano()) / 1e9
		_ = config.SaveRegistry(registry)
		ReconcileRegistry()
		BroadcastRegistryToOnlinePeers(false)
		ManageKeepalive("restart")
		return true, fmt.Sprintf("Device '%s' (%s) removed from cluster registry.", curr, ep)
	}
	return false, fmt.Sprintf("Device '%s' not found.", name)
}

func SetDefaultDevice(name string) (bool, string) {
	ep, alias := ssh.FindEndpoint(name)
	if ep == "" {
		return false, fmt.Sprintf("Device '%s' not found in cluster registry.", name)
	}
	if config.IsLocalEndpoint(ep, nil) {
		return false, fmt.Sprintf("Cannot set local device '%s' as the default sync target.", name)
	}
	_, _, port, targetSSH := config.ParseEndpoint(ep, 8022)
	cfg := config.LoadConfig()
	if _, ok := cfg["sync"]; !ok {
		cfg["sync"] = make(map[string]string)
	}
	chosenAlias := alias
	if chosenAlias == "" {
		chosenAlias = strings.ToLower(name)
	}
	cfg["sync"]["default"] = chosenAlias
	cfg["sync"]["target"] = targetSSH
	cfg["sync"]["port"] = strconv.Itoa(port)
	_ = config.SaveConfig(cfg)
	return true, fmt.Sprintf("Default sync target set to '%s' (%s:%d).", chosenAlias, targetSSH, port)
}

func SaveDefaultTarget(host string, port int, alias string) {
	cfg := config.LoadConfig()
	if _, ok := cfg["sync"]; !ok {
		cfg["sync"] = make(map[string]string)
	}
	cfg["sync"]["target"] = host
	cfg["sync"]["port"] = strconv.Itoa(port)
	if alias != "" {
		cfg["sync"]["default"] = strings.ToLower(alias)
	}
	_ = config.SaveConfig(cfg)
}

func LoadDefaultTarget() (alias string, targetSSH string, port int) {
	cfg := config.LoadConfig()
	devices := LoadDevices()
	localName := config.GetLocalDeviceName()
	localEp := config.GetLocalEndpoint()

	if syncSec, ok := cfg["sync"]; ok {
		defName := strings.ToLower(strings.TrimSpace(syncSec["default"]))
		if dev, exists := devices[defName]; exists && !config.IsLocalTarget(dev.TargetSSH, dev.Port, localEp, localName) {
			return defName, dev.TargetSSH, dev.Port
		}
		if t, ok := syncSec["target"]; ok && strings.TrimSpace(t) != "" {
			p := 8022
			if pStr, ok := syncSec["port"]; ok {
				if pi, err := strconv.Atoi(pStr); err == nil {
					p = pi
				}
			}
			return defName, strings.TrimSpace(t), p
		}
	}

	for name, dev := range devices {
		if !config.IsLocalTarget(dev.TargetSSH, dev.Port, localEp, localName) {
			return name, dev.TargetSSH, dev.Port
		}
	}
	return "", "", 8022
}

func GetClusterTailscaleKey() string {
	// 1. Check environment variables
	for _, envKey := range []string{"TAILSCALE_AUTH_KEY", "TS_AUTHKEY", "TAILSCALE_KEY"} {
		if val := strings.TrimSpace(os.Getenv(envKey)); val != "" {
			return val
		}
	}

	// 2. Check local key files
	keyFiles := []string{
		fmt.Sprintf("%s/.gemini/tailscale_key", config.HomeDir),
		fmt.Sprintf("%s/.tailscale_key", config.HomeDir),
	}
	for _, kf := range keyFiles {
		if data, err := os.ReadFile(kf); err == nil {
			if val := strings.TrimSpace(string(data)); val != "" {
				return val
			}
		}
	}

	// 3. Check local sync.conf
	cfg := config.LoadConfig()
	if cl, ok := cfg["cluster"]; ok {
		if k := strings.TrimSpace(cl["tailscale_auth_key"]); k != "" {
			return k
		}
	}

	devices := LoadDevices()
	localEp := config.GetLocalEndpoint()
	localName := config.GetLocalDeviceName()

	for _, dev := range devices {
		if !config.IsLocalTarget(dev.TargetSSH, dev.Port, localEp, localName) {
			out, _, _, err := ssh.RunSSHCmd(dev.TargetSSH, dev.Port, "grep -E '^tailscale_auth_key' ~/.gemini/sync.conf 2>/dev/null", false, 3)
			if err == nil && strings.Contains(out, "=") {
				parts := strings.SplitN(out, "=", 2)
				val := strings.TrimSpace(parts[1])
				if val != "" {
					if _, ok := cfg["cluster"]; !ok {
						cfg["cluster"] = make(map[string]string)
					}
					cfg["cluster"]["tailscale_auth_key"] = val
					_ = config.SaveConfig(cfg)
					return val
				}
			}
		}
	}
	return ""
}

func BroadcastRegistryToOnlinePeers(showSpinner ...bool) map[string]bool {
	spin := true
	if len(showSpinner) > 0 {
		spin = showSpinner[0]
	}

	devices := LoadDevices()
	localEp := config.GetLocalEndpoint()
	localName := config.GetLocalDeviceName()

	var targets []DeviceTarget
	for _, dev := range devices {
		if !config.IsLocalTarget(dev.TargetSSH, dev.Port, localEp, localName) {
			targets = append(targets, dev)
		}
	}

	results := make(map[string]bool)
	if len(targets) == 0 {
		return results
	}

	var spinner *terminal.Spinner
	if spin {
		spinner = terminal.NewSpinner(fmt.Sprintf("Broadcasting registry to %d peers", len(targets)))
		spinner.Start()
	}

	regData, err := os.ReadFile(config.DevicesJSONFile)
	if err != nil {
		if spinner != nil {
			spinner.Stop()
		}
		return results
	}

	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, t := range targets {
		wg.Add(1)
		go func(target DeviceTarget) {
			defer wg.Done()
			cmdStr := fmt.Sprintf("%s mkdir -p ~/.gemini && cat > ~/.gemini/devices.json.tmp && mv -f ~/.gemini/devices.json.tmp ~/.gemini/devices.json && (agy-sync refresh-registry 2>/dev/null || true)", config.PathPrefix)
			sshArgs := ssh.GetSSHCmd(target.TargetSSH, target.Port, cmdStr, true)
			proc := exec.Command("ssh", sshArgs...)
			proc.Stdin = strings.NewReader(string(regData))
			err := proc.Run()

			mu.Lock()
			results[target.TargetSSH] = (err == nil)
			mu.Unlock()
		}(t)
	}

	wg.Wait()
	if spinner != nil {
		spinner.Stop()
	}
	return results
}

func BroadcastAuthorizedKeyToOnlinePeers(pubkey string) {
	pubkey = strings.TrimSpace(pubkey)
	if pubkey == "" {
		return
	}
	devices := LoadDevices()
	localEp := config.GetLocalEndpoint()
	localName := config.GetLocalDeviceName()

	var wg sync.WaitGroup
	for _, dev := range devices {
		if !config.IsLocalTarget(dev.TargetSSH, dev.Port, localEp, localName) {
			wg.Add(1)
			go func(d DeviceTarget) {
				defer wg.Done()
				cmd := "mkdir -p ~/.ssh && chmod 700 ~/.ssh && cat >> ~/.ssh/authorized_keys && sort -u ~/.ssh/authorized_keys -o ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys"
				_, _, _, _ = ssh.RunSSHCmdWithStdin(d.TargetSSH, d.Port, cmd, strings.NewReader(pubkey+"\n"), false)
			}(dev)
		}
	}
	wg.Wait()
}

func ResetAlias(oldTarget string, showCard ...bool) (bool, string) {
	show := true
	if len(showCard) > 0 {
		show = showCard[0]
	}

	endpoint, currAlias := ssh.FindEndpoint(oldTarget)
	if endpoint == "" {
		return false, fmt.Sprintf("Target '%s' not found in cluster registry.", oldTarget)
	}

	localEp := config.GetLocalEndpoint()
	registry := config.LoadRegistry()
	isLocal := (endpoint == localEp)

	idType := "Codename"
	defaultName := "node"

	if isLocal {
		idType, defaultName = config.GetLocalIdentity()
	} else if d, ok := registry[endpoint]; ok && d.NativeName != "" {
		defaultName = d.NativeName
		idType = d.IdType
	} else {
		_, host, port, _ := config.ParseEndpoint(endpoint, 8022)
		effH, effP, _, _ := network.ResolveFastestEndpoint(host, port, oldTarget)
		ok, _, banner := network.ProbeSSHBanner(effH, effP, 2*time.Second)
		if ok && banner != "" {
			defaultName = host
		}
	}

	cleanDef := strings.ToLower(safeHostCharRegex.ReplaceAllString(defaultName, ""))
	if cleanDef == "" || config.ReservedAliases[cleanDef] {
		cleanDef = fmt.Sprintf("node-%s", cleanDef)
	}

	uniqueAlias := GetUniqueAlias(cleanDef, endpoint, registry)
	rec := registry[endpoint]
	if rec == nil {
		rec = &config.DeviceEntry{}
	}
	rec.Alias = uniqueAlias
	rec.NativeName = cleanDef
	rec.IdType = idType
	rec.Deleted = false
	rec.UpdatedAt = float64(time.Now().UnixNano()) / 1e9
	registry[endpoint] = rec

	_ = config.SaveRegistry(registry)
	ReconcileRegistry()

	bRes := BroadcastRegistryToOnlinePeers(false)
	msg := fmt.Sprintf("Reset alias: '%s' → '%s' (%s)", currAlias, uniqueAlias, endpoint)
	if show {
		terminal.RenderAliasCard("Alias Reset · Cluster Mesh Updated", oldTarget, fmt.Sprintf("%s → %s (native)", currAlias, uniqueAlias), endpoint, bRes)
	}
	return true, msg
}

func RenameAlias(oldTarget, newAlias string, showCard ...bool) (bool, string) {
	show := true
	if len(showCard) > 0 {
		show = showCard[0]
	}

	if strings.TrimSpace(newAlias) == "" {
		return ResetAlias(oldTarget, show)
	}

	valid, validAlias := ValidateAlias(newAlias)
	if !valid {
		return false, validAlias
	}

	endpoint, currAlias := ssh.FindEndpoint(oldTarget)
	if endpoint == "" {
		return false, fmt.Sprintf("Target '%s' not found in cluster registry.", oldTarget)
	}

	if strings.EqualFold(currAlias, validAlias) {
		return true, fmt.Sprintf("Alias is already '%s'.", validAlias)
	}

	registry := config.LoadRegistry()
	for ep, data := range registry {
		if ep != endpoint && !data.Deleted && strings.EqualFold(data.Alias, validAlias) {
			return false, fmt.Sprintf("Alias '%s' is already in use by %s.", validAlias, ep)
		}
	}

	rec := registry[endpoint]
	if rec == nil {
		rec = &config.DeviceEntry{}
	}
	rec.Alias = validAlias
	rec.Deleted = false
	rec.UpdatedAt = float64(time.Now().UnixNano()) / 1e9
	if rec.NativeName == "" {
		rec.NativeName = currAlias
	}
	registry[endpoint] = rec

	_ = config.SaveRegistry(registry)
	ReconcileRegistry()

	bRes := BroadcastRegistryToOnlinePeers(false)
	msg := fmt.Sprintf("Renamed alias: '%s' → '%s' (%s)", currAlias, validAlias, endpoint)
	if show {
		terminal.RenderAliasCard("Alias Renamed · Cluster Mesh Updated", oldTarget, fmt.Sprintf("%s → %s", currAlias, validAlias), endpoint, bRes)
	}
	return true, msg
}

func SetLocalName(newName string) (bool, string) {
	if strings.TrimSpace(newName) == "" {
		return ResetAlias("self")
	}
	return RenameAlias(config.GetLocalEndpoint(), newName)
}

func ManageKeepalive(action string) {
	switch action {
	case "stop":
		if data, err := os.ReadFile(config.KeepalivePIDFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGTERM)
			}
			_ = os.Remove(config.KeepalivePIDFile)
		}
		fmt.Println("[ℹ️] Keepalive daemon stopped.")
	case "restart":
		ManageKeepalive("stop")
		ManageKeepalive("start")
	case "status":
		running := false
		if data, err := os.ReadFile(config.KeepalivePIDFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				if err := syscall.Kill(pid, 0); err == nil {
					running = true
					fmt.Printf("[🟢] Keepalive daemon is running (PID: %d)\n", pid)
				}
			}
		}
		if !running {
			fmt.Println("[🔴] Keepalive daemon is not running.")
		}
	default:
		// start
		if data, err := os.ReadFile(config.KeepalivePIDFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				if err := syscall.Kill(pid, 0); err == nil {
					return
				}
			}
		}
		pid := os.Getpid()
		_ = os.WriteFile(config.KeepalivePIDFile, []byte(strconv.Itoa(pid)), 0644)
	}
}
