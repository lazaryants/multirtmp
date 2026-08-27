package buffer

import (
	"context"
	"log/slog"
	"math"
	"time"
)

// Stats — снимок состояния буфера.
type Stats struct {
	Bytes     int64   // занято байт (сумма len(Tag.Data) удерживаемых тегов)
	Tags      int     // сколько тегов сейчас в буфере
	DepthS    float64 // глубина в секундах: Arrival самого нового минус самого старого
	Keyframes int     // сколько keyframe'ов в индексе
}

// Stats возвращает снимок текущего состояния буфера.
func (b *Buffer) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()

	st := Stats{
		Bytes:     b.curBytes,
		Tags:      len(b.tags),
		Keyframes: len(b.keyframes),
	}

	if len(b.tags) > 0 {
		oldest := b.tags[0].tag.Arrival
		newest := b.tags[len(b.tags)-1].tag.Arrival
		st.DepthS = newest.Sub(oldest).Seconds()
	}

	return st
}

// LogStats печатает снимок Stats раз в interval, пока не отменён ctx. Буфер
// живёт весь процесс (переживает Reset при новом publish), поэтому это
// самостоятельный репортер, не привязанный к жизненному циклу publish-сессии
// или выхода.
func (b *Buffer) LogStats(ctx context.Context, logger *slog.Logger, interval time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("buffer: panic in stats goroutine", "panic", r)
		}
	}()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			st := b.Stats()
			logger.Debug("buffer: stats",
				"bytes", st.Bytes,
				"tags", st.Tags,
				"depth_s", round(st.DepthS, 3),
				"keyframes", st.Keyframes,
			)
		}
	}
}

func round(x float64, decimals int) float64 {
	p := math.Pow(10, float64(decimals))
	return math.Round(x*p) / p
}
