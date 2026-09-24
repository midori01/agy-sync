package config

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	HomeDir             string
	GeminiDir           string
	CliDir              string
	BrainDir            string
	ConvDir             string
	AnnDir              string
	ConfigDir           string
	HandoffFile         string
	RulesFile           string
	ConfFile            string
	DevicesJSONFile     string
	AuthKeysFile        string
	SSHConfigFile       string
	PeersListFile       string
	KeepalivePIDFile    string
	AgySyncScript       string
	SettingsFile        string
	ProtoSummariesFile  string
	HistoryFile         string
	SummariesDBFile     string
	OAuthTokenFile      string
	PoolAccountsFile    string
	PoolDir             string
	AgySyncGoDir        string
	SocketDir           string
)

const (
	Version               = "6.7"
	SSHConfigMarkerStart  = "# >>> agy-sync cluster devices >>>"
	SSHConfigMarkerEnd    = "# <<< agy-sync cluster devices <<<"
	ShellPeerMarkerStart  = "# >>> agy-sync ssh peer integration >>>"
	ShellPeerMarkerEnd    = "# <<< agy-sync ssh peer integration <<<"
	PathPrefix            = "export PATH=$HOME/.gemini/antigravity-cli/bin:$HOME/.local/bin:/data/data/com.termux/files/usr/bin:/usr/local/bin:/usr/bin:/bin:$PATH;"
)

var ReservedAliases = map[string]bool{
	"all": true, "items": true, "devices": true, "device": true, "nodes": true,
	"pair": true, "invite": true, "enroll": true, "deploy": true, "alias": true,
	"rename": true, "unalias": true, "self": true, "local": true, "me": true,
	"this": true, "help": true, "ssh": true, "connect": true, "keepalive": true,
	"ping": true, "daemon": true, "manifest": true, "list": true, "reconcile": true,
	"refresh-registry": true, "version": true, "probe-lan": true,
	"push-settings": true, "settings": true, "is-peer": true,
}

var SafeAliasRegex = regexp.MustCompile(`^[a-zA-Z0-9_\-\.]+$`)

func init() {
	HomeDir = os.Getenv("HOME")
	if HomeDir == "" {
		if h, err := os.UserHomeDir(); err == nil {
			HomeDir = h
		} else {
			HomeDir = "/data/data/com.termux/files/home"
		}
	}

	GeminiDir = filepath.Join(HomeDir, ".gemini")
	CliDir = filepath.Join(GeminiDir, "antigravity-cli")
	BrainDir = filepath.Join(CliDir, "brain")
	ConvDir = filepath.Join(CliDir, "conversations")
	AnnDir = filepath.Join(CliDir, "annotations")
	ConfigDir = filepath.Join(GeminiDir, "config")
	HandoffFile = filepath.Join(GeminiDir, "session_handoff.md")
	RulesFile = filepath.Join(HomeDir, "GEMINI.md")
	ConfFile = filepath.Join(GeminiDir, "sync.conf")
	DevicesJSONFile = filepath.Join(GeminiDir, "devices.json")
	AuthKeysFile = filepath.Join(HomeDir, ".ssh", "authorized_keys")
	SSHConfigFile = filepath.Join(HomeDir, ".ssh", "config")
	PeersListFile = filepath.Join(CliDir, "peers.list")
	KeepalivePIDFile = filepath.Join(GeminiDir, "agy-sync-keepalive.pid")
	AgySyncScript = filepath.Join(CliDir, "bin", "agy-sync")
	SettingsFile = filepath.Join(CliDir, "settings.json")
	ProtoSummariesFile = filepath.Join(CliDir, "jetbox_summaries_proto.pb")
	HistoryFile = filepath.Join(CliDir, "history.jsonl")
	SummariesDBFile = filepath.Join(CliDir, "conversation_summaries.db")
	OAuthTokenFile = filepath.Join(CliDir, "antigravity-oauth-token")
	PoolAccountsFile = filepath.Join(GeminiDir, "agy-pool-accounts.json")
	PoolDir = filepath.Join(HomeDir, "agy-pool")
	AgySyncGoDir = filepath.Join(HomeDir, "agy-sync-go")
	SocketDir = filepath.Join(HomeDir, ".ssh", "sockets")

	_ = os.MkdirAll(SocketDir, 0700)
	_ = os.Chmod(SocketDir, 0700)
}

// DeviceEntry represents a node in devices.json.
type DeviceEntry struct {
	Alias        string   `json:"alias"`
	NativeName   string   `json:"native_name"`
	IdType       string   `json:"id_type"`
	Deleted      bool     `json:"deleted"`
	UpdatedAt    float64  `json:"updated_at"`
	User         string   `json:"user,omitempty"`
	LanIps       []string `json:"lan_ips,omitempty"`
	LanUpdatedAt float64  `json:"lan_updated_at,omitempty"`
}

type Registry map[string]*DeviceEntry

// IniConfig represents simple INI structure: section -> key -> value
type IniConfig map[string]map[string]string

func LoadConfig() IniConfig {
	cfg := make(IniConfig)
	f, err := os.Open(ConfFile)
	if err != nil {
		return cfg
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	currentSection := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			currentSection = strings.TrimSpace(line[1 : len(line)-1])
			if _, exists := cfg[currentSection]; !exists {
				cfg[currentSection] = make(map[string]string)
			}
			continue
		}
		if currentSection != "" && strings.Contains(line, "=") {
			parts := strings.SplitN(line, "=", 2)
			k := strings.TrimSpace(parts[0])
			v := strings.TrimSpace(parts[1])
			cfg[currentSection][k] = v
		}
	}
	return cfg
}

func SaveConfig(cfg IniConfig) error {
	_ = os.MkdirAll(filepath.Dir(ConfFile), 0755)
	tmpPath := ConfFile + fmt.Sprintf(".tmp_%d", time.Now().UnixNano())
	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	w := bufio.NewWriter(f)
	// Order sections standardly
	orderedSections := []string{"local", "sync", "devices", "cluster"}
	written := make(map[string]bool)

	for _, sec := range orderedSections {
		if kv, ok := cfg[sec]; ok {
			fmt.Fprintf(w, "[%s]\n", sec)
			for k, v := range kv {
				fmt.Fprintf(w, "%s = %s\n", k, v)
			}
			fmt.Fprintln(w)
			written[sec] = true
		}
	}

	for sec, kv := range cfg {
		if !written[sec] {
			fmt.Fprintf(w, "[%s]\n", sec)
			for k, v := range kv {
				fmt.Fprintf(w, "%s = %s\n", k, v)
			}
			fmt.Fprintln(w)
		}
	}

	_ = w.Flush()
	_ = f.Sync()
	_ = f.Close()

	return os.Rename(tmpPath, ConfFile)
}

func LoadRegistry() Registry {
	reg := make(Registry)
	data, err := os.ReadFile(DevicesJSONFile)
	if err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &reg); err == nil && len(reg) > 0 {
			return reg
		}
	}
	return SeedRegistryFromConfig()
}

func SaveRegistry(reg Registry) error {
	_ = os.MkdirAll(filepath.Dir(DevicesJSONFile), 0755)
	tmpPath := DevicesJSONFile + ".tmp"
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, DevicesJSONFile)
}

func SeedRegistryFromConfig() Registry {
	cfg := LoadConfig()
	localEp := GetLocalEndpoint()
	localIdType, localIdVal := GetLocalIdentity()
	reg := make(Registry)
	now := float64(time.Now().UnixNano()) / 1e9

	devs := make(map[string]string)
	if devSec, ok := cfg["devices"]; ok {
		for k, v := range devSec {
			ep := strings.TrimSpace(v)
			if !strings.Contains(ep, ":") {
				ep = ep + ":8022"
			}
			devs[strings.ToLower(strings.TrimSpace(k))] = ep
		}
	}
	if len(devs) == 0 {
		devs[strings.ToLower(localIdVal)] = localEp
	}

	for name, ep := range devs {
		if ep == localEp {
			reg[ep] = &DeviceEntry{
				Alias:      name,
				NativeName: strings.ToLower(localIdVal),
				IdType:     localIdType,
				Deleted:    false,
				UpdatedAt:  now,
			}
		} else {
			reg[ep] = &DeviceEntry{
				Alias:      name,
				NativeName: name,
				IdType:     "Host",
				Deleted:    false,
				UpdatedAt:  now,
			}
		}
	}
	_ = SaveRegistry(reg)
	return reg
}

// ParseEndpoint splits [user@]host[:port] into user, host, port, targetSSH.
func ParseEndpoint(ep string, defaultPort int) (user, host string, port int, targetSSH string) {
	ep = strings.TrimSpace(ep)
	port = defaultPort
	targetPart := ep

	if strings.Contains(ep, ":") {
		idx := strings.LastIndex(ep, ":")
		pStr := ep[idx+1:]
		if p, err := strconv.Atoi(pStr); err == nil {
			port = p
			targetPart = ep[:idx]
		}
	}

	if strings.Contains(targetPart, "@") {
		parts := strings.SplitN(targetPart, "@", 2)
		user = parts[0]
		host = parts[1]
	} else {
		user = ""
		host = targetPart
	}

	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}

	if user != "" {
		targetSSH = fmt.Sprintf("%s@%s", user, host)
	} else {
		targetSSH = host
	}
	return
}

func ToRemotePath(p string, remHome string) string {
	cleanRemHome := strings.TrimRight(remHome, "/")
	cleanHome := strings.TrimRight(HomeDir, "/")
	if strings.HasPrefix(p, cleanHome) {
		rel := strings.TrimPrefix(p, cleanHome)
		rel = strings.TrimLeft(rel, "/")
		return fmt.Sprintf("%s/%s", cleanRemHome, rel)
	}
	return p
}

// GetLocalIdentity returns (typeLabel, value), e.g. ("Codename", "nezha") or ("Hostname", "box")
func GetLocalIdentity() (string, string) {
	if out, err := exec.Command("getprop", "ro.product.device").Output(); err == nil {
		s := strings.TrimSpace(string(out))
		if s != "" {
			return "Codename", s
		}
	}
	if out, err := exec.Command("getprop", "ro.build.product").Output(); err == nil {
		s := strings.TrimSpace(string(out))
		if s != "" {
			return "Codename", s
		}
	}
	if h, err := os.Hostname(); err == nil && h != "" && h != "localhost" {
		return "Hostname", h
	}
	if out, err := exec.Command("uname", "-n").Output(); err == nil {
		s := strings.TrimSpace(string(out))
		if s != "" {
			return "Hostname", s
		}
	}
	return "Host", "unknown"
}

func GetLocalCodename() string {
	_, val := GetLocalIdentity()
	return val
}

func GetLocalDeviceName() string {
	cfg := LoadConfig()
	if loc, ok := cfg["local"]; ok {
		if n, ok := loc["name"]; ok && strings.TrimSpace(n) != "" {
			return strings.ToLower(strings.TrimSpace(n))
		}
	}
	return strings.ToLower(GetLocalCodename())
}

var (
	localIPsCache   map[string]bool
	localIPsLock    sync.Mutex
)

func ClearLocalNetworkCache() {
	localIPsLock.Lock()
	defer localIPsLock.Unlock()
	localIPsCache = nil
}

func GetLocalIPs(forceRefresh bool) map[string]bool {
	localIPsLock.Lock()
	defer localIPsLock.Unlock()

	if localIPsCache != nil && !forceRefresh {
		res := make(map[string]bool)
		for k, v := range localIPsCache {
			res[k] = v
		}
		return res
	}

	ips := map[string]bool{
		"127.0.0.1": true, "localhost": true, "0.0.0.0": true, "::1": true,
	}

	// 1. Configured local endpoint IP
	cfg := LoadConfig()
	if loc, ok := cfg["local"]; ok {
		if ep, ok := loc["endpoint"]; ok && strings.TrimSpace(ep) != "" {
			_, h, _, _ := ParseEndpoint(ep, 8022)
			if h != "" && !ips[h] {
				ips[h] = true
			}
		}
	}

	// 2. ifconfig inet
	if out, err := exec.Command("ifconfig").Output(); err == nil {
		re := regexp.MustCompile(`inet\s+(?:addr:)?(\d+\.\d+\.\d+\.\d+)`)
		matches := re.FindAllStringSubmatch(string(out), -1)
		for _, m := range matches {
			if len(m) > 1 {
				ips[m[1]] = true
			}
		}
	}

	// 3. hostname -I
	if out, err := exec.Command("hostname", "-I").Output(); err == nil {
		for _, part := range strings.Fields(string(out)) {
			ips[strings.TrimSpace(part)] = true
		}
	}

	// 4. ip -o addr show
	for _, cmdArgs := range [][]string{{"ip", "-o", "addr", "show"}, {"/system/bin/ip", "-o", "addr", "show"}} {
		if out, err := exec.Command(cmdArgs[0], cmdArgs[1:]...).Output(); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				fields := strings.Fields(line)
				if len(fields) >= 4 && strings.Contains(fields[3], "/") {
					ip := strings.Split(fields[3], "/")[0]
					ips[ip] = true
				}
			}
		}
	}

	// 5. Tailscale IP
	if tailscalePath, err := exec.LookPath("tailscale"); err == nil && tailscalePath != "" {
		if out, err := exec.Command("tailscale", "ip", "-4").Output(); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				l := strings.TrimSpace(line)
				if l != "" {
					ips[l] = true
				}
			}
		}
	}

	localIPsCache = ips
	res := make(map[string]bool)
	for k, v := range ips {
		res[k] = v
	}
	return res
}

func GetLocalEndpoint() string {
	cfg := LoadConfig()
	if loc, ok := cfg["local"]; ok {
		if ep, ok := loc["endpoint"]; ok && strings.TrimSpace(ep) != "" {
			trimmed := strings.TrimSpace(ep)
			if !strings.HasPrefix(trimmed, "127.0.0.1") && !strings.HasPrefix(trimmed, "localhost") {
				return trimmed
			}
		}
	}

	localName := GetLocalDeviceName()
	localNative := strings.ToLower(GetLocalCodename())
	localIPs := GetLocalIPs(false)
	realIPs := make(map[string]bool)
	for ip := range localIPs {
		if ip != "127.0.0.1" && ip != "localhost" && ip != "0.0.0.0" && ip != "::1" {
			realIPs[ip] = true
		}
	}

	if data, err := os.ReadFile(DevicesJSONFile); err == nil {
		var reg Registry
		if err := json.Unmarshal(data, &reg); err == nil {
			for ep, d := range reg {
				if d.Deleted {
					continue
				}
				dNative := strings.ToLower(strings.TrimSpace(d.NativeName))
				dAlias := strings.ToLower(strings.TrimSpace(d.Alias))
				_, h, _, _ := ParseEndpoint(ep, 8022)
				if h == "127.0.0.1" || h == "localhost" || h == "0.0.0.0" || h == "::1" {
					continue
				}
				if (dNative != "" && dNative == localNative && (realIPs[h] || len(realIPs) == 0)) ||
					(dAlias != "" && dAlias == localName && (realIPs[h] || len(realIPs) == 0)) ||
					(realIPs[h]) {
					if _, ok := cfg["local"]; !ok {
						cfg["local"] = make(map[string]string)
					}
					cfg["local"]["endpoint"] = ep
					if dAlias != "" {
						cfg["local"]["name"] = dAlias
					}
					_ = SaveConfig(cfg)
					return ep
				}
			}
		}
	}

	if devSec, ok := cfg["devices"]; ok {
		for alias, ep := range devSec {
			_, h, _, _ := ParseEndpoint(ep, 8022)
			if h == "127.0.0.1" || h == "localhost" || h == "0.0.0.0" || h == "::1" {
				continue
			}
			al := strings.ToLower(alias)
			if (al == localName || al == localNative) && (realIPs[h] || len(realIPs) == 0) || realIPs[h] {
				if _, ok := cfg["local"]; !ok {
					cfg["local"] = make(map[string]string)
				}
				cfg["local"]["endpoint"] = ep
				cfg["local"]["name"] = al
				_ = SaveConfig(cfg)
				return ep
			}
		}
	}

	return "127.0.0.1:8022"
}

func IsLocalEndpoint(ep string, data *DeviceEntry) bool {
	localEp := GetLocalEndpoint()
	if ep == localEp {
		return true
	}
	_, host, port, _ := ParseEndpoint(ep, 8022)
	_, locHost, locPort, _ := ParseEndpoint(localEp, 8022)

	if host == "127.0.0.1" || host == "localhost" || host == "0.0.0.0" || host == "::1" {
		if locHost == "127.0.0.1" || locHost == "localhost" || locHost == "0.0.0.0" || locHost == "::1" {
			return port == locPort
		}
		return false
	}

	localIPs := GetLocalIPs(false)
	realIPs := make(map[string]bool)
	for ip := range localIPs {
		if ip != "127.0.0.1" && ip != "localhost" && ip != "0.0.0.0" && ip != "::1" {
			realIPs[ip] = true
		}
	}

	if realIPs[host] && port == locPort {
		return true
	}

	localName := GetLocalDeviceName()
	localNative := strings.ToLower(GetLocalCodename())
	if data != nil {
		dAlias := strings.ToLower(strings.TrimSpace(data.Alias))
		dNative := strings.ToLower(strings.TrimSpace(data.NativeName))
		if dAlias == localName || (dNative != "" && dNative == localNative) {
			if realIPs[host] || host == locHost {
				return port == locPort
			}
		}
	}
	return false
}

func IsLocalTarget(targetStr string, port int, localEp string, localName string) bool {
	_, locHost, locPort, _ := ParseEndpoint(localEp, 8022)
	_, tHost, tPort, _ := ParseEndpoint(targetStr, port)
	if tHost == "127.0.0.1" || tHost == "localhost" || tHost == "0.0.0.0" || tHost == "::1" {
		return tPort == locPort
	}
	localIPs := GetLocalIPs(false)
	realIPs := make(map[string]bool)
	for ip := range localIPs {
		if ip != "127.0.0.1" && ip != "localhost" && ip != "0.0.0.0" && ip != "::1" {
			realIPs[ip] = true
		}
	}
	if realIPs[tHost] && tPort == locPort {
		return true
	}
	if locHost != "" && tHost == locHost && tPort == locPort {
		return true
	}
	return false
}

func GetLocalLANIPs() []string {
	var ips []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return ips
	}
	ignoredPrefixes := []string{
		"lo", "rmnet", "ccmni", "pdp", "dummy", "tun", "tap",
		"tailscale", "sbt", "sbd", "docker", "virbr", "veth",
		"wwan", "cellular", "ccinet",
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		ignored := false
		name := strings.ToLower(iface.Name)
		for _, pfx := range ignoredPrefixes {
			if strings.HasPrefix(name, pfx) {
				ignored = true
				break
			}
		}
		if ignored {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if ok && !ipNet.IP.IsLoopback() && ipNet.IP.To4() != nil {
				ips = append(ips, ipNet.IP.String())
			}
		}
	}
	return ips
}

func GetTailscaleIPs() []string {
	var ips []string
	if tailscalePath, err := exec.LookPath("tailscale"); err == nil && tailscalePath != "" {
		if out, err := exec.Command(tailscalePath, "ip", "-4").Output(); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				l := strings.TrimSpace(line)
				if l != "" && net.ParseIP(l) != nil {
					ips = append(ips, l)
				}
			}
		}
	}
	return ips
}

