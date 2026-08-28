package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"multirtmp/internal/buildinfo"
	"multirtmp/internal/config"
	"multirtmp/internal/winservice"
)

func main() {
	if runServiceSubcommand() {
		return
	}

	configPath := flag.String("config", "", "path to config.json (default: next to the executable)")
	debug := flag.Bool("debug", false, "force debug-level logs regardless of log.level in config")
	version := flag.Bool("version", false, "print version and exit")

	addr := flag.String("addr", config.DefaultIngestAddr, "override ingest.addr from config")
	bufSize := sizeFlag(config.BufferMinSize)
	flag.Var(&bufSize, "buffer", "override buffer.size from config (disables autosize), accepts K/M/G or KB/MB/GB suffixes")
	sourceTimeout := flag.Duration("source-timeout", config.DefaultSourceTimeout, "override ingest.source_timeout from config")
	apiAddr := flag.String("api-addr", config.DefaultAPIAddr, "override api.addr from config")

	var outs outputSpecs
	flag.Var(&outs, "out", `override the entire outputs list from config, repeatable, "url|delay" (debug only)`)

	flag.Parse()

	if *version {
		fmt.Printf("multirtmp %s (%s)\n", buildinfo.Version, buildinfo.Commit)
		return
	}

	cfgPath, err := resolveConfigPath(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "multirtmp: failed to resolve config path:", err)
		os.Exit(1)
	}

	isService, err := winservice.IsWindowsService()
	if err != nil {
		fmt.Fprintln(os.Stderr, "multirtmp: failed to determine if running as a service:", err)
		os.Exit(1)
	}

	logger, levelVar, logCloser, err := newLogger(filepath.Dir(mustExecutable()), isService, slog.LevelInfo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "multirtmp: failed to open log file:", err)
		os.Exit(1)
	}
	defer logCloser.Close()

	opts := appOptions{
		cfgPath:               cfgPath,
		overrideAddr:          addr,
		overrideBufSize:       &bufSize,
		overrideSourceTimeout: sourceTimeout,
		overrideAPIAddr:       apiAddr,
		overrideDebug:         debug,
		overrideOuts:          outs,
	}

	err = winservice.Run(serviceName, func(ctx context.Context) error {
		return runApp(ctx, logger, levelVar, opts)
	})
	if err != nil {
		logger.Error("main: exited with error", "error", err)
		os.Exit(1)
	}
}

// resolveConfigPath возвращает explicit, если он задан (-config), иначе
// config.json рядом с исполняемым файлом.
func resolveConfigPath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	return filepath.Join(filepath.Dir(mustExecutable()), "config.json"), nil
}

func mustExecutable() string {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "multirtmp: failed to determine executable path:", err)
		os.Exit(1)
	}
	return exe
}
