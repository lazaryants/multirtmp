// Package api — HTTP API + веб-интерфейс статуса и управления выходами. На
// 127.0.0.1 работает без аутентификации, как и раньше; при не-loopback
// api.addr (доступ с планшета по LAN) требует токен — см. NewServer.
package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"multirtmp/internal/buffer"
	"multirtmp/internal/config"
	"multirtmp/internal/ingest"
	"multirtmp/internal/notify"
	"multirtmp/internal/output"
)

// NetworkInfo — то, что Server сообщает о своей LAN-доступности (см.
// cmd/multirtmp/lanurl.go, где эти значения вычисляются один раз при
// старте). Пустые поля означают "не применимо" (loopback-режим) — фронтенд
// прячет соответствующие элементы интерфейса.
type NetworkInfo struct {
	// LANURL — полная ссылка на веб-интерфейс с токеном (для кнопки "Show
	// link for tablet"). Отдаётся только уже аутентифицированному клиенту —
	// это тот же токен, что клиент и так знает, не новая утечка.
	LANURL string
	// FirewallHint — готовая PowerShell-команда, если правило Windows
	// Firewall для текущего порта не найдено. Пусто — либо loopback, либо
	// правило на месте.
	FirewallHint string
}

// Server — HTTP API + статика поверх ingest/buffer/output.
type Server struct {
	addr    string
	token   string
	network NetworkInfo
	logger  *slog.Logger

	ingest   *ingest.Server
	buf      *buffer.Buffer
	registry *output.Registry
	outputs  *outputsStore

	httpSrv *http.Server
}

// NewServer создаёт API-сервер, слушающий addr (например, "127.0.0.1:8080").
// token — из config.Resolved.APIToken; пусто на loopback-адресах (тогда
// аутентификация выключена целиком, поведение как раньше). doc/cfgPath —
// текущий config.json для персиста правок из раздела Destinations (см.
// outputs_store.go); notifier — для алертов down/up у выходов, добавленных
// через UI, наравне с выходами из стартового конфига.
func NewServer(addr, token string, network NetworkInfo, logger *slog.Logger, ingestSrv *ingest.Server, buf *buffer.Buffer, registry *output.Registry, cfgPath string, doc *config.Doc, notifier *notify.Notifier) *Server {
	s := &Server{
		addr:     addr,
		token:    token,
		network:  network,
		logger:   logger,
		ingest:   ingestSrv,
		buf:      buf,
		registry: registry,
		outputs:  newOutputsStore(cfgPath, doc, registry, buf, logger, notifier),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("POST /outputs/{name}/start", s.handleOutputStart)
	mux.HandleFunc("POST /outputs/{name}/stop", s.handleOutputStop)
	mux.HandleFunc("GET /outputs", s.handleOutputsList)
	mux.HandleFunc("POST /outputs", s.handleOutputsCreate)
	mux.HandleFunc("PUT /outputs/{name}", s.handleOutputsUpdate)
	mux.HandleFunc("DELETE /outputs/{name}", s.handleOutputsDelete)
	mux.HandleFunc("GET /", s.handleIndex)

	s.httpSrv = &http.Server{
		Addr:    addr,
		Handler: s.requireToken(mux),
	}

	return s
}

// MaxOutputDelay — см. outputsStore.MaxOutputDelay; проброшено наружу для
// cmd/multirtmp/bufferauto.go, которому не нужно знать про outputsStore.
func (s *Server) MaxOutputDelay() time.Duration {
	return s.outputs.MaxOutputDelay()
}

// requireToken — единственная точка проверки аутентификации, оборачивает
// весь mux целиком (включая GET / — токен из query должен пускать и первую
// загрузку страницы). Если token пуст (loopback-режим), пропускает всё без
// проверки — поведение stage 5 не меняется. Токен ищется в заголовке
// Authorization: Bearer <token> (все запросы фронтенда после первой
// загрузки) и в ?token=<token> (первая загрузка страницы по ссылке).
//
// Ни здесь, ни где-либо ещё в этом пакете НЕ логируется r.URL.String()/
// RawQuery целиком — там может быть токен. Единственная строка про неудачную
// попытку логирует только r.URL.Path.
func (s *Server) requireToken(next http.Handler) http.Handler {
	if s.token == "" {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.URL.Query().Get("token")
		if tok == "" {
			if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
				tok = strings.TrimPrefix(auth, "Bearer ")
			}
		}

		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) != 1 {
			s.logger.Warn("api: rejected request with missing/invalid token", "path", r.URL.Path)
			writeError(w, http.StatusUnauthorized, errors.New("unauthorized: missing or invalid token"))
			return
		}

		next.ServeHTTP(w, r)
	})
}

// ListenAndServe блокируется, пока не будет вызван Close. При штатной
// остановке возвращает nil.
func (s *Server) ListenAndServe() error {
	s.logger.Info("api: listening for HTTP", "addr", s.addr)

	if err := s.httpSrv.ListenAndServe(); err != nil {
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("api: serve error: %w", err)
	}

	return nil
}

// Close останавливает сервер.
func (s *Server) Close() error {
	if err := s.httpSrv.Shutdown(context.Background()); err != nil {
		return fmt.Errorf("api: failed to stop server: %w", err)
	}
	return nil
}
