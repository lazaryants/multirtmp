package output_test

// Проверка 24-битного RTMP timestamp через границу 0xFFFFFF (~4ч39м):
// прогоняет синтетический поток с TS вокруг и за границей через настоящий
// wire round-trip — наш output.Worker (запись через gortmplib/rawmessage.Writer)
// в наш же ingest.Server (чтение через yutopp/go-rtmp) по loopback-соединению
// на локальном порту. Это не арифметика на бумаге: extended timestamp
// реально кодируется на проводе и реально декодируется обратно нашим кодом.
//
// Спортивные трансляции (крикет: 4-8ч, ODI до 8ч) гарантированно пересекают
// эту границу в середине эфира — не теоретический случай.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"multirtmp/internal/buffer"
	"multirtmp/internal/ingest"
	"multirtmp/internal/output"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// videoTagBytes — минимально валидный AVC NALU видео-тег (keyframe), которого
// достаточно, чтобы пройти parseVideoTag на принимающей стороне: не несёт
// реального кадра, только заголовок + немного мусора вместо payload.
func videoTagBytes() []byte {
	return []byte{0x17, 0x01, 0x00, 0x00, 0x00, 0xAA, 0xBB, 0xCC, 0xDD}
}

// startLoopbackIngest поднимает ingest.Server на addr, пишущий в buf, и
// возвращает функцию очистки.
func startLoopbackIngest(t *testing.T, addr string, buf *buffer.Buffer) func() {
	t.Helper()

	logger := discardLogger()
	srv := ingest.NewServer(addr, logger, buf, 5*time.Second, nil)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	// Дать листенеру подняться — ListenAndServe стартует net.Listen синхронно
	// перед тем как заблокироваться в Accept, но сама горутина ещё не
	// гарантированно успела выполниться к этому моменту.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ingest server on %s did not come up in time", addr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	return func() {
		_ = srv.Close()
		select {
		case <-errCh:
		case <-time.After(2 * time.Second):
			t.Error("ingest server did not stop in time")
		}
	}
}

// resilientReader читает из buf, прозрачно пересоздавая курсор при
// ErrReset — нужно для сценария "реконнект после нового publish": между
// bufA.Reset() на стороне источника и фактическим h.buf.Reset() на
// принимающей стороне (он срабатывает не раньше, чем воркер реально
// переподключится и опубликует заново — см. TestTimestampWraparoundReconnect)
// проходит какое-то время, и точный момент непредсказуем. Пересоздание по
// факту ErrReset, а не по угаданному таймингу, убирает эту гонку.
type resilientReader struct {
	buf *buffer.Buffer
	cur *buffer.Cursor
}

func newResilientReader(buf *buffer.Buffer) *resilientReader {
	return &resilientReader{buf: buf, cur: buf.NewCursor(0)}
}

func (r *resilientReader) next(t *testing.T, timeout time.Duration) uint32 {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("resilientReader: timed out waiting for next tag")
		}

		ctx, cancel := context.WithTimeout(context.Background(), remaining)
		tag, err := r.cur.Next(ctx)
		cancel()

		if err == nil {
			return tag.TS
		}
		if errors.Is(err, buffer.ErrReset) {
			r.cur = r.buf.NewCursor(0)
			continue
		}
		t.Fatalf("Next: %v", err)
		return 0
	}
}

// streamTag кладёт в src ровно один тег с TS=ts (Arrival — сейчас минус
// backdate, чтобы пейсинг, включая delay>0, не заставлял тест реально
// ждать), затем блокируется, пока он не будет прочитан на приёмной стороне
// через r — т.е. пока не завершится полный wire round-trip запись->
// передача->разбор. Тег за тегом, а не пачкой: у Cursor.NewCursor/Reseek
// для delay=0 «встать на самый свежий keyframe» — намеренное поведение (см.
// CLAUDE.md), и если пушить все теги разом ДО того, как воркер успеет
// позиционироваться, он корректно проглотит все, кроме последнего, и
// честно зависнет в ожидании следующего, которого не будет. Потоковая
// подача тег-за-тегом убирает эту гонку и заодно больше похожа на
// реальный источник.
func streamTag(t *testing.T, src *buffer.Buffer, r *resilientReader, ts uint32, backdate time.Duration, readTimeout time.Duration) uint32 {
	t.Helper()

	src.Push(buffer.Tag{
		Kind:    9,
		TS:      ts,
		Arrival: time.Now().Add(-backdate),
		Key:     true,
		Data:    videoTagBytes(),
	})

	return r.next(t, readTimeout)
}

// streamWraparoundSequence прогоняет n тегов с TS = startTS + i*step (step
// специально большой, чтобы пересечь границу 0xFFFFFF за несколько тегов, а
// не за 4.6 реальных часа) через src -> r, тег за тегом, и возвращает
// полученные на принимающей стороне TS.
func streamWraparoundSequence(t *testing.T, src *buffer.Buffer, r *resilientReader, startTS, step uint32, n int, backdate time.Duration) []uint32 {
	t.Helper()

	got := make([]uint32, 0, n)
	for i := 0; i < n; i++ {
		ts := startTS + uint32(i)*step
		got = append(got, streamTag(t, src, r, ts, backdate, 5*time.Second))
	}
	return got
}

// assertMonotonicRebasedFrom0xFFFFFF проверяет, что got — это последовательность
// абсолютных timestamp'ов, которые: 1) строго не убывают, 2) действительно
// пересекают границу 0xFFFFFF где-то в середине (не только до и не только
// после — иначе тест ничего не проверяет), 3) значения после границы —
// настоящие большие uint32, а не обрезанные/завёрнутые в 24 бита.
func assertMonotonicRebasedFrom0xFFFFFF(t *testing.T, got []uint32) {
	t.Helper()

	const boundary = uint32(0xFFFFFF)

	crossedBefore, crossedAfter := false, false
	for i, ts := range got {
		if i > 0 && ts < got[i-1] {
			t.Fatalf("timestamp went backwards at tag %d: %#x -> %#x (full sequence: %#x)", i, got[i-1], ts, got)
		}
		if ts <= boundary {
			crossedBefore = true
		}
		if ts > boundary {
			crossedAfter = true
		}
	}

	if !crossedBefore || !crossedAfter {
		t.Fatalf("test setup didn't actually straddle the 0xFFFFFF boundary — got %#x (crossedBefore=%v crossedAfter=%v)",
			got, crossedBefore, crossedAfter)
	}

	// Ничего не должно быть "подозрительно маленьким" после пересечения —
	// признак того, что где-то по пути значение обрезали до 24 бит.
	for i, ts := range got {
		if i > 0 && got[i-1] > boundary && ts < got[i-1]-boundary {
			t.Fatalf("tag %d looks truncated to 24 bits: prev=%#x this=%#x", i, got[i-1], ts)
		}
	}
}

// --- дальше собственно сценарии ---

// TestTimestampWraparoundWhileStreaming — сценарий 1: переполнение при
// работающем выходе (delay=0), в рамках одного соединения.
func TestTimestampWraparoundWhileStreaming(t *testing.T) {
	const addr = "127.0.0.1:19601"

	bufB := buffer.New(64 * 1024 * 1024) // принимающая сторона (наш же ingest)
	stop := startLoopbackIngest(t, addr, bufB)
	defer stop()

	bufA := buffer.New(64 * 1024 * 1024) // источник
	bufA.Reset()                         // отметить "уже была публикация" — см. TestTimestampWraparoundReconnect

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := output.NewWorker("wrap1", "rtmp://"+addr+"/live/wrap1", 0, bufA, discardLogger(), nil)
	go w.Run(ctx)

	// relTS у выхода = tag.TS - baseTS = tag.TS - startTS = i*step здесь,
	// т.к. это первый тег этого соединения (rebase от нуля). step подобран
	// так, чтобы пересечь 0xFFFFFF за 10 тегов, а не за 4.6 часа.
	const startTS = uint32(0)
	const step = uint32(4_000_000)
	const n = 10

	curB := newResilientReader(bufB)
	got := streamWraparoundSequence(t, bufA, curB, startTS, step, n, time.Minute)

	t.Logf("received TS sequence: %#x", got)
	assertMonotonicRebasedFrom0xFFFFFF(t, got)
}

// TestTimestampWraparoundWithDelay — сценарий 2: то же самое, но выход с
// delay=15s — проверяет, что delay-специфичные пути (Cursor.Reseek/
// LagBehind/reposition) не портят timestamp-логику. Тест не ждёт реальные
// 15 секунд: Arrival тегов забэкдейчен дальше, чем delay, так что
// waitUntilDue возвращается немедленно.
func TestTimestampWraparoundWithDelay(t *testing.T) {
	const addr = "127.0.0.1:19602" // отдельный порт: у ingest один активный источник одновременно

	bufB := buffer.New(64 * 1024 * 1024)
	stop := startLoopbackIngest(t, addr, bufB)
	defer stop()

	bufA := buffer.New(64 * 1024 * 1024)
	bufA.Reset()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const delay = 15 * time.Second
	w := output.NewWorker("wrap2", "rtmp://"+addr+"/live/wrap2", delay, bufA, discardLogger(), nil)
	go w.Run(ctx)

	const startTS = uint32(0)
	const step = uint32(4_000_000)
	const n = 10

	curB := newResilientReader(bufB)
	// backdate заметно больше delay, иначе waitUntilDue реально ждал бы.
	got := streamWraparoundSequence(t, bufA, curB, startTS, step, n, delay+time.Minute)

	t.Logf("received TS sequence (delay=15s): %#x", got)
	assertMonotonicRebasedFrom0xFFFFFF(t, got)
}

// TestTimestampWraparoundReconnect — сценарий 3: реконнект сразу после
// пересечения границы (новый publish источника — buffer.Reset) — проверяет,
// что baseTS на выходе пересчитывается заново от первого тега НОВОЙ сессии,
// а не продолжает расти от того, что было до реконнекта.
func TestTimestampWraparoundReconnect(t *testing.T) {
	const addr = "127.0.0.1:19603"

	bufB := buffer.New(64 * 1024 * 1024)
	stop := startLoopbackIngest(t, addr, bufB)
	defer stop()

	bufA := buffer.New(64 * 1024 * 1024)
	bufA.Reset() // первая "публикация" начинается — см. Buffer.Reset: первый вызов не рвёт уже подключённые курсоры

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := output.NewWorker("wrap3", "rtmp://"+addr+"/live/wrap3", 0, bufA, discardLogger(), nil)
	go w.Run(ctx)

	const step = uint32(4_000_000)
	const n = 10

	curB := newResilientReader(bufB)

	// Первая сессия: пересекаем границу, как в сценарии 1.
	firstBatch := streamWraparoundSequence(t, bufA, curB, 0, step, n, time.Minute)
	t.Logf("first session TS sequence: %#x", firstBatch)
	assertMonotonicRebasedFrom0xFFFFFF(t, firstBatch)

	lastOfFirst := firstBatch[len(firstBatch)-1]
	if lastOfFirst <= 0xFFFFFF {
		t.Fatalf("test setup issue: first session should have ended past the boundary, got %#x", lastOfFirst)
	}

	// Новый publish источника: буфер сбрасывается, воркер должен поймать
	// ErrReset, взять новый курсор и переподключиться немедленно (проверено
	// на этапе 4) — здесь важно, что НОВОЕ соединение снова стартует с
	// baseTS от своего первого тега, а не продолжает счёт от lastOfFirst.
	// clearCurrent/takeOver на приёмной стороне тоже отработает — новое TCP-
	// соединение от воркера вытеснит старое, как обычная смена publisher'а,
	// и h.buf.Reset() на приёмной стороне (bufB) сработает по-настоящему —
	// у неё это уже НЕ первый publish. Момент, когда именно это случится,
	// нам неизвестен (воркер не подключается, пока нет готового тега — см.
	// правку про delay ранее в этой сессии) — curB (resilientReader) сам
	// пересоздаст себя по факту ErrReset при следующем чтении, timing не важен.
	bufA.Reset()

	// Абсолютный TS новой публикации — небольшой и никак не связанный с тем,
	// на чём остановилась прошлая сессия (у нового источника своя жизнь).
	const secondStartTS = uint32(500)
	got := streamWraparoundSequence(t, bufA, curB, secondStartTS, 40, 5, time.Minute)
	t.Logf("second session TS sequence: %#x", got)

	if got[0] > 1000 {
		t.Fatalf("expected a fresh rebase near 0 after reconnect, got first TS = %#x (previous session ended at %#x)",
			got[0], lastOfFirst)
	}
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Fatalf("post-reconnect timestamp went backwards at tag %d: %#x -> %#x", i, got[i-1], got[i])
		}
	}
}
