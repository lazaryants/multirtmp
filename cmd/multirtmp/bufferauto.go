package main

import (
	"context"
	"log/slog"
	"math"
	"time"

	"multirtmp/internal/buffer"
	"multirtmp/internal/config"
	"multirtmp/internal/ingest"
)

const (
	bufferAutoInterval = 2 * time.Second

	// bufferAutoRetention — запас сверх максимальной задержки среди выходов.
	// Покрывает правило "прыжок вперёд при отставании > delay + 30s" с
	// двойным запасом: даже полный обрыв связи на время реконнекта не должен
	// вытолкнуть из буфера то, что делавшему delay-выходу ещё нужно.
	bufferAutoRetention = 60 * time.Second
)

// runBufferAutosize периодически пересчитывает ёмкость buf как
// (maxDelay + bufferAutoRetention) * измеренный битрейт источника,
// ограничивая результат [config.BufferMinSize, maxSize]. Активен только пока
// buffer.size не задан явно в конфиге (см. cfg.BufferAuto) — до первой
// публикации, пока измерить нечего, буфер остаётся на начальном
// config.BufferMinSize.
func runBufferAutosize(ctx context.Context, buf *buffer.Buffer, src *ingest.Server, logger *slog.Logger, maxDelay time.Duration, maxSize int64) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("main: panic in buffer autosize goroutine", "panic", r)
		}
	}()

	ticker := time.NewTicker(bufferAutoInterval)
	defer ticker.Stop()

	window := (maxDelay + bufferAutoRetention).Seconds()
	lastSize := int64(-1)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			st := src.SourceStatus()
			if !st.Connected {
				continue
			}

			kbps := st.VideoKbps + st.AudioKbps
			if kbps <= 0 {
				continue
			}

			bytesPerSec := float64(kbps) * 1000 / 8
			size := int64(bytesPerSec * window)

			if size < config.BufferMinSize {
				size = config.BufferMinSize
			}
			if size > maxSize {
				size = maxSize
			}

			if size == lastSize {
				continue
			}
			lastSize = size

			buf.SetCapacity(size)

			logger.Info("buffer: resized",
				"bytes", size,
				"kbps", kbps,
				"max_delay", maxDelay,
				"retention_s", round(float64(size)/bytesPerSec, 1),
			)
		}
	}
}

func round(x float64, decimals int) float64 {
	p := math.Pow(10, float64(decimals))
	return math.Round(x*p) / p
}
