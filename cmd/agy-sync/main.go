package main

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"agy-sync/pkg/cluster"
	"agy-sync/pkg/config"
	"agy-sync/pkg/display"
	"agy-sync/pkg/network"
	"agy-sync/pkg/ssh"
	"agy-sync/pkg/sync"
)

func printHelp() {
	fmt.Printf(`
Antigravity Cross-Device Full Bidirectional Synchronizer (agy-sync v%s)

Basic Usage:
  agy-sync                                  Sync with default peer (Bento Dashboard)
  agy-sync all [--verbose|-V]               Sync with all enrolled cluster peers (Matrix Table)
  agy-sync <alias> [--verbose|-V]           Sync with specific peer (e.g. agy-sync peer1)
  agy-sync ssh [alias] [args...]            Interactive SSH session to peer node (or native ssh <peer>)
  agy-sync probe-lan <alias> [ip]           Probe if peer is directly reachable over LAN
  agy-sync is-peer <target>                 Check if host/alias is an enrolled cluster peer
  agy-sync push-settings [alias]            Force-push local settings.json to cluster (or peer)
  agy-sync push-pool [alias]                Force-push local agy-pool accounts.json to cluster (or peer)
  agy-sync <host>[:port] [--save]           Sync with ad-hoc IP/host (optionally save)
  agy-sync items                            Display tracked assets manifest
  agy-sync devices                          List enrolled devices, ping, status & hardware

One-Click Automated Deployment & Enrollment:
  agy-sync enroll <target> [alias] [opts]   Zero-touch auto-deploy to remote SSH host
  agy-sync deploy <target> [alias] [opts]   Alias for enroll
  agy-sync pair [port]                      Start 1-click HTTP pairing server for client
  agy-sync invite [port]                    Alias for pair

Alias & Device Management:
  agy-sync alias <old> [new]                Rename alias (omit new to reset to native)
  agy-sync device add <alias> <host>[:port] Register a new device to the cluster
  agy-sync device default <alias>           Set the default target peer
  agy-sync device remove <alias>            Remove a node from the cluster
  agy-sync keepalive [status|stop|restart]  Tailnet NAT background keepalive & Termux guard
  agy-sync -V, --verbose                    Verbose step-by-step stream (bypasses collapsed dashboard)
  agy-sync -v, --version                    Show version information
  agy-sync -h, --help                       Show this help message

`, config.Version)
}

func main() {
	args := os.Args[1:]

	// Fast-path: is-peer, version, commit, help (sub-millisecond response)
	if len(args) >= 2 && args[0] == "is-peer" {
		os.Exit(ssh.CmdIsPeer(args[1]))
	}
	if len(args) == 1 {
		if args[0] == "--commit" || args[0] == "commit" {
			fmt.Println(config.GitCommit)
			return
		}
		if args[0] == "-v" || args[0] == "--version" || args[0] == "version" {
			fmt.Printf("Antigravity Cross-Device Synchronizer (agy-sync v%s @ %s, %s/%s)\n", config.Version, config.GitCommit, runtime.GOOS, runtime.GOARCH)
			return
		}
		if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
			printHelp()
			return
		}
	}

	config.EnsureTermuxEnvironment()
	config.EnsureSelfUpToDate()
	cluster.ReconcileRegistry()

	if len(args) == 0 {
		// Sync with default peer
		defAlias, defSSH, defPort := cluster.LoadDefaultTarget()
		if defAlias == "" && defSSH == "" {
			fmt.Println("[❌] No default target configured. Set one with: agy-sync device default <alias>")
			os.Exit(1)
		}
		targetHost := defSSH
		if targetHost == "" {
			targetHost = defAlias
		}
		sync.SyncWithPeer(targetHost, defPort, defAlias, false, false, nil, nil)
		return
	}

	cmd := args[0]
	switch cmd {
	case "help", "-h", "--help":
		printHelp()
		return
	case "version", "-v", "--version":
		fmt.Printf("Antigravity Cross-Device Synchronizer (agy-sync v%s @ %s, %s/%s)\n", config.Version, config.GitCommit, runtime.GOOS, runtime.GOARCH)
		return
	case "commit", "--commit":
		fmt.Println(config.GitCommit)
		return
	case "devices", "nodes", "list-devices":
		verbose := false
		for _, a := range args[1:] {
			if a == "-v" || a == "-V" || a == "--verbose" {
				verbose = true
			}
		}
		display.ShowDevices(verbose)
		return
	case "items", "manifest", "list", "list-items":
		display.ShowItems()
		return
	case "ssh", "connect":
		ssh.HandleSSHCommand(args[1:])
		return
	case "probe-lan":
		if len(args) < 2 {
			fmt.Println("Usage: agy-sync probe-lan <target> [cand_ip] [port]")
			os.Exit(1)
		}
		candIP := ""
		if len(args) >= 3 {
			candIP = args[2]
		}
		port := 0
		if len(args) >= 4 {
			port, _ = strconv.Atoi(args[3])
		}
		os.Exit(network.CmdProbeLAN(args[1], candIP, port))
		return
	case "push-settings", "settings":
		target := ""
		if len(args) >= 2 {
			target = args[1]
		}
		sync.PushSettingsToPeers(target)
		return
	case "push-pool", "push-accounts", "pool-push":
		target := ""
		if len(args) >= 2 {
			target = args[1]
		}
		sync.PushPoolToPeers(target)
		return
	case "all", "--all":
		verbose := false
		for _, a := range args[1:] {
			if a == "-V" || a == "--verbose" {
				verbose = true
			}
		}
		sync.SyncAllPeers(verbose)
		return
	case "alias", "rename":
		if len(args) < 2 {
			fmt.Println("Usage: agy-sync alias <old_alias> [new_alias]")
			os.Exit(1)
		}
		oldTarget := args[1]
		newAlias := ""
		if len(args) >= 3 {
			newAlias = args[2]
		}
		if newAlias == "" {
			cluster.ResetAlias(oldTarget, true)
		} else {
			cluster.RenameAlias(oldTarget, newAlias, true)
		}
		return
	case "unalias", "reset-alias":
		if len(args) < 2 {
			fmt.Println("Usage: agy-sync unalias <alias>")
			os.Exit(1)
		}
		cluster.ResetAlias(args[1], true)
		return
	case "self", "set-self":
		if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
			cluster.ResetAlias("self", true)
		} else {
			ok, msg := cluster.SetLocalName(args[1])
			if ok {
				fmt.Printf("[✅] %s\n", msg)
			} else {
				fmt.Printf("[❌] %s\n", msg)
				os.Exit(1)
			}
		}
		return
	case "enroll", "deploy":
		sync.CmdEnroll(args[1:])
		return
	case "pair", "invite":
		sync.CmdPair(args[1:])
		return
	case "device":
		if len(args) < 2 || strings.EqualFold(args[1], "list") || strings.EqualFold(args[1], "show") {
			display.ShowDevices(false)
			return
		}
		subCmd := strings.ToLower(args[1])
		switch subCmd {
		case "add":
			if len(args) < 4 {
				fmt.Println("Usage: agy-sync device add <alias> <host>[:port]")
				os.Exit(1)
			}
			al := args[2]
			ep := args[3]
			_, host, port, _ := config.ParseEndpoint(ep, 8022)
			cluster.SaveDevice(al, host, port)
			fmt.Printf("[✅] Registered device '%s' -> %s:%d\n", al, host, port)
		case "remove", "rm", "del", "delete":
			if len(args) < 3 {
				fmt.Println("Usage: agy-sync device remove <alias>")
				os.Exit(1)
			}
			ok, msg := cluster.RemoveDevice(args[2])
			if ok {
				fmt.Printf("[✅] %s\n", msg)
			} else {
				fmt.Printf("[❌] %s\n", msg)
				os.Exit(1)
			}
		case "default", "set-default":
			if len(args) < 3 {
				fmt.Println("Usage: agy-sync device default <alias>")
				os.Exit(1)
			}
			ok, msg := cluster.SetDefaultDevice(args[2])
			if ok {
				fmt.Printf("[✅] %s\n", msg)
			} else {
				fmt.Printf("[❌] %s\n", msg)
				os.Exit(1)
			}
		case "alias", "rename":
			if len(args) < 3 {
				fmt.Println("Usage: agy-sync device alias <old_alias> [new_alias]")
				os.Exit(1)
			}
			target := args[2]
			newAlias := ""
			if len(args) >= 4 {
				newAlias = args[3]
			}
			if newAlias == "" {
				cluster.ResetAlias(target, true)
			} else {
				cluster.RenameAlias(target, newAlias, true)
			}
		case "unalias", "reset-alias":
			if len(args) < 3 {
				fmt.Println("Usage: agy-sync device unalias <alias>")
				os.Exit(1)
			}
			cluster.ResetAlias(args[2], true)
		case "self", "set-self", "local", "set-local":
			if len(args) < 3 || strings.TrimSpace(args[2]) == "" {
				cluster.ResetAlias("self", true)
			} else {
				ok, msg := cluster.SetLocalName(args[2])
				if ok {
					fmt.Printf("[✅] %s\n", msg)
				} else {
					fmt.Printf("[❌] %s\n", msg)
					os.Exit(1)
				}
			}
		default:
			fmt.Printf("[❌] Unknown device action '%s'. Use add, remove, default, alias, or list.\n", subCmd)
			os.Exit(1)
		}
		return
	case "keepalive", "daemon", "ping":
		action := "start"
		if len(args) >= 2 {
			action = args[1]
		}
		cluster.ManageKeepalive(action)
		return
	case "reconcile", "refresh-registry":
		cluster.ReconcileRegistry()
		fmt.Println("[✅] Registry, SSH configuration, and shell integration refreshed.")
		return
	default:
		// Target peer or ad-hoc endpoint sync
		verbose := false
		saveTarget := false
		customPort := 0
		var cleanArgs []string

		i := 0
		for i < len(args) {
			a := args[i]
			if a == "-V" || a == "--verbose" {
				verbose = true
				i++
			} else if a == "--save" {
				saveTarget = true
				i++
			} else if (a == "-p" || a == "--port") && i+1 < len(args) {
				if p, err := strconv.Atoi(args[i+1]); err == nil {
					customPort = p
				}
				i += 2
			} else {
				cleanArgs = append(cleanArgs, a)
				i++
			}
		}

		if len(cleanArgs) == 0 {
			defAlias, defSSH, defPort := cluster.LoadDefaultTarget()
			if defAlias == "" && defSSH == "" {
				fmt.Println("[❌] No default target configured. Set one with: agy-sync device default <alias>")
				os.Exit(1)
			}
			targetHost := defSSH
			if targetHost == "" {
				targetHost = defAlias
			}
			sync.SyncWithPeer(targetHost, defPort, defAlias, verbose, false, nil, nil)
			return
		}

		target := cleanArgs[0]
		ep, matchedAlias := ssh.FindEndpoint(target)
		if ep != "" {
			if config.IsLocalEndpoint(ep, nil) {
				fmt.Printf("[⚠️] Cannot sync with local device '%s'.\n", matchedAlias)
				os.Exit(1)
			}
			_, host, port, targetSSH := config.ParseEndpoint(ep, 8022)
			_ = host
			if customPort > 0 {
				port = customPort
			}
			if saveTarget {
				cluster.SaveDefaultTarget(targetSSH, port, matchedAlias)
				fmt.Printf("[✅] Saved default target: %s:%d\n", targetSSH, port)
			}
			sync.SyncWithPeer(targetSSH, port, matchedAlias, verbose, false, nil, nil)
			return
		}

		// Ad-hoc host or host:port
		defaultPort := 8022
		if customPort > 0 {
			defaultPort = customPort
		}
		_, host, port, targetSSH := config.ParseEndpoint(target, defaultPort)
		if host != "" {
			if config.IsLocalTarget(targetSSH, port, config.GetLocalEndpoint(), config.GetLocalDeviceName()) {
				fmt.Printf("[⚠️] Target '%s:%d' is this local device. Cannot sync a device with itself.\n", targetSSH, port)
				os.Exit(1)
			}
			if saveTarget {
				cluster.SaveDefaultTarget(targetSSH, port, "")
				fmt.Printf("[✅] Saved default target: %s:%d\n", targetSSH, port)
			}
			sync.SyncWithPeer(targetSSH, port, host, verbose, false, nil, nil)
			return
		}

		fmt.Printf("[❌] Unknown command or device '%s'. Run 'agy-sync --help' for usage.\n", target)
		os.Exit(1)
	}
}
