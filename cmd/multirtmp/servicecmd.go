package main

import (
	"flag"
	"fmt"
	"os"

	"multirtmp/internal/config"
	"multirtmp/internal/winservice"
)

const (
	serviceName        = "multirtmp"
	serviceDisplayName = "multirtmp RTMP relay"
	serviceDescription = "Relays an RTMP publish (vMix) to multiple RTMP destinations, each with its own delay."
)

// runServiceSubcommand обрабатывает install/uninstall/start/stop/status,
// если os.Args[1] — одна из них. Возвращает true, если подкоманда
// распознана и обработана (вызывающий должен просто завершиться).
func runServiceSubcommand() bool {
	if len(os.Args) < 2 {
		return false
	}

	switch os.Args[1] {
	case "install":
		cmdInstall(os.Args[2:])
	case "uninstall":
		cmdUninstall()
	case "start":
		cmdStart()
	case "stop":
		cmdStop()
	case "status":
		cmdStatus()
	default:
		return false
	}

	return true
}

func cmdInstall(args []string) {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config.json path to pass to the installed service (default: next to the executable)")
	_ = fs.Parse(args)

	exePath, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "multirtmp install: failed to determine executable path:", err)
		os.Exit(1)
	}

	var svcArgs []string
	if *cfgPath != "" {
		svcArgs = []string{"-config", *cfgPath}
	}

	if err := winservice.Install(serviceName, serviceDisplayName, serviceDescription, exePath, svcArgs); err != nil {
		fmt.Fprintln(os.Stderr, "multirtmp install:", err)
		os.Exit(1)
	}

	fmt.Println("service installed:", serviceName)

	// Best-effort: if config.json already exists and is set up for LAN
	// access, open the port now. If it doesn't exist yet (typical on first
	// deployment — the operator hasn't created/edited it), this is skipped
	// silently; runApp checks (and warns with a ready-made command) on every
	// startup regardless, so a missing rule never goes unnoticed even if it
	// wasn't possible to create it here.
	if resolvedCfgPath, err := resolveConfigPath(*cfgPath); err == nil {
		if cfg, err := config.Load(resolvedCfgPath); err == nil && !config.IsLoopbackAddr(cfg.APIAddr) {
			if err := winservice.AddFirewallRule(portOf(cfg.APIAddr)); err != nil {
				fmt.Fprintln(os.Stderr, "multirtmp install: warning: failed to add firewall rule:", err)
			} else {
				fmt.Println("firewall rule added for port", portOf(cfg.APIAddr), "(private networks)")
			}
		}
	}
}

func cmdUninstall() {
	if err := winservice.Uninstall(serviceName); err != nil {
		fmt.Fprintln(os.Stderr, "multirtmp uninstall:", err)
		os.Exit(1)
	}

	fmt.Println("service removed:", serviceName)

	if err := winservice.RemoveFirewallRule(); err != nil {
		fmt.Fprintln(os.Stderr, "multirtmp uninstall: warning: failed to remove firewall rule:", err)
	}
}

func cmdStart() {
	if err := winservice.StartService(serviceName); err != nil {
		fmt.Fprintln(os.Stderr, "multirtmp start:", err)
		os.Exit(1)
	}

	fmt.Println("service started:", serviceName)
}

func cmdStop() {
	if err := winservice.StopService(serviceName); err != nil {
		fmt.Fprintln(os.Stderr, "multirtmp stop:", err)
		os.Exit(1)
	}

	fmt.Println("service stopped:", serviceName)
}

func cmdStatus() {
	st, err := winservice.QueryStatus(serviceName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "multirtmp status:", err)
		os.Exit(1)
	}

	fmt.Println(st)
}
