// Package output отправляет теги из общего кольцевого буфера на один RTMP-
// выход: ждать тег, готовый к немедленной отправке (Arrival+delay <= now) ->
// подключиться -> липкие теги -> побайтовая отправка, дальше пейсинг по
// Arrival+delay для каждого следующего тега. Соединение с назначением НЕ
// открывается, пока такого тега нет (см. CLAUDE.md, жёсткие правила) — этот
// порядок действует и при первом подключении, и при каждом реконнекте.
package output

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"multirtmp/internal/buffer"
	"multirtmp/internal/notify"
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

	// neverConnectedBackoffCeiling — потолок backoff для выхода, который ни
	// разу за свой жизненный цикл не дошёл до streaming. Обычный потолок 5с
	// рассчитан на "уже работавшее, временно отвалившееся" направление;
	// для заведомо неверного url/ключа (например, опечатка в ключе YouTube)
	// это означает штурм в 12+ попыток в минуту часами — реальный риск
	// временной блокировки по IP на стороне назначения. После
	// neverConnectedThreshold подряд неудачных попыток без единого успешного
	// streaming потолок поднимается сюда; после первого успешного streaming —
	// навсегда возвращается к обычному backoffSchedule.
	neverConnectedBackoffCeiling = 60 * time.Second
)

// backoffSchedule — 0 → 0.5 → 1 → 2 → 5с, дальше 5с (или
// neverConnectedBackoffCeiling, если выход ни разу не выходил в streaming —
// см. backoffFor) бесконечно.
var backoffSchedule = []time.Duration{0, 500 * time.Millisecond, time.Second, 2 * time.Second, 5 * time.Second}

// neverConnectedThreshold — после скольких подряд неудачных попыток (без
// единого успешного streaming) потолок backoff поднимается с 5с до
// neverConnectedBackoffCeiling. Совпадает с длиной обычного расписания:
// ровно тот момент, когда мы и так уже упёрлись бы в обычный потолок.
var neverConnectedThreshold = len(backoffSchedule)

func backoffFor(attempt int, everStreamed bool) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt < len(backoffSchedule) {
		return backoffSchedule[attempt]
	}
	if !everStreamed {
		return neverConnectedBackoffCeiling
	}
	return backoffSchedule[len(backoffSchedule)-1]
}

// Worker — воркер одного выхода.
type Worker struct {
	name     string
	url      string
	delay    time.Duration
	buf      *buffer.Buffer
	log      *slog.Logger
	status   *status
	notifier *notify.Notifier
}

// NewWorker создаёт воркер выхода name на url (уже с ключом потока в пути)
// с заданной задержкой пейсинга. В логи попадает только name и host — ключ
// маскируется (см. MaskURL). notifier может быть nil/выключен.
func NewWorker(name, url string, delay time.Duration, buf *buffer.Buffer, logger *slog.Logger, notifier *notify.Notifier) *Worker {
	host, keyHint := MaskURL(url)

	return &Worker{
		name:     name,
		url:      url,
		delay:    delay,
		buf:      buf,
		status:   newStatus(name),
		notifier: notifier,
		log:      logger.With("name", name, "host", host, "key", keyHint, "delay", delay),
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
	go w.status.run(statsCtx, w.log, statsInterval, w.notifier, w.delay)

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
			w.status.recordReset(err)
			cur = w.buf.NewCursor(w.delay)
			wait = 0
			continue
		}

		w.status.recordFailure(err, w.notifier)

		if uptime >= stableUptime {
			attempt = 0
		} else {
			attempt++
		}

		everStreamed := w.status.hasEverStreamed()
		wait = backoffFor(attempt, everStreamed)

		// Ровно момент перехода на повышенный потолок — не на каждой
		// последующей попытке, иначе это ровно тот шторм (уведомлений),
		// который мы пытаемся убрать из соединений с назначением.
		if !everStreamed && attempt == neverConnectedThreshold {
			w.log.Warn("output: has never connected successfully, check url/key",
				"attempts", attempt, "backoff_ceiling", neverConnectedBackoffCeiling)
			w.notifier.Send("output_never_connected:"+w.name, fmt.Sprintf(
				"Output %q has never connected successfully after %d attempts — check url/key",
				w.name, attempt))
		}

		w.log.Warn("output: connection lost, will retry",
			"error", err, "attempt", attempt, "retry_in", wait, "uptime", uptime)
	}
}

// runConnection — одна попытка подключения и потоковой отдачи. Возвращает
// продолжительность фазы streaming (0, если до неё не дошло) и причину
// остановки.
func (w *Worker) runConnection(ctx context.Context, cur *buffer.Cursor, attempt int) (time.Duration, error) {
	w.reposition(cur)

	w.status.setState(StateWaitingForSource, w.notifier)
	w.log.Info("output: waiting for source", "state", StateWaitingForSource, "attempt", attempt)

	first, err := cur.Next(ctx)
	if err != nil {
		return 0, err
	}

	// Ждать нужно не "тег есть в буфере", а "тег готов к немедленной отправке
	// с учётом задержки" — иначе соединение открывается и потом простаивает
	// до Arrival+delay, а сервер назначения рвёт простаивающий publish по
	// своему read timeout (та же причина, что и раньше, просто дальше по
	// цепочке: сон должен быть ДО connect, а не после).
	if err := waitUntilDue(ctx, first, w.delay); err != nil {
		return 0, err
	}

	w.status.setState(StateConnecting, w.notifier)
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

	w.status.setState(StateStreaming, w.notifier)
	w.log.Info("output: streaming", "state", StateStreaming, "attempt", attempt)
	streamStartedAt := time.Now()

	var haveBase bool
	var baseTS uint32

	send := func(tag buffer.Tag) error {
		if err := waitUntilDue(ctx, tag, w.delay); err != nil {
			return err
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

// waitUntilDue блокируется до tag.Arrival+delay (пейсинг строго по времени
// прихода, не по внутренним timestamp'ам). Если тег уже готов к отправке —
// возвращается немедленно, без отдельной ветки для delay=0.
func waitUntilDue(ctx context.Context, tag buffer.Tag, delay time.Duration) error {
	sleepFor := time.Until(tag.Arrival.Add(delay))
	if sleepFor <= 0 {
		return nil
	}

	t := time.NewTimer(sleepFor)
	defer t.Stop()

	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
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
