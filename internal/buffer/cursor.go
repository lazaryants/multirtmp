package buffer

import (
	"context"
	"errors"
	"time"
)

// ErrReset возвращается из Cursor.Next, когда буфер был сброшен новым
// publish (см. Buffer.Reset). Курсор больше не годится, вызывающий должен
// прекратить работу с ним.
var ErrReset = errors.New("buffer: reset by new publish")

// Cursor — курсор чтения одного выхода. Не потокобезопасен для параллельного
// использования из нескольких горутин одновременно (у каждого выхода — своя
// горутина и свой курсор).
type Cursor struct {
	buf        *Buffer
	generation uint64
	nextSeq    uint64
}

// NewCursor создаёт курсор, изначально позиционированный на ближайший
// keyframe к моменту "сейчас минус delay" (см. initialSeqForDelayLocked).
func (b *Buffer) NewCursor(delay time.Duration) *Cursor {
	b.mu.Lock()
	defer b.mu.Unlock()

	return &Cursor{
		buf:        b,
		generation: b.generation,
		nextSeq:    b.initialSeqForDelayLocked(delay),
	}
}

// Next блокируется до появления следующего тега (через sync.Cond, без
// busy-wait) и возвращает его. Если курсор отстал настолько, что нужный тег
// уже был затёрт, он молча перепозиционируется на ближайший ещё живой
// keyframe. Если буфер был сброшен новым publish — возвращает ErrReset. Если
// отменён ctx (например, остановка процесса, пока источника ещё не было) —
// возвращает ctx.Err(): без этого ожидание на sync.Cond ничего не знает про
// ctx и может зависнуть навсегда.
func (c *Cursor) Next(ctx context.Context) (Tag, error) {
	b := c.buf

	// Будим ВСЕХ ожидающих на Cond при отмене ctx; каждый сам решает по
	// своему ctx, останавливаться ли, — общий Broadcast для этого безопасен.
	stop := context.AfterFunc(ctx, func() {
		b.mu.Lock()
		b.cond.Broadcast()
		b.mu.Unlock()
	})
	defer stop()

	b.mu.Lock()
	defer b.mu.Unlock()

	for {
		if err := ctx.Err(); err != nil {
			return Tag{}, err
		}

		if c.generation != b.generation {
			return Tag{}, ErrReset
		}

		if len(b.tags) > 0 {
			oldest := b.tags[0].seq
			if c.nextSeq < oldest {
				c.repositionLocked()
			}

			if idx := c.nextSeq - oldest; idx < uint64(len(b.tags)) {
				e := b.tags[idx]
				c.nextSeq = e.seq + 1
				return e.tag, nil
			}
		}

		b.cond.Wait()
	}
}

// repositionLocked прыгает на ближайший ещё живой keyframe. Вызывается,
// когда тег, который курсор собирался прочитать, уже был затёрт.
func (c *Cursor) repositionLocked() {
	b := c.buf

	if len(b.keyframes) > 0 {
		c.nextSeq = b.keyframes[0]
		return
	}

	if len(b.tags) > 0 {
		c.nextSeq = b.tags[0].seq
	}
}

// Reseek перепозиционирует курсор на ближайший keyframe к "сейчас минус
// delay" — та же логика, что и при создании курсора (initialSeqForDelayLocked).
// Используется при реконнекте: выход с delay=0 всегда прыгает на свежий
// keyframe, выход с delay>0 — только если отстал больше чем на delay+30s (см.
// LagBehind). Если буфер уже сброшен новым publish — ничего не делает, курсор
// всё равно недействителен и следующий Next вернёт ErrReset.
func (c *Cursor) Reseek(delay time.Duration) {
	b := c.buf
	b.mu.Lock()
	defer b.mu.Unlock()

	if c.generation != b.generation {
		return
	}
	c.nextSeq = b.initialSeqForDelayLocked(delay)
}

// LagBehind возвращает, насколько по времени прихода отстаёт от "сейчас"
// следующий непрочитанный тег курсора. ok=false — курсор уже на живом крае
// (или буфер сброшен/пуст), мерить не с чем.
func (c *Cursor) LagBehind() (lag time.Duration, ok bool) {
	b := c.buf
	b.mu.Lock()
	defer b.mu.Unlock()

	if c.generation != b.generation || len(b.tags) == 0 {
		return 0, false
	}

	oldest := b.tags[0].seq
	next := c.nextSeq
	if next < oldest {
		next = oldest // курсор отстал за уже затёртые данные — мерим по самому старому доступному
	}

	off := next - oldest
	if off >= uint64(len(b.tags)) {
		return 0, false // курсор уже догнал живой край
	}

	return time.Since(b.tags[off].tag.Arrival), true
}
