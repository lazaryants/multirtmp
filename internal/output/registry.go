package output

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Registry управляет именованными выходами: регистрация при старте
// процесса, включение/выключение по запросу (POST /outputs/{name}/start|stop),
// сводка состояний для GET /status. Задержка каждого выхода при этом не
// меняется — это не про Registry, а про Worker.delay, который задаётся один
// раз при создании и меняется только пересозданием процесса.
type Registry struct {
	ctx context.Context // корневой, отменяется при остановке процесса

	mu    sync.Mutex
	items map[string]*registryEntry
}

type registryEntry struct {
	worker *Worker
	cancel context.CancelFunc // nil, если не запущен
	done   chan struct{}      // закрывается, когда Run() вернулась; nil, если не запущен
}

// NewRegistry создаёт реестр. ctx — корневой контекст процесса: все
// запущенные через Start выходы получают контекст, производный от него, и
// автоматически останавливаются при его отмене.
func NewRegistry(ctx context.Context) *Registry {
	return &Registry{ctx: ctx, items: map[string]*registryEntry{}}
}

// Register регистрирует воркер под именем name, не запуская его.
func (r *Registry) Register(name string, w *Worker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items[name] = &registryEntry{worker: w}
}

// Start запускает выход name, если он ещё не запущен. Повторный Start уже
// запущенного выхода — не ошибка, просто ничего не делает.
func (r *Registry) Start(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.items[name]
	if !ok {
		return fmt.Errorf("unknown output %q", name)
	}
	if e.cancel != nil {
		return nil
	}

	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan struct{})
	e.cancel = cancel
	e.done = done

	go func() {
		defer close(done)
		e.worker.Run(ctx)
	}()

	return nil
}

// Stop останавливает выход name, если он запущен, и дожидается фактического
// завершения его горутины (Run реагирует на отмену ctx быстро — см.
// Cursor.Next(ctx)). Повторный Stop уже остановленного выхода — не ошибка.
func (r *Registry) Stop(name string) error {
	r.mu.Lock()
	e, ok := r.items[name]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("unknown output %q", name)
	}
	cancel := e.cancel
	done := e.done
	e.cancel = nil
	e.done = nil
	r.mu.Unlock()

	if cancel == nil {
		return nil
	}
	cancel()
	<-done

	return nil
}

// StartAll запускает все зарегистрированные выходы, для которых names
// содержит их имя (обычно — те, у кого enabled=true в конфиге).
func (r *Registry) StartAll(names []string) {
	for _, name := range names {
		_ = r.Start(name)
	}
}

// Wait блокируется, пока не завершатся горутины всех выходов, которые были
// запущены на момент вызова. Используется при штатной остановке процесса —
// после отмены корневого ctx все они и так завершаются сами, Wait просто
// дожидается.
func (r *Registry) Wait() {
	r.mu.Lock()
	dones := make([]chan struct{}, 0, len(r.items))
	for _, e := range r.items {
		if e.done != nil {
			dones = append(dones, e.done)
		}
	}
	r.mu.Unlock()

	for _, d := range dones {
		<-d
	}
}

// Snapshot возвращает состояние всех зарегистрированных выходов,
// отсортированное по имени. Не запущенные сейчас показывают State ==
// StateStopped независимо от того, что было в них последним, пока работали.
func (r *Registry) Snapshot() []Status {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]Status, 0, len(r.items))
	for _, e := range r.items {
		st := e.worker.Status()
		if e.cancel == nil {
			st.State = StateStopped
		}
		out = append(out, st)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out
}
