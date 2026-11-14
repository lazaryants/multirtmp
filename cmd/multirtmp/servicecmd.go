package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"multirtmp/internal/config"
	"multirtmp/internal/winservice"
)

// startVerifyDelay — сколько ждать после запроса на старт службы, прежде чем
// проверять, что она реально поднялась. StartService лишь просит SCM
// запустить процесс и возвращается сразу — если конфиг невалиден или
// произошла любая другая PermanentError-ошибка, процесс успевает выйти почти
// мгновенно (см. internal/winservice/handler.go), и без этой паузы "started"
// напечатается для службы, которая уже упала.
const startVerifyDelay = 3 * time.Second

const (
	serviceName        = "multirtmp"
	serviceDisplayName = "multirtmp RTMP relay"
	serviceDescription = "Relays an RTMP publish (vMix) to multiple RTMP destinations, each with its own delay."
)

// runServiceSubcommand обрабатывает install/uninstall/start/stop/status/open,
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
	case "open":
		cmdOpen(os.Args[2:])
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

	addDesktopShortcut(exePath, *cfgPath)
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

	if err := winservice.RemoveDesktopShortcut(); err != nil {
		fmt.Fprintln(os.Stderr, "multirtmp uninstall: warning:", err)
	}
}

func cmdStart() {
	if err := winservice.StartService(serviceName); err != nil {
		fmt.Fprintln(os.Stderr, "multirtmp start:", err)
		os.Exit(1)
	}

	time.Sleep(startVerifyDelay)

	st, err := winservice.QueryStatus(serviceName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "multirtmp start: failed to verify status:", err)
		os.Exit(1)
	}

	if st != "running" {
		fmt.Fprintf(os.Stderr, "multirtmp start: service did not start (state: %s)\n", st)
		if line, ok := lastErrorLogLine(); ok {
			fmt.Fprintln(os.Stderr, "last error in multirtmp.log:", line)
		} else {
			fmt.Fprintln(os.Stderr, "no ERROR line found in multirtmp.log — check Windows Event Viewer")
		}
		os.Exit(1)
	}

	fmt.Println("service started:", serviceName)
}

// lastErrorLogLine возвращает последнюю строку уровня ERROR из
// multirtmp.log (рядом с exe — см. cmd/multirtmp/logging.go), если такая
// есть. Используется cmdStart, чтобы оператору не приходилось лезть в лог
// руками при неудачном старте — самая частая причина (невалидный
// config.json) уже есть текстом в этой самой строке.
func lastErrorLogLine() (string, bool) {
	logPath := filepath.Join(filepath.Dir(mustExecutable()), "multirtmp.log")

	f, err := os.Open(logPath)
	if err != nil {
		return "", false
	}
	defer f.Close()

	var last string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "level=ERROR") {
			last = line
		}
	}

	return last, last != ""
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

// addDesktopShortcut — best-effort, как и правило firewall: служба уже
// установлена, отсутствие ярлыка — не повод считать install неудачным.
// Ярлык запускает `multirtmp.exe open`; если install вызван с -config, тот же
// путь (абсолютный — у ярлыка своя рабочая папка) передаётся и в open.
func addDesktopShortcut(exePath, cfgPath string) {
	args := []string{"open"}
	if cfgPath != "" {
		abs, err := filepath.Abs(cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "multirtmp install: warning: desktop shortcut not created: failed to resolve config path:", err)
			return
		}
		args = append(args, "-config", abs)
	}

	lnk, err := winservice.CreateDesktopShortcut(exePath, args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "multirtmp install: warning:", err)
		return
	}
	fmt.Println("desktop shortcut created:", lnk)
}
