package output

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"multirtmp/internal/notify"
)

// State — текущее состояние выхода, как оно отдаётся в GET /status.
type State string

const (
	StateStopped State = "stopped"
	// StateStarting/StateStopping — не про сам Worker (он ничего о них не
	// знает), а про Registry: пока выполняется операция Start/Stop/Replace/
	// Remove над этим выходом, Registry.Snapshot отдаёт именно их, перекрывая
	// то, что сообщает Worker.Status(). Нужны оператору как подтверждение
	// "клик дошёл до сервера и обрабатывается" — без них waiting_for_source
	// неотличимо от "никто ничего не нажимал".
	StateStarting         State = "starting"
	StateStopping         State = "stopping"
	StateWaitingForSource State = "waiting_for_source"
	StateConnecting       State = "connecting"
	StateStreaming        State = "streaming"
	// StateNotConfigured — у выхода нет url или ключа (см.
	// config.ResolvedOutput.Configured); воркера нет, Start недоступен.
	StateNotConfigured State = "not_configured"
)

// reconnectRateWindow/reconnectRateThreshold — порог алерта "шторм реконнектов".
const (
	reconnectRateWindow    = 60 * time.Second
	reconnectRateThreshold = 5
)

// lastErrorClearAfter — сколько стрим должен непрерывно продержаться в
// StateStreaming, прежде чем last_error сотрётся сам. Без этого last_error
// от давно отжившего сбоя (например, разовый обрыв назначения) висел бы в
// /status и в интерфейсе бесконечно — оператор видел бы красный текст об
// ошибке, которой уже час как нет, хотя выход стабильно стримит. Счётчик
// reconnects при этом НЕ сбрасывается — это накопительная метрика за весь
// жизненный цикл выхода, отдельно от того, актуальна ли последняя ошибка
// прямо сейчас.
const lastErrorClearAfter = 60 * time.Second

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

	down           bool        // true, если последняя попытка подключения провалилась и мы ещё не восстановились
	recentFailures []time.Time // метки отказов за последние reconnectRateWindow — для алерта "шторм реконнектов"

	everStreamed bool // true после первого же успешного входа в StateStreaming, дальше не сбрасывается
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

// setState меняет состояние. Переход в StateStreaming после того, как выход
// был отмечен как "down", шлёт алерт восстановления и снимает флаг — это
// единственный переход, порождающий уведомление (обычный первый коннект при
// старте воркера — down изначально false, алерт не шлётся).
func (s *status) setState(st State, notifier *notify.Notifier) {
	s.mu.Lock()
	s.state = st
	if st == StateStreaming {
		s.streamStartedAt = time.Now()
		s.everStreamed = true
		wasDown := s.down
		s.down = false
		name := s.name
		s.mu.Unlock()

		if wasDown {
			notifier.Send("output_up:"+name, fmt.Sprintf("Output %q is back up", name))
		}
		return
	}

	s.streamStartedAt = time.Time{}
	s.mu.Unlock()
}

// recordFailure отмечает провалившуюся попытку подключения/отправки — не
// republish источника (для него см. recordReset). Шлёт алерт "выход
// отвалился" по фронту (не на каждой повторной неудачной попытке подряд) и
// отдельный алерт, если реконнектов за последнюю минуту больше
// reconnectRateThreshold.
func (s *status) recordFailure(err error, notifier *notify.Notifier) {
	s.mu.Lock()
	s.reconnects++
	if err != nil {
		s.lastError = err.Error()
	}

	now := time.Now()
	s.recentFailures = append(s.recentFailures, now)
	cutoff := now.Add(-reconnectRateWindow)
	i := 0
	for i < len(s.recentFailures) && s.recentFailures[i].Before(cutoff) {
		i++
	}
	s.recentFailures = s.recentFailures[i:]
	rate := len(s.recentFailures)

	wasDown := s.down
	s.down = true
	name := s.name
	s.mu.Unlock()

	if !wasDown {
		notifier.Send("output_down:"+name, fmt.Sprintf("Output %q is down: %v", name, err))
	}
	if rate > reconnectRateThreshold {
		notifier.Send("output_flapping:"+name,
			fmt.Sprintf("Output %q reconnected %d times in the last minute", name, rate))
	}
}

// recordReset отмечает реконнект, вызванный новым publish источника (см.
// buffer.ErrReset) — это не отказ назначения, поэтому алерт "выход отвалился"
// не шлётся, только счётчики/last_error для наблюдаемости.
func (s *status) recordReset(err error) {
	s.mu.Lock()
	s.reconnects++
	if err != nil {
		s.lastError = err.Error()
	}
	s.mu.Unlock()
}

// hasEverStreamed сообщает, доходил ли этот выход хоть раз до StateStreaming
// за весь свой жизненный цикл — используется для эскалации потолка backoff
// (см. Worker.Run): выход, который ни разу не подключился, скорее всего
// настроен неверно (url/key), и не должен долбить назначение раз в 5с
// бесконечно.
func (s *status) hasEverStreamed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.everStreamed
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
// Заодно проверяет lag_s против delay+30s — алерт, если превышен.
func (s *status) run(ctx context.Context, logger *slog.Logger, interval time.Duration, notifier *notify.Notifier, delay time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("output: panic in stats goroutine", "panic", r)
		}
	}()

	lagAlertThreshold := delay + catchUpGrace

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
			if s.state == StateStreaming && !s.streamStartedAt.IsZero() &&
				s.lastError != "" && time.Since(s.streamStartedAt) >= lastErrorClearAfter {
				s.lastError = ""
			}
			name := s.name
			s.mu.Unlock()

			if haveLag && time.Duration(lagS*float64(time.Second)) > lagAlertThreshold {
				notifier.Send("output_lag:"+name,
					fmt.Sprintf("Output %q lag is %.1fs (delay %s + 30s grace exceeded)", name, lagS, delay))
			}

			attrs := []any{"kbps", kbps, "tags_sent", tagsSent}
			if haveLag {
				attrs = append(attrs, "lag_s", lagS)
			}
			logger.Debug("output: stats", attrs...)
		}
	}
}
