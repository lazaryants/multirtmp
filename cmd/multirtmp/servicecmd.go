package main

import (
	"flag"
	"fmt"
	"os"

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
}

func cmdUninstall() {
	if err := winservice.Uninstall(serviceName); err != nil {
		fmt.Fprintln(os.Stderr, "multirtmp uninstall:", err)
		os.Exit(1)
	}

	fmt.Println("service removed:", serviceName)
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
