package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"multirtmp/internal/api"
	"multirtmp/internal/buffer"
	"multirtmp/internal/config"
	"multirtmp/internal/ingest"
	"multirtmp/internal/output"
)

const bufferStatsInterval = 2 * time.Second

func main() {
	configPath := flag.String("config", "", "path to config.json (default: next to the executable)")
	debug := flag.Bool("debug", true, "print debug logs (including periodic stats every 2s)")

	addr := flag.String("addr", config.DefaultIngestAddr, "override ingest.addr from config")
	bufSize := sizeFlag(config.BufferMinSize)
	flag.Var(&bufSize, "buffer", "override buffer.size from config (disables autosize), accepts K/M/G or KB/MB/GB suffixes")
	sourceTimeout := flag.Duration("source-timeout", config.DefaultSourceTimeout, "override ingest.source_timeout from config")
	apiAddr := flag.String("api-addr", config.DefaultAPIAddr, "override api.addr from config")

	var outs outputSpecs
	flag.Var(&outs, "out", `override the entire outputs list from config, repeatable, "url|delay" (debug only)`)

	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	cfgPath, err := resolveConfigPath(*configPath)
	if err != nil {
		logger.Error("main: failed to resolve config path", "error", err)
		os.Exit(1)
	}

	if _, statErr := os.Stat(cfgPath); os.IsNotExist(statErr) {
		if err := config.WriteSample(cfgPath); err != nil {
			logger.Error("main: failed to create sample config", "error", err)
			os.Exit(1)
		}
		logger.Info("main: no config found, created a sample — edit it and run again", "path", cfgPath)
		os.Exit(1)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		logger.Error("main: invalid config", "error", err)
		os.Exit(1)
	}

	applyFlagOverrides(cfg, addr, &bufSize, sourceTimeout, apiAddr, outs)

	logger.Info("main: config loaded",
		"path", cfgPath,
		"ingest_addr", cfg.IngestAddr,
		"api_addr", cfg.APIAddr,
		"buffer_size", cfg.BufferSize,
		"buffer_auto", cfg.BufferAuto,
		"outputs", len(cfg.Outputs),
	)

	buf := buffer.New(cfg.BufferSize)
	ingestSrv := ingest.NewServer(cfg.IngestAddr, logger, buf, cfg.SourceTimeout)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	go buf.LogStats(ctx, logger, bufferStatsInterval)

	if cfg.BufferAuto {
		go runBufferAutosize(ctx, buf, ingestSrv, logger, cfg.MaxOutputDelay, cfg.BufferMaxSize)
	}

	registry := output.NewRegistry(ctx)
	var toStart []string
	for _, o := range cfg.Outputs {
		w := output.NewWorker(o.Name, o.URL, o.Delay, buf, logger)
		registry.Register(o.Name, w)
		if o.Enabled {
			toStart = append(toStart, o.Name)
		}
	}
	registry.StartAll(toStart)

	apiSrv := api.NewServer(cfg.APIAddr, logger, ingestSrv, buf, registry)

	ingestErrCh := make(chan error, 1)
	go func() { ingestErrCh <- ingestSrv.ListenAndServe() }()

	apiErrCh := make(chan error, 1)
	go func() { apiErrCh <- apiSrv.ListenAndServe() }()

	var ingestDone, apiDone bool

	select {
	case <-ctx.Done():
		logger.Info("main: received stop signal")

	case err := <-ingestErrCh:
		ingestDone = true
		if err != nil {
			logger.Error("main: ingest server exited with error", "error", err)
		}

	case err := <-apiErrCh:
		apiDone = true
		if err != nil {
			logger.Error("main: api server exited with error", "error", err)
		}
	}

	stop() // на случай если сюда попали не через ctx.Done — останавливаем и выходы

	if err := ingestSrv.Close(); err != nil {
		logger.Error("main: error stopping ingest server", "error", err)
	}
	if err := apiSrv.Close(); err != nil {
		logger.Error("main: error stopping api server", "error", err)
	}

	if !ingestDone {
		<-ingestErrCh
	}
	if !apiDone {
		<-apiErrCh
	}

	registry.Wait()
}

// resolveConfigPath возвращает explicit, если он задан (-config), иначе
// config.json рядом с исполняемым файлом.
func resolveConfigPath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}

	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("failed to determine executable path: %w", err)
	}

	return filepath.Join(filepath.Dir(exe), "config.json"), nil
}

// applyFlagOverrides накладывает явно переданные CLI-флаги поверх
// загруженного конфига — только те, что реально были указаны (flag.Visit),
// а не те, что просто совпали со значением по умолчанию.
func applyFlagOverrides(cfg *config.Resolved, addr *string, bufSize *sizeFlag, sourceTimeout *time.Duration, apiAddr *string, outs outputSpecs) {
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "addr":
			cfg.IngestAddr = *addr
		case "buffer":
			cfg.BufferSize = int64(*bufSize)
			cfg.BufferAuto = false
		case "source-timeout":
			cfg.SourceTimeout = *sourceTimeout
		case "api-addr":
			cfg.APIAddr = *apiAddr
		}
	})

	if len(outs) > 0 {
		cfg.Outputs = make([]config.ResolvedOutput, 0, len(outs))
		cfg.MaxOutputDelay = 0
		for i, o := range outs {
			cfg.Outputs = append(cfg.Outputs, config.ResolvedOutput{
				Name:    fmt.Sprintf("out%d", i),
				URL:     o.URL,
				Delay:   o.Delay,
				Enabled: true,
			})
			if o.Delay > cfg.MaxOutputDelay {
				cfg.MaxOutputDelay = o.Delay
			}
		}
	}
}
