// Package api — HTTP API статуса и управления выходами. Слушает только
// 127.0.0.1, без аутентификации (см. CLAUDE.md — наружу не открываем).
// Задержка выхода через API не меняется, только рестарт процесса.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"multirtmp/internal/buffer"
	"multirtmp/internal/ingest"
	"multirtmp/internal/output"
)

// Server — HTTP API поверх ingest/buffer/output.
type Server struct {
	addr   string
	logger *slog.Logger

	ingest   *ingest.Server
	buf      *buffer.Buffer
	registry *output.Registry

	httpSrv *http.Server
}

// NewServer создаёт API-сервер, слушающий addr (например, "127.0.0.1:8080").
func NewServer(addr string, logger *slog.Logger, ingestSrv *ingest.Server, buf *buffer.Buffer, registry *output.Registry) *Server {
	s := &Server{
		addr:     addr,
		logger:   logger,
		ingest:   ingestSrv,
		buf:      buf,
		registry: registry,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("POST /outputs/{name}/start", s.handleOutputStart)
	mux.HandleFunc("POST /outputs/{name}/stop", s.handleOutputStop)

	s.httpSrv = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	return s
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
