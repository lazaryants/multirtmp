package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"multirtmp/internal/api"
	"multirtmp/internal/buffer"
	"multirtmp/internal/config"
	"multirtmp/internal/ingest"
	"multirtmp/internal/notify"
	"multirtmp/internal/output"
	"multirtmp/internal/winservice"
)

const bufferStatsInterval = 2 * time.Second

// appOptions — CLI-флаги обычного запуска (не подкоманды службы), которые
// могут переопределять config.json. Указатели — потому что нужно различать
// "флаг не передан" (flag.Visit) от "передан со значением по умолчанию".
type appOptions struct {
	cfgPath string

	overrideAddr          *string
	overrideBufSize       *sizeFlag
	overrideSourceTimeout *time.Duration
	overrideAPIAddr       *string
	overrideDebug         *bool
	overrideOuts          outputSpecs
}

// runApp — единая точка входа приложения и для обычного процесса, и для
// Windows-службы (см. winservice.Run — вызывается как RunFunc). НИКОГДА не
// вызывает os.Exit сама: под службой это обошло бы протокол остановки SCM
// (см. internal/winservice/handler.go) и выглядело бы как крэш вместо чистой
// остановки. Ошибка "конфига нет/он невалиден" оборачивается в
// winservice.PermanentError — сигнал не перезапускать процесс в цикле на
// опечатке в config.json.
func runApp(ctx context.Context, logger *slog.Logger, levelVar *slog.LevelVar, opts appOptions) error {
	if _, statErr := os.Stat(opts.cfgPath); os.IsNotExist(statErr) {
		if err := config.WriteSample(opts.cfgPath); err != nil {
			return &winservice.PermanentError{Err: fmt.Errorf("main: failed to create sample config: %w", err)}
		}
		logger.Info("main: no config found, created a sample — edit it and run again", "path", opts.cfgPath)
		return &winservice.PermanentError{Err: fmt.Errorf("no config at %s, sample created", opts.cfgPath)}
	}

	cfg, err := config.Load(opts.cfgPath)
	if err != nil {
		logger.Error("main: invalid config", "error", err)
		return &winservice.PermanentError{Err: err}
	}

	applyFlagOverrides(cfg, opts)
	levelVar.Set(cfg.LogLevel)

	logger.Info("main: config loaded",
		"path", opts.cfgPath,
		"ingest_addr", cfg.IngestAddr,
		"api_addr", cfg.APIAddr,
		"buffer_size", cfg.BufferSize,
		"buffer_auto", cfg.BufferAuto,
		"log_level", cfg.LogLevel.String(),
		"notify_enabled", cfg.Notify.BotToken != "",
		"outputs", len(cfg.Outputs),
	)

	notifier := notify.New(cfg.Notify, logger)

	buf := buffer.New(cfg.BufferSize)
	ingestSrv := ingest.NewServer(cfg.IngestAddr, logger, buf, cfg.SourceTimeout, notifier)

	// Свой дочерний ctx: если один из серверов ниже упадёт сам (не по внешней
	// отмене ctx), нужно остановить и всё остальное (выходы, статистику
	// буфера) — а внешний stop() нам отсюда не виден (он у вызывающего:
	// winservice.Run для foreground, handler.Execute для службы).
	appCtx, cancelApp := context.WithCancel(ctx)
	defer cancelApp()

	go buf.LogStats(appCtx, logger, bufferStatsInterval)

	if cfg.BufferAuto {
		go runBufferAutosize(appCtx, buf, ingestSrv, logger, cfg.MaxOutputDelay, cfg.BufferMaxSize)
	}

	registry := output.NewRegistry(appCtx)
	var toStart []string
	for _, o := range cfg.Outputs {
		w := output.NewWorker(o.Name, o.URL, o.Delay, buf, logger, notifier)
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
	case <-appCtx.Done():
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

	cancelApp() // остановить выходы/автоподбор буфера, даже если попали сюда не через appCtx.Done()

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

	return nil
}

// applyFlagOverrides накладывает явно переданные CLI-флаги поверх
// загруженного конфига — только те, что реально были указаны (flag.Visit),
// а не те, что просто совпали со значением по умолчанию.
func applyFlagOverrides(cfg *config.Resolved, opts appOptions) {
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "addr":
			cfg.IngestAddr = *opts.overrideAddr
		case "buffer":
			cfg.BufferSize = int64(*opts.overrideBufSize)
			cfg.BufferAuto = false
		case "source-timeout":
			cfg.SourceTimeout = *opts.overrideSourceTimeout
		case "api-addr":
			cfg.APIAddr = *opts.overrideAPIAddr
		case "debug":
			if *opts.overrideDebug {
				cfg.LogLevel = slog.LevelDebug
			} else {
				cfg.LogLevel = slog.LevelInfo
			}
		}
	})

	if len(opts.overrideOuts) > 0 {
		cfg.Outputs = make([]config.ResolvedOutput, 0, len(opts.overrideOuts))
		cfg.MaxOutputDelay = 0
		for i, o := range opts.overrideOuts {
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
