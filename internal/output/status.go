package output

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// State — текущее состояние выхода, как оно отдаётся в GET /status.
type State string

const (
	StateStopped          State = "stopped"
	StateWaitingForSource State = "waiting_for_source"
	StateConnecting       State = "connecting"
	StateStreaming        State = "streaming"
)

// Status — снимок состояния одного выхода.
type Status struct {
	Name       string  `json:"name"`
	State      State   `json:"state"`
	KBPS       int64   `json:"kbps"`
	TagsSent   int64   `json:"tags_sent"`
	LagS       float64 `json:"lag_s"`
	Reconnects int64   `json:"reconnects"`
	LastError  string  `json:"last_error"`
	UptimeS    float64 `json:"uptime_s"`
}

// status копит живое состояние одного выхода на весь его жизненный цикл (не
// пересоздаётся при реконнекте) — то же самое видно и в периодическом
// debug-логе, и по запросу через GET /status. Счётчики на горячем пути
// (record, вызывается на каждый отправленный тег) — атомики; редкие
// переходы (смена состояния, ошибка, реконнект) — под мьютексом.
type status struct {
	name string

	windowBytes    atomic.Int64
	windowLagSumNS atomic.Int64
	windowLagCount atomic.Int64
	tagsSentTotal  atomic.Int64

	mu              sync.Mutex
	state           State
	lastKbps        int64
	lastLagS        float64
	reconnects      int64
	lastError       string
	streamStartedAt time.Time // zero, если state != StateStreaming
}

func newStatus(name string) *status {
	return &status{name: name, state: StateWaitingForSource}
}

// record регистрирует отправленный тег. lag — на сколько отправленный тег
// отстаёт по времени прихода от текущего момента.
func (s *status) record(n int, lag time.Duration) {
	s.windowBytes.Add(int64(n))
	s.tagsSentTotal.Add(1)
	s.windowLagSumNS.Add(int64(lag))
	s.windowLagCount.Add(1)
}

func (s *status) setState(st State) {
	s.mu.Lock()
	s.state = st
	if st == StateStreaming {
		s.streamStartedAt = time.Now()
	} else {
		s.streamStartedAt = time.Time{}
	}
	s.mu.Unlock()
}

// recordReconnect отмечает, что соединение оборвалось и потребуется новая
// попытка — увеличивает счётчик и запоминает причину для last_error.
func (s *status) recordReconnect(err error) {
	s.mu.Lock()
	s.reconnects++
	if err != nil {
		s.lastError = err.Error()
	}
	s.mu.Unlock()
}

// Snapshot возвращает текущее состояние для GET /status.
func (s *status) Snapshot() Status {
	s.mu.Lock()
	defer s.mu.Unlock()

	var uptime float64
	if s.state == StateStreaming && !s.streamStartedAt.IsZero() {
		uptime = time.Since(s.streamStartedAt).Seconds()
	}

	return Status{
		Name:       s.name,
		State:      s.state,
		KBPS:       s.lastKbps,
		TagsSent:   s.tagsSentTotal.Load(),
		LagS:       s.lastLagS,
		Reconnects: s.reconnects,
		LastError:  s.lastError,
		UptimeS:    round(uptime, 3),
	}
}

func round(x float64, decimals int) float64 {
	p := math.Pow(10, float64(decimals))
	return math.Round(x*p) / p
}

// run печатает сводную статистику раз в interval, пока не отменён ctx, и
// заодно кеширует последние kbps/lag_s для Snapshot. Живёт весь жизненный
// цикл воркера (запускается один раз в Run), не пересоздаётся при реконнектах.
func (s *status) run(ctx context.Context, logger *slog.Logger, interval time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("output: panic in stats goroutine", "panic", r)
		}
	}()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			bytesSent := s.windowBytes.Swap(0)
			lagSum := s.windowLagSumNS.Swap(0)
			lagCount := s.windowLagCount.Swap(0)
			tagsSent := s.tagsSentTotal.Load()

			secs := interval.Seconds()
			kbps := int64(math.Round(float64(bytesSent) * 8 / 1000 / secs))

			var lagS float64
			haveLag := lagCount > 0
			if haveLag {
				lagS = round(time.Duration(lagSum/lagCount).Seconds(), 3)
			}

			s.mu.Lock()
			s.lastKbps = kbps
			if haveLag {
				s.lastLagS = lagS
			}
			s.mu.Unlock()

			attrs := []any{"kbps", kbps, "tags_sent", tagsSent}
			if haveLag {
				attrs = append(attrs, "lag_s", lagS)
			}
			logger.Debug("output: stats", attrs...)
		}
	}
}
