package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"multirtmp/internal/notify"
)

const watchdogCheckInterval = 1 * time.Second

// watchdog отслеживает, не пропал ли источник: каждый входящий тег двигает
// lastTagAt, отдельная горутина раз в watchdogCheckInterval проверяет
// разрыв. Только логирует ("потеря источника" — Error, восстановление —
// Info) и не рвёт соединение принудительно: пропажа тегов может быть
// временной заминкой у vMix, а обрывать TCP самим смысла нет — симметрично
// правилу "если YouTube встал, vMix не должен узнать".
type watchdog struct {
	lastTagAt atomic.Int64 // UnixNano
}

func newWatchdog() *watchdog {
	w := &watchdog{}
	w.touch()
	return w
}

func (w *watchdog) touch() {
	w.lastTagAt.Store(time.Now().UnixNano())
}

// Status возвращает, потерян ли источник прямо сейчас (тот же порог timeout,
// что использует run), и сколько секунд прошло с последнего тега. Можно
// звать из любой горутины в любой момент — не привязано к тику run.
func (w *watchdog) Status(timeout time.Duration) (lost bool, gapS float64) {
	gap := time.Since(time.Unix(0, w.lastTagAt.Load()))
	return gap > timeout, gap.Seconds()
}

func (w *watchdog) run(ctx context.Context, logger *slog.Logger, timeout time.Duration, notifier *notify.Notifier, streamKey string) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("ingest: panic in watchdog goroutine", "panic", r)
		}
	}()

	ticker := time.NewTicker(watchdogCheckInterval)
	defer ticker.Stop()

	lost := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			gap := time.Since(time.Unix(0, w.lastTagAt.Load()))

			switch {
			case gap > timeout && !lost:
				lost = true
				logger.Error("ingest: source watchdog: no tags received", "gap_s", gap.Seconds())
				notifier.Send("source_lost", fmt.Sprintf("Source lost (stream %q): no tags for %.1fs", streamKey, gap.Seconds()))
			case gap <= timeout && lost:
				lost = false
				logger.Info("ingest: source watchdog: recovered", "gap_s", gap.Seconds())
				notifier.Send("source_recovered", fmt.Sprintf("Source recovered (stream %q)", streamKey))
			}
		}
	}
}
