package ingest

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// stats копит счётчики одной публикации и раз в reportInterval печатает сводку в лог.
type stats struct {
	videoBytes atomic.Int64
	audioBytes atomic.Int64
	videoTags  atomic.Int64
	totalTags  atomic.Int64

	mu sync.Mutex

	haveWindowTS  bool
	windowFirstTS uint32
	windowLastTS  uint32
	lastFPS       float64

	haveLastKeyframe  bool
	lastKeyframeAt    time.Time
	windowKfIntervals []time.Duration
	haveKfIntervalAvg bool
	lastKfIntervalAvg time.Duration

	lastVideoKbps int64
	lastAudioKbps int64
}

func newStats() *stats {
	return &stats{}
}

// addVideo регистрирует видео-тег. ts — RTMP timestamp тега (используется для
// fps, который иначе скачет из-за пачечной доставки тегов от vMix), now —
// время прихода (используется для интервала между keyframe).
func (s *stats) addVideo(n int, isKeyFrame bool, ts uint32, now time.Time) {
	s.videoBytes.Add(int64(n))
	s.videoTags.Add(1)
	s.totalTags.Add(1)

	s.mu.Lock()
	if !s.haveWindowTS {
		s.windowFirstTS = ts
		s.haveWindowTS = true
	}
	s.windowLastTS = ts

	if isKeyFrame {
		if s.haveLastKeyframe {
			s.windowKfIntervals = append(s.windowKfIntervals, now.Sub(s.lastKeyframeAt))
		}
		s.lastKeyframeAt = now
		s.haveLastKeyframe = true
	}
	s.mu.Unlock()
}

func (s *stats) addAudio(n int) {
	s.audioBytes.Add(int64(n))
	s.totalTags.Add(1)
}

func (s *stats) addOther() {
	s.totalTags.Add(1)
}

// swapFPS вычисляет fps по разбросу timestamp'ов видео-тегов, попавших в
// текущее окно (videoTags — число тегов в окне). Если тегов недостаточно,
// чтобы оценить интервал, возвращает последнее известное значение.
func (s *stats) swapFPS(videoTags int64) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	haveTS := s.haveWindowTS
	firstTS, lastTS := s.windowFirstTS, s.windowLastTS
	s.haveWindowTS = false

	if haveTS && videoTags > 1 {
		deltaMs := int32(lastTS - firstTS) // корректно и через переполнение 24/32-бит TS
		if deltaMs > 0 {
			s.lastFPS = float64(videoTags-1) / (float64(deltaMs) / 1000.0)
		}
	}

	return s.lastFPS
}

// swapKeyframeInterval усредняет интервалы между keyframe, попавшими в
// текущее окно. Если в окно не попал ни один keyframe, возвращает последнее
// известное значение. ok=false означает, что ни в этом окне, ни раньше не
// было ни одного полного интервала между keyframe — значение показывать не надо.
func (s *stats) swapKeyframeInterval() (avg time.Duration, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.windowKfIntervals) > 0 {
		var sum time.Duration
		for _, d := range s.windowKfIntervals {
			sum += d
		}
		s.lastKfIntervalAvg = sum / time.Duration(len(s.windowKfIntervals))
		s.windowKfIntervals = s.windowKfIntervals[:0]
		s.haveKfIntervalAvg = true
	}

	return s.lastKfIntervalAvg, s.haveKfIntervalAvg
}

// round округляет x до decimals знаков после запятой.
func round(x float64, decimals int) float64 {
	p := math.Pow(10, float64(decimals))
	return math.Round(x*p) / p
}

// Snapshot — статистика источника на момент запроса (последние посчитанные
// за интервал значения; нули/false, пока не прошёл первый тик после publish).
type Snapshot struct {
	VideoKbps            int64
	AudioKbps            int64
	FPS                  float64
	KeyframeIntervalS    float64
	HaveKeyframeInterval bool
	TotalTags            int64
}

// Snapshot возвращает снимок для GET /status. Можно звать из любой горутины.
func (s *stats) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	return Snapshot{
		VideoKbps:            s.lastVideoKbps,
		AudioKbps:            s.lastAudioKbps,
		FPS:                  round(s.lastFPS, 2),
		KeyframeIntervalS:    round(s.lastKfIntervalAvg.Seconds(), 3),
		HaveKeyframeInterval: s.haveKfIntervalAvg,
		TotalTags:            s.totalTags.Load(),
	}
}

// run печатает сводную статистику раз в interval, пока не отменён ctx.
func (s *stats) run(ctx context.Context, logger *slog.Logger, interval time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("ingest: panic in stats goroutine", "panic", r)
		}
	}()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			videoBytes := s.videoBytes.Swap(0)
			audioBytes := s.audioBytes.Swap(0)
			videoTags := s.videoTags.Swap(0)
			totalTags := s.totalTags.Load()

			secs := interval.Seconds()

			videoKbps := int64(math.Round(float64(videoBytes) * 8 / 1000 / secs))
			audioKbps := int64(math.Round(float64(audioBytes) * 8 / 1000 / secs))

			s.mu.Lock()
			s.lastVideoKbps = videoKbps
			s.lastAudioKbps = audioKbps
			s.mu.Unlock()

			attrs := []any{
				"video_kbps", videoKbps,
				"audio_kbps", audioKbps,
				"fps", round(s.swapFPS(videoTags), 2),
			}
			if kfInterval, ok := s.swapKeyframeInterval(); ok {
				attrs = append(attrs, "keyframe_interval_s", round(kfInterval.Seconds(), 3))
			}
			attrs = append(attrs, "total_tags", totalTags)

			logger.Debug("ingest: stats", attrs...)
		}
	}
}
