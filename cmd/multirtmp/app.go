package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"multirtmp/internal/api"
	"multirtmp/internal/buffer"
	"multirtmp/internal/config"
	"multirtmp/internal/ingest"
	"multirtmp/internal/mediamtx"
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
	// isService — true, если процесс запущен через SCM (svc.Run), не
	// напрямую с консоли. Используется только для решения, стоит ли в
	// подсказке "нет конфига" упоминать install: под уже установленной
	// службой это упоминание было бы бессмысленным (она и так установлена).
	isService bool

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
	// До любого запуска дочерних процессов: всё, что runApp запустит дальше
	// (MediaMTX), умрёт вместе с этим процессом при любом его завершении,
	// включая аварийное. Не фатально — см. mediamtx.BindChildrenToProcess.
	if err := mediamtx.BindChildrenToProcess(); err != nil {
		logger.Warn("main: could not bind child processes to this one; falling back to per-child binding", "error", err)
	}

	if _, statErr := os.Stat(opts.cfgPath); os.IsNotExist(statErr) {
		if err := config.WriteSample(opts.cfgPath); err != nil {
			return &winservice.PermanentError{Err: fmt.Errorf("main: failed to create sample config: %w", err)}
		}
		logger.Info("main: no config found, created a sample — edit it and run again", "path", opts.cfgPath)
		if !opts.isService {
			logger.Info("main: to run multirtmp in the background as a Windows service, use: multirtmp.exe install")
		}
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

	registry := output.NewRegistry(appCtx)
	var toStart []string
	for _, o := range cfg.Outputs {
		if !o.Configured {
			// Нет url или ключа — регистрируем без воркера: виден в
			// интерфейсе как NOT CONFIGURED, никуда не подключается.
			registry.Register(o.Name, nil)
			logger.Info("main: output is not configured yet, enter its server URL and stream key in the web UI", "name", o.Name)
			continue
		}
		w := output.NewWorker(o.Name, o.URL, o.Delay, buf, logger, notifier)
		registry.Register(o.Name, w)
		if o.Enabled {
			toStart = append(toStart, o.Name)
		}
	}
	registry.StartAll(toStart)

	// EnsureBackup — разовая копия config.json "как настроено руками", перед
	// тем как раздел Destinations в веб-интерфейсе начнёт его переписывать
	// (и вместе с этим — терять `//`-комментарии, см. internal/config/doc.go).
	// Не фатально, если не получилось: сам конфиг уже успешно загружен выше.
	// Обязательно ДО того, как что-либо получает возможность вызвать
	// api.NewServer/outputsStore — первая же запись через API не должна
	// оказаться раньше первого бэкапа.
	if err := config.EnsureBackup(opts.cfgPath); err != nil {
		logger.Warn("main: failed to back up config.json", "error", err)
	}

	lanURLs, err := detectLANURLs(cfg.APIAddr, cfg.APIToken, cfg.APIPublicHost)
	if err != nil {
		logger.Warn("main: could not detect LAN addresses for the tablet link, set api.public_host", "error", err)
	}
	for _, u := range lanURLs {
		logger.Info("main: web UI reachable from the LAN", "url", u)
	}

	// Правила брандмауэра проверяются на каждом старте (не только при install):
	// доступ по LAN или превью могли включить позже. Чего не хватает — Warn в
	// лог и одна готовая строка PowerShell в баннер интерфейса.
	var missingRules []string
	checkRule := func(name, protocol, port, what string) {
		if ok, err := winservice.FirewallRuleExists(name); err != nil {
			logger.Warn("main: failed to check Windows Firewall rule", "rule", name, "error", err)
		} else if !ok {
			cmd := winservice.FirewallRuleCommand(name, protocol, port)
			missingRules = append(missingRules, cmd)
			logger.Warn("main: Windows Firewall rule not found, "+what+" from the LAN is likely blocked",
				"rule", name, "port", port+"/"+protocol, "create_with", cmd)
		}
	}
	if !config.IsLoopbackAddr(cfg.APIAddr) {
		checkRule(winservice.APIFirewallRule, "TCP", portOf(cfg.APIAddr), "the web interface")
		if cfg.PreviewEnabled {
			checkRule(winservice.PreviewFirewallRule, "UDP", previewUDPPort, "the preview")
		}
	}
	firewallHint := strings.Join(missingRules, "; ")

	// Примечание про -out (debug-флаг, см. flags.go): он подменяет выходы
	// мимо config.json, но outputsStore теперь всегда читает выходы с диска
	// заново перед каждой операцией (см. internal/api/outputs_store.go —
	// это то, что и чинит текущий баг с потерей секций конфига) и понятия не
	// имеет о -out. При активном -out буфер-автоподбор и GET /outputs будут
	// смотреть на реальный файл, а не на то, что подменил -out — известный,
	// осознанно принятый нюанс debug-режима, не для продакшна.
	//
	// Ссылки для планшета в интерфейсе вычисляются заново на каждый клик
	// "Show link for tablet" (адрес машины меняется через DHCP); строка в
	// логе выше — снимок на момент старта.
	// Плеер превью — через прокси multirtmp (/preview/...), а не напрямую с
	// MediaMTX: тот же хост, что и у страницы, и тот же токен.
	previewInfo := api.PreviewInfo{Enabled: cfg.PreviewEnabled}
	if cfg.PreviewEnabled {
		previewInfo.URL = "/preview/" + cfg.PreviewPath + "/"
		previewInfo.Target = cfg.PreviewBaseURL
	}
	var mtx *mediamtx.Supervisor
	if cfg.PreviewEnabled {
		mtx = newMediaMTXSupervisor(cfg.PreviewMediaMTXExe, cfg.PreviewBaseURL, logger)
		previewInfo.MediaMTX = mtx.Status
	}

	network := api.NetworkInfo{
		LANAccess: !config.IsLoopbackAddr(cfg.APIAddr),
		LANURLs: func() ([]string, error) {
			return detectLANURLs(cfg.APIAddr, cfg.APIToken, cfg.APIPublicHost)
		},
		FirewallHint: firewallHint,
	}
	apiSrv, err := api.NewServer(cfg.APIAddr, cfg.APIToken, network,
		previewInfo,
		logger, ingestSrv, buf, registry, opts.cfgPath, notifier)
	if err != nil {
		return fmt.Errorf("main: failed to start api server: %w", err)
	}

	if cfg.BufferAuto {
		go runBufferAutosize(appCtx, buf, ingestSrv, logger, apiSrv.MaxOutputDelay, cfg.BufferMaxSize)
	}

	mtxDone := make(chan struct{})
	if mtx != nil {
		go func() {
			defer close(mtxDone)
			mtx.Run(appCtx)
		}()
	} else {
		close(mtxDone)
	}

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
	<-mtxDone

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
		for i, o := range opts.overrideOuts {
			cfg.Outputs = append(cfg.Outputs, config.ResolvedOutput{
				Name:       fmt.Sprintf("out%d", i),
				URL:        o.URL,
				Delay:      o.Delay,
				Enabled:    true,
				Configured: true,
			})
		}
	}
}
