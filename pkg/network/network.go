package network

import (
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"agy-sync/pkg/config"
)

type LanNetwork struct {
	Interface string
	IP        string
	Net       *net.IPNet
}

var (
	lanNetsCache   []LanNetwork
	lanNetsLock    sync.Mutex
	reIfconfigIP   = regexp.MustCompile(`inet\s+(?:addr:)?(\d+\.\d+\.\d+\.\d+)`)
	reIfconfigMask = regexp.MustCompile(`netmask\s+(?:addr:)?(\d+\.\d+\.\d+\.\d+)`)
)

var ignoredPrefixes = []string{
	"lo", "rmnet", "ccmni", "pdp", "dummy", "tun", "tap",
	"tailscale", "sbt", "sbd", "docker", "virbr", "veth",
	"wwan", "cellular", "ccinet",
}

func ClearNetworkCache() {
	lanNetsLock.Lock()
	defer lanNetsLock.Unlock()
	lanNetsCache = nil
	config.ClearLocalNetworkCache()
}

func GetLocalLanNetworks(forceRefresh ...bool) []LanNetwork {
	lanNetsLock.Lock()
	defer lanNetsLock.Unlock()

	force := len(forceRefresh) > 0 && forceRefresh[0]
	if lanNetsCache != nil && !force {
		res := make([]LanNetwork, len(lanNetsCache))
		copy(res, lanNetsCache)
		return res
	}

	var networks []LanNetwork

	// 1. Fast-path: Go native net.Interfaces (0-subprocess, <0.1ms)
	if ifaces, err := net.Interfaces(); err == nil {
		for _, iface := range ifaces {
			ignored := false
			for _, pfx := range ignoredPrefixes {
				if strings.HasPrefix(iface.Name, pfx) {
					ignored = true
					break
				}
			}
			if ignored {
				continue
			}

			if addrs, err := iface.Addrs(); err == nil {
				for _, addr := range addrs {
					if ipNet, ok := addr.(*net.IPNet); ok {
						if ip4 := ipNet.IP.To4(); ip4 != nil && !ip4.IsLoopback() && !ip4.IsLinkLocalUnicast() {
							networks = append(networks, LanNetwork{
								Interface: iface.Name,
								IP:        ip4.String(),
								Net:       ipNet,
							})
						}
					}
				}
			}
		}
	}

	if len(networks) > 0 {
		lanNetsCache = networks
		res := make([]LanNetwork, len(networks))
		copy(res, networks)
		return res
	}

	// 2. Fallback: Try ifconfig output
	if out, err := exec.Command("ifconfig").Output(); err == nil {
		rawLines := strings.Split(string(out), "\n")
		var blocks []string
		var curBlock []string
		for _, line := range rawLines {
			trimmed := strings.TrimRight(line, "\r")
			if len(trimmed) > 0 && trimmed[0] != ' ' && trimmed[0] != '\t' {
				if len(curBlock) > 0 {
					blocks = append(blocks, strings.Join(curBlock, "\n"))
					curBlock = nil
				}
			}
			if len(trimmed) > 0 {
				curBlock = append(curBlock, trimmed)
			}
		}
		if len(curBlock) > 0 {
			blocks = append(blocks, strings.Join(curBlock, "\n"))
		}
		for _, block := range blocks {
			lines := strings.Split(strings.TrimSpace(block), "\n")
			if len(lines) == 0 {
				continue
			}
			firstLine := lines[0]
			parts := strings.Fields(strings.ReplaceAll(firstLine, ":", " "))
			if len(parts) == 0 {
				continue
			}
			ifname := parts[0]
			ignored := false
			for _, pfx := range ignoredPrefixes {
				if strings.HasPrefix(ifname, pfx) {
					ignored = true
					break
				}
			}
			if ignored {
				continue
			}

			mIP := reIfconfigIP.FindStringSubmatch(block)
			mMask := reIfconfigMask.FindStringSubmatch(block)
			if len(mIP) > 1 && len(mMask) > 1 {
				ipStr := mIP[1]
				maskStr := mMask[1]
				maskIP := net.ParseIP(maskStr).To4()
				if maskIP != nil {
					mask := net.IPv4Mask(maskIP[0], maskIP[1], maskIP[2], maskIP[3])
					ip := net.ParseIP(ipStr).To4()
					if ip != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
						ipNet := &net.IPNet{IP: ip.Mask(mask), Mask: mask}
						networks = append(networks, LanNetwork{
							Interface: ifname,
							IP:        ipStr,
							Net:       ipNet,
						})
					}
				}
			}
		}
	}

	// 2. If not found, try ip -o -4 addr show
	if len(networks) == 0 {
		for _, cmdArgs := range [][]string{{"ip", "-o", "-4", "addr", "show"}, {"/system/bin/ip", "-o", "-4", "addr", "show"}} {
			if out, err := exec.Command(cmdArgs[0], cmdArgs[1:]...).Output(); err == nil {
				for _, line := range strings.Split(string(out), "\n") {
					parts := strings.Fields(line)
					if len(parts) >= 4 {
						ifname := parts[1]
						ignored := false
						for _, pfx := range ignoredPrefixes {
							if strings.HasPrefix(ifname, pfx) {
								ignored = true
								break
							}
						}
						if ignored {
							continue
						}
						cidr := parts[3]
						ip, ipNet, err := net.ParseCIDR(cidr)
						if err == nil && ip.To4() != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
							networks = append(networks, LanNetwork{
								Interface: ifname,
								IP:        ip.String(),
								Net:       ipNet,
							})
						}
					}
				}
				if len(networks) > 0 {
					break
				}
			}
		}
	}

	lanNetsCache = networks
	res := make([]LanNetwork, len(networks))
	copy(res, networks)
	return res
}

func GetLocalLanIPs() []string {
	nets := GetLocalLanNetworks()
	var ips []string
	for _, n := range nets {
		ips = append(ips, n.IP)
	}
	return ips
}

func ProbeSSHBanner(ip string, port int, timeout time.Duration) (bool, float64, string) {
	t0 := time.Now()
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false, float64(time.Since(t0).Microseconds()) / 1000.0, ""
	}
	defer conn.Close()

	tcpLat := float64(time.Since(t0).Microseconds()) / 1000.0
	_ = conn.SetDeadline(time.Now().Add(timeout))

	// Send probe banner
	_, _ = conn.Write([]byte("SSH-2.0-AgyProbe\r\n"))

	var buf [256]byte
	n, err := conn.Read(buf[:])
	totalLat := float64(time.Since(t0).Microseconds()) / 1000.0

	if err == nil && n > 0 {
		banner := strings.TrimSpace(string(buf[:n]))
		if idx := strings.IndexByte(banner, '\n'); idx >= 0 {
			banner = strings.TrimSpace(banner[:idx])
		}
		if strings.HasPrefix(banner, "SSH-2.0-") {
			effLat := tcpLat
			if tcpLat < 10.0 && totalLat >= 10.0 {
				effLat = totalLat
			}
			return true, effLat, banner
		}
	}

	return false, totalLat, ""
}

func ProbeLanSSHBanner(ip string, port int, timeout ...time.Duration) (bool, float64, string) {
	t := 600 * time.Millisecond
	if len(timeout) > 0 {
		t = timeout[0]
	}
	return ProbeSSHBanner(ip, port, t)
}

func IsPrivateIPv4(ipStr string) bool {
	ip := net.ParseIP(ipStr).To4()
	if ip == nil {
		return false
	}
	return ip.IsPrivate()
}

func ResolveFastestEndpoint(host string, port int, peerName string) (effectiveHost string, effectivePort int, routeTier string, latencyMs float64) {
	user, hOnly, _, _ := config.ParseEndpoint(host, port)
	lookupHost := hOnly
	if lookupHost == "" {
		lookupHost = host
	}

	registry := config.LoadRegistry()
	var peerData *config.DeviceEntry

	for ep, d := range registry {
		if d.Deleted {
			continue
		}
		_, eHost, _, _ := config.ParseEndpoint(ep, 8022)
		if eHost == lookupHost || ep == fmt.Sprintf("%s:%d", lookupHost, port) {
			peerData = d
			break
		}
		if peerName != "" && strings.EqualFold(d.Alias, peerName) {
			peerData = d
			break
		}
		if peerName != "" && strings.EqualFold(d.NativeName, peerName) {
			peerData = d
			break
		}
	}

	effectiveUser := user
	if effectiveUser == "" && peerData != nil {
		effectiveUser = peerData.User
	}

	defRetHost := host
	if effectiveUser != "" && !strings.Contains(host, "@") {
		defRetHost = fmt.Sprintf("%s@%s", effectiveUser, host)
	}

	// 2. Check local network context: If local device has no Wi-Fi/Ethernet, skip LAN probe immediately (0ms cost)
	localNets := GetLocalLanNetworks()
	if len(localNets) == 0 {
		return defRetHost, port, "Tailnet Mesh", 0.0
	}

	// 3. Gather candidate LAN IPs
	var candidates []string
	candSet := make(map[string]bool)

	if peerData != nil {
		for _, ip := range peerData.LanIps {
			if ip != "" && ip != "127.0.0.1" && ip != "localhost" && ip != "0.0.0.0" && ip != "::1" && !candSet[ip] {
				candidates = append(candidates, ip)
				candSet[ip] = true
			}
		}
	}

	if IsPrivateIPv4(lookupHost) && !candSet[lookupHost] {
		candidates = append(candidates, lookupHost)
		candSet[lookupHost] = true
	}

	if len(candidates) == 0 {
		return defRetHost, port, "Tailnet Mesh", 0.0
	}

	// 4. Filter candidates by subnet overlap with local Wi-Fi/Ethernet interfaces
	var subnetCandidates []string
	for _, cand := range candidates {
		ip := net.ParseIP(cand).To4()
		if ip == nil {
			continue
		}
		for _, netw := range localNets {
			if netw.Net != nil && netw.Net.Contains(ip) {
				subnetCandidates = append(subnetCandidates, cand)
				break
			}
		}
	}

	if len(subnetCandidates) == 0 {
		return defRetHost, port, "Tailnet Mesh", 0.0
	}

	// 5. Active probe: Non-blocking SSH banner check
	type probeResult struct {
		ip      string
		ok      bool
		lat     float64
		banner  string
	}

	if len(subnetCandidates) == 1 {
		cand := subnetCandidates[0]
		ok, lat, banner := ProbeLanSSHBanner(cand, port, 600*time.Millisecond)
		if ok {
			effH := cand
			if effectiveUser != "" {
				effH = fmt.Sprintf("%s@%s", effectiveUser, cand)
			}
			return effH, port, "LAN Direct", lat
		}
		_ = banner
	} else if len(subnetCandidates) > 1 {
		resChan := make(chan probeResult, len(subnetCandidates))
		var wg sync.WaitGroup

		for _, cand := range subnetCandidates {
			wg.Add(1)
			go func(ip string) {
				defer wg.Done()
				ok, lat, banner := ProbeLanSSHBanner(ip, port, 600*time.Millisecond)
				resChan <- probeResult{ip: ip, ok: ok, lat: lat, banner: banner}
			}(cand)
		}

		// Close resChan when all finished
		go func() {
			wg.Wait()
			close(resChan)
		}()

		// Wait for first successful probe
		for res := range resChan {
			if res.ok {
				effH := res.ip
				if effectiveUser != "" {
					effH = fmt.Sprintf("%s@%s", effectiveUser, res.ip)
				}
				return effH, port, "LAN Direct", res.lat
			}
		}
	}

	return defRetHost, port, "Tailnet Mesh", 0.0
}

func CmdProbeLAN(target, candIP string, port int) int {
	localNets := GetLocalLanNetworks()
	if len(localNets) == 0 {
		return 1
	}

	registry := config.LoadRegistry()
	var targetData *config.DeviceEntry
	var targetEP string

	for ep, d := range registry {
		if d.Deleted || config.IsLocalEndpoint(ep, d) {
			continue
		}
		if strings.EqualFold(d.Alias, target) || strings.EqualFold(d.NativeName, target) {
			targetData = d
			targetEP = ep
			break
		}
		if ep == target || strings.HasPrefix(ep, target+":") {
			targetData = d
			targetEP = ep
			break
		}
	}

	targetPort := port
	if targetPort <= 0 {
		if targetEP != "" {
			_, _, p, _ := config.ParseEndpoint(targetEP, 8022)
			targetPort = p
		} else {
			targetPort = 8022
		}
	}

	if candIP != "" {
		ip := net.ParseIP(candIP).To4()
		if ip == nil {
			return 1
		}
		inSubnet := false
		for _, netw := range localNets {
			if netw.Net != nil && netw.Net.Contains(ip) {
				inSubnet = true
				break
			}
		}
		if !inSubnet {
			return 1
		}
		ok, _, _ := ProbeLanSSHBanner(candIP, targetPort, 600*time.Millisecond)
		if ok {
			fmt.Println(candIP)
			return 0
		}
		return 1
	}

	if targetData == nil {
		return 1
	}

	_, host, regPort, _ := config.ParseEndpoint(targetEP, 8022)
	effPort := targetPort
	if effPort <= 0 {
		effPort = regPort
	}
	effH, _, rTier, _ := ResolveFastestEndpoint(host, effPort, targetData.Alias)
	if strings.Contains(rTier, "LAN") {
		fmt.Println(effH)
		return 0
	}
	return 1
}

