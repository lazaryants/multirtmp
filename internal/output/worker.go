// Package output отправляет теги из общего кольцевого буфера на один RTMP-
// выход: ждать данные -> подключиться -> липкие теги -> пейсинг по времени
// прихода тега (Arrival) + настроенная задержка -> побайтовая отправка.
// Соединение с назначением НЕ открывается, пока в буфере нет данных для
// немедленной отправки (см. CLAUDE.md, жёсткие правила) — этот порядок
// действует и при первом подключении, и при каждом реконнекте.
package output

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"multirtmp/internal/buffer"
)

const (
	kindAudio uint8 = 8
	kindVideo uint8 = 9
	kindData  uint8 = 18

	statsInterval = 2 * time.Second

	// connectTimeout ограничивает время на handshake одной попытки —
	// зависший на связи сервер назначения не должен блокировать реконнект
	// навсегда.
	connectTimeout = 10 * time.Second

	// catchUpGrace — см. Worker.reposition: выход с delay>0 перепрыгивает на
	// свежий keyframe, только если отстал больше чем на delay+catchUpGrace.
	catchUpGrace = 30 * time.Second

	// stableUptime — сколько нужно непрерывно и успешно отправлять теги,
	// чтобы засчитать попытку удавшейся и сбросить backoff в 0. Сброс сразу
	// по факту handshake был бы неверным: если сервер назначения принимает
	// соединение и тут же рвёт его (типичное поведение YouTube при
	// перегрузке/проблеме с ключом), backoff=0 после каждого handshake даёт
	// шторм мгновенных реконнектов.
	stableUptime = 10 * time.Second
)

// backoffSchedule — 0 → 0.5 → 1 → 2 → 5с, дальше 5с бесконечно.
var backoffSchedule = []time.Duration{0, 500 * time.Millisecond, time.Second, 2 * time.Second, 5 * time.Second}

func backoffFor(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= len(backoffSchedule) {
		return backoffSchedule[len(backoffSchedule)-1]
	}
	return backoffSchedule[attempt]
}

// Worker — воркер одного выхода.
type Worker struct {
	name   string
	url    string
	delay  time.Duration
	buf    *buffer.Buffer
	log    *slog.Logger
	status *status
}

// NewWorker создаёт воркер выхода name на url (уже с ключом потока в пути)
// с заданной задержкой пейсинга. В логи попадает только name и host — ключ
// маскируется (см. MaskURL).
func NewWorker(name, url string, delay time.Duration, buf *buffer.Buffer, logger *slog.Logger) *Worker {
	host, keyHint := MaskURL(url)

	return &Worker{
		name:   name,
		url:    url,
		delay:  delay,
		buf:    buf,
		status: newStatus(name),
		log:    logger.With("name", name, "host", host, "key", keyHint, "delay", delay),
	}
}

// Status возвращает снимок текущего состояния для GET /status.
func (w *Worker) Status() Status {
	return w.status.Snapshot()
}

// Run работает до отмены ctx, переподключаясь по backoff-расписанию при
// любой ошибке. Паника внутри одного воркера не должна ронять остальные —
// recover на верхнем уровне горутины.
func (w *Worker) Run(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("output: panic recovered", "panic", r)
		}
	}()

	cur := w.buf.NewCursor(w.delay)

	statsCtx, cancelStats := context.WithCancel(ctx)
	defer cancelStats()
	go w.status.run(statsCtx, w.log, statsInterval)

	attempt := 0
	var wait time.Duration

	for {
		if ctx.Err() != nil {
			return
		}

		if wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return
			}
		}

		uptime, err := w.runConnection(ctx, cur, attempt)

		if ctx.Err() != nil {
			w.log.Info("output: stopping", "reason", ctx.Err())
			return
		}

		if errors.Is(err, buffer.ErrReset) {
			// Новый publish — не сбой назначения, а ожидаемое событие: буфер
			// сброшен, старый курсор больше не годится. Реконнект без
			// ожидания, backoff не трогаем — он про здоровье destination'а,
			// а не источника.
			w.log.Warn("output: source republished, reconnecting", "error", err)
			w.status.recordReconnect(err)
			cur = w.buf.NewCursor(w.delay)
			wait = 0
			continue
		}

		w.status.recordReconnect(err)

		if uptime >= stableUptime {
			attempt = 0
		} else {
			attempt++
		}
		wait = backoffFor(attempt)

		w.log.Warn("output: connection lost, will retry",
			"error", err, "attempt", attempt, "retry_in", wait, "uptime", uptime)
	}
}

// runConnection — одна попытка подключения и потоковой отдачи. Возвращает
// продолжительность фазы streaming (0, если до неё не дошло) и причину
// остановки.
func (w *Worker) runConnection(ctx context.Context, cur *buffer.Cursor, attempt int) (time.Duration, error) {
	w.reposition(cur)

	w.status.setState(StateWaitingForSource)
	w.log.Info("output: waiting for source", "state", StateWaitingForSource, "attempt", attempt)

	first, err := cur.Next(ctx)
	if err != nil {
		return 0, err
	}

	w.status.setState(StateConnecting)
	w.log.Info("output: connecting", "state", StateConnecting, "attempt", attempt)

	dialCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	c, err := dial(dialCtx, w.url)
	cancel()
	if err != nil {
		w.log.Warn("output: connect failed", "error", err)
		return 0, err
	}
	defer c.close()

	if err := w.sendSticky(c); err != nil {
		w.log.Warn("output: failed to send sticky tags", "error", err)
		return 0, err
	}

	w.status.setState(StateStreaming)
	w.log.Info("output: streaming", "state", StateStreaming, "attempt", attempt)
	streamStartedAt := time.Now()

	var haveBase bool
	var baseTS uint32

	send := func(tag buffer.Tag) error {
		if sleepFor := time.Until(tag.Arrival.Add(w.delay)); sleepFor > 0 {
			t := time.NewTimer(sleepFor)
			defer t.Stop()
			select {
			case <-t.C:
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		if !haveBase {
			baseTS = tag.TS
			haveBase = true
		}
		relTS := tag.TS - baseTS

		if err := c.sendTag(tag.Kind, relTS, tag.Data); err != nil {
			return err
		}

		w.status.record(len(tag.Data), time.Since(tag.Arrival))
		return nil
	}

	if err := send(first); err != nil {
		if ctx.Err() == nil {
			w.log.Warn("output: write failed", "error", err)
		}
		return time.Since(streamStartedAt), err
	}

	for {
		tag, err := cur.Next(ctx)
		if err != nil {
			return time.Since(streamStartedAt), err
		}

		if err := send(tag); err != nil {
			if ctx.Err() == nil {
				w.log.Warn("output: write failed", "error", err)
			}
			return time.Since(streamStartedAt), err
		}
	}
}

// reposition применяет политику позиционирования курсора перед (пере)
// подключением. delay=0 — всегда на свежий keyframe (без отдельной ветки для
// первого коннекта — Reseek на только что созданном курсоре просто не
// меняет позицию). delay>0 — только если реально отстали больше чем на
// delay+catchUpGrace; иначе продолжаем с того места, где остановились
// (контент не теряется, задержка временно растёт).
func (w *Worker) reposition(cur *buffer.Cursor) {
	if w.delay == 0 {
		cur.Reseek(0)
		return
	}

	if lag, ok := cur.LagBehind(); ok && lag > w.delay+catchUpGrace {
		cur.Reseek(w.delay)
	}
}

// sendSticky отправляет onMetaData, AVC и AAC sequence header (если есть) с
// timestamp=0, как принято для липких тегов в начале нового соединения. К
// этому моменту в буфере уже есть данные (мы дождались первого тега перед
// connect), а значит sticky-слоты, если они вообще появятся у этой
// публикации, уже заполнены — ingest обновляет их раньше, чем Push будит курсор.
func (w *Worker) sendSticky(c *conn) error {
	sticky := w.buf.Sticky()

	for _, tag := range []*buffer.Tag{sticky.Metadata, sticky.AVCHeader, sticky.AACHeader} {
		if tag == nil {
			continue
		}
		if err := c.sendTag(tag.Kind, 0, tag.Data); err != nil {
			return err
		}
	}

	return nil
}
