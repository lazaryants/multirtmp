// Package ingest принимает RTMP-публикацию, разбирает FLV-теги и считает
// статистику. На этом этапе теги никуда не буферизуются и не пересылаются.
package ingest

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/yutopp/go-rtmp"

	"multirtmp/internal/buffer"
	"multirtmp/internal/notify"
)

// Server — RTMP-сервер приёма. Поддерживает ровно один активный источник:
// новый publish (с любым ключом) вытесняет предыдущий, если он ещё активен.
type Server struct {
	addr          string
	logger        *slog.Logger
	buf           *buffer.Buffer
	sourceTimeout time.Duration
	notifier      *notify.Notifier

	rtmpSrv *rtmp.Server

	mu      sync.Mutex
	current *handler
}

// NewServer создаёт RTMP-сервер, слушающий addr (например, "127.0.0.1:1935").
// Принятые теги пишутся в buf. sourceTimeout — порог watchdog'а источника:
// сколько без единого тега считается потерей источника (лог уровня Error).
// notifier может быть nil/выключен — тогда алерты просто не отправляются.
func NewServer(addr string, logger *slog.Logger, buf *buffer.Buffer, sourceTimeout time.Duration, notifier *notify.Notifier) *Server {
	return &Server{
		addr:          addr,
		logger:        logger,
		buf:           buf,
		sourceTimeout: sourceTimeout,
		notifier:      notifier,
	}
}

// ListenAndServe блокируется, принимая соединения, пока не будет вызван Close
// либо не произойдёт ошибка приёма. При штатной остановке возвращает nil.
func (s *Server) ListenAndServe() error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("ingest: failed to listen on %s: %w", s.addr, err)
	}

	s.rtmpSrv = rtmp.NewServer(&rtmp.ServerConfig{
		OnConnect: func(conn net.Conn) (io.ReadWriteCloser, *rtmp.ConnConfig) {
			h := newHandler(s, conn.RemoteAddr().String(), s.logger, s.buf, s.sourceTimeout, s.notifier)

			return conn, &rtmp.ConnConfig{
				Handler: h,
				ControlState: rtmp.StreamControlStateConfig{
					DefaultBandwidthWindowSize: 6 * 1024 * 1024 / 8,
				},
			}
		},
	})

	s.logger.Info("ingest: listening for RTMP", "addr", s.addr)

	if err := s.rtmpSrv.Serve(listener); err != nil {
		if errors.Is(err, rtmp.ErrClosed) {
			return nil
		}
		return fmt.Errorf("ingest: serve error: %w", err)
	}

	return nil
}

// Close останавливает приём новых соединений и разблокирует ListenAndServe.
func (s *Server) Close() error {
	if s.rtmpSrv == nil {
		return nil
	}

	if err := s.rtmpSrv.Close(); err != nil {
		return fmt.Errorf("ingest: failed to stop server: %w", err)
	}

	return nil
}

// takeOver регистрирует h как текущего активного источника и закрывает
// соединение предыдущего, если он ещё не отключился сам. Один активный
// источник — жёсткое правило: второй publish вытесняет первый вне
// зависимости от ключа потока.
func (s *Server) takeOver(h *handler) {
	s.mu.Lock()
	prev := s.current
	s.current = h
	s.mu.Unlock()

	if prev != nil && prev != h {
		s.logger.Warn("ingest: new publisher preempts previous one",
			"remote", h.remoteAddr,
			"previous_remote", prev.remoteAddr,
		)
		prev.closeConn()
	}
}

// clearCurrent убирает h из текущего активного источника, если он им ещё
// является. Вызывается при отключении соединения.
func (s *Server) clearCurrent(h *handler) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.current == h {
		s.current = nil
	}
}

// SourceStatus — снимок состояния источника для GET /status.
type SourceStatus struct {
	Connected            bool
	RemoteAddr           string
	App                  string
	StreamKey            string
	VideoKbps            int64
	AudioKbps            int64
	FPS                  float64
	KeyframeIntervalS    float64
	HaveKeyframeInterval bool
	WatchdogLost         bool
	WatchdogGapS         float64
}

// SourceStatus возвращает текущее состояние источника. Можно звать из любой
// горутины: h.stats/h.watchdog/h.app/h.streamKey были выставлены до
// takeOver(h), которая берёт тот же s.mu, что и здесь, — этого достаточно
// для видимости по happens-before, без дополнительной синхронизации в handler.
func (s *Server) SourceStatus() SourceStatus {
	s.mu.Lock()
	h := s.current
	s.mu.Unlock()

	if h == nil {
		return SourceStatus{}
	}

	st := SourceStatus{
		Connected:  true,
		RemoteAddr: h.remoteAddr,
		App:        h.app,
		StreamKey:  h.streamKey,
	}

	if h.stats != nil {
		snap := h.stats.Snapshot()
		st.VideoKbps = snap.VideoKbps
		st.AudioKbps = snap.AudioKbps
		st.FPS = snap.FPS
		st.KeyframeIntervalS = snap.KeyframeIntervalS
		st.HaveKeyframeInterval = snap.HaveKeyframeInterval
	}

	if h.watchdog != nil {
		st.WatchdogLost, st.WatchdogGapS = h.watchdog.Status(h.sourceTimeout)
	}

	return st
}
