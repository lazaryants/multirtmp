// Package buffer содержит общий кольцевой буфер FLV-тегов между ingest и
// выходами. Запись из ingest никогда не блокируется: при заполнении
// затирается самый старый тег. У каждого выхода свой курсор чтения.
package buffer

import (
	"sync"
	"time"
)

// Tag — один FLV-тег с метаданными приёма.
type Tag struct {
	Kind    uint8 // 8 audio / 9 video / 18 data
	TS      uint32
	Arrival time.Time
	Key     bool
	Data    []byte
}

type entry struct {
	seq uint64
	tag Tag
}

// Sticky — снимок липких тегов на момент подключения нового выхода.
type Sticky struct {
	Metadata  *Tag
	AVCHeader *Tag
	AACHeader *Tag
}

// Buffer — общий кольцевой буфер тегов одной публикации.
type Buffer struct {
	mu   sync.Mutex
	cond *sync.Cond

	capBytes int64
	curBytes int64

	tags      []entry
	keyframes []uint64 // seq тегов с Key==true, всегда в живом хвосте tags

	nextSeq    uint64
	generation uint64 // увеличивается при Reset, чтобы разбудить курсоры
	published  bool   // был ли хоть один publish; см. Reset

	metadata  *Tag
	avcHeader *Tag
	aacHeader *Tag
}

// New создаёт буфер ёмкостью capBytes байт (по сумме len(Tag.Data)).
func New(capBytes int64) *Buffer {
	b := &Buffer{capBytes: capBytes}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// Push добавляет тег в буфер. Никогда не блокируется дольше, чем нужно для
// обновления структур в памяти: при переполнении затирает старые теги, сама
// сеть/диск тут не участвуют.
func (b *Buffer) Push(tag Tag) {
	b.mu.Lock()

	e := entry{seq: b.nextSeq, tag: tag}
	b.nextSeq++
	b.tags = append(b.tags, e)
	b.curBytes += int64(len(tag.Data))
	if tag.Key {
		b.keyframes = append(b.keyframes, e.seq)
	}

	for b.curBytes > b.capBytes && len(b.tags) > 1 {
		oldest := b.tags[0]
		b.curBytes -= int64(len(oldest.tag.Data))
		b.tags = b.tags[1:]
		if oldest.tag.Key && len(b.keyframes) > 0 {
			b.keyframes = b.keyframes[1:]
		}
	}

	b.cond.Broadcast()
	b.mu.Unlock()
}

// SetCapacity меняет ёмкость буфера в байтах — используется автоподбором
// размера (see cmd/multirtmp/bufferauto.go). При уменьшении лишние старые
// теги не обрезаются немедленно, а естественно вытесняются при следующих
// Push — не тратим время под локом на внеочередную чистку.
func (b *Buffer) SetCapacity(bytes int64) {
	b.mu.Lock()
	b.capBytes = bytes
	b.mu.Unlock()
}

// Capacity возвращает текущую ёмкость буфера в байтах.
func (b *Buffer) Capacity() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.capBytes
}

// SetMetadata обновляет липкий тег onMetaData.
func (b *Buffer) SetMetadata(tag Tag) {
	b.mu.Lock()
	t := tag
	b.metadata = &t
	b.mu.Unlock()
}

// SetAVCHeader обновляет липкий тег AVC sequence header.
func (b *Buffer) SetAVCHeader(tag Tag) {
	b.mu.Lock()
	t := tag
	b.avcHeader = &t
	b.mu.Unlock()
}

// SetAACHeader обновляет липкий тег AAC sequence header.
func (b *Buffer) SetAACHeader(tag Tag) {
	b.mu.Lock()
	t := tag
	b.aacHeader = &t
	b.mu.Unlock()
}

// Sticky возвращает снимок текущих липких тегов.
func (b *Buffer) Sticky() Sticky {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Sticky{Metadata: b.metadata, AVCHeader: b.avcHeader, AACHeader: b.aacHeader}
}

// Reset полностью очищает буфер и липкие теги — вызывается при новом publish.
// Будит все курсоры, ожидающие в Next(), чтобы они сразу увидели сброс.
//
// Исключение — самый первый publish в только что созданный, ещё нетронутый
// буфер: сбрасывать там нечего, а курсоры, успевшие подключиться раньше
// первого publish (обычный порядок запуска: сервис поднялся, выходы
// подключились и ждут, потом стартовал vMix), не должны из-за этого рваться.
func (b *Buffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()

	wasPublished := b.published
	b.published = true

	if !wasPublished {
		return
	}

	b.tags = nil
	b.keyframes = nil
	b.curBytes = 0
	b.nextSeq = 0
	b.metadata = nil
	b.avcHeader = nil
	b.aacHeader = nil
	b.generation++
	b.cond.Broadcast()
}

// initialSeqForDelayLocked ищет seq тега, с которого должен начать читать
// новый курсор с заданной задержкой: ближайший keyframe с Arrival <=
// now-delay, а если буфер настолько глубоко не хранит — самый старый
// доступный keyframe. При delay=0 это всегда самый свежий keyframe — без
// отдельной ветки, тот же код.
func (b *Buffer) initialSeqForDelayLocked(delay time.Duration) uint64 {
	if len(b.keyframes) == 0 {
		if len(b.tags) > 0 {
			return b.tags[0].seq
		}
		return b.nextSeq
	}

	oldest := b.tags[0].seq
	cutoff := time.Now().Add(-delay)

	best := b.keyframes[0]
	for _, seq := range b.keyframes {
		if b.tags[seq-oldest].tag.Arrival.After(cutoff) {
			break
		}
		best = seq
	}

	return best
}
