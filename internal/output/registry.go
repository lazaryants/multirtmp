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
	r.startLocked(e)

	return nil
}

// startLocked запускает воркер entry, если он ещё не запущен. Вызывающий уже
// держит r.mu.
func (r *Registry) startLocked(e *registryEntry) {
	if e.cancel != nil {
		return
	}

	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan struct{})
	e.cancel = cancel
	e.done = done

	go func() {
		defer close(done)
		e.worker.Run(ctx)
	}()
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
	done := r.stopLocked(e)
	r.mu.Unlock()

	if done != nil {
		<-done
	}

	return nil
}

// stopLocked отменяет контекст воркера entry, если он запущен, и возвращает
// его done-канал для ожидания ВНЕ лока (сам Run() может обращаться к
// Registry только опосредованно, но дожидаться завершения горутины под r.mu
// незачем — это удлиняет удержание лока без причины). nil, если уже не
// запущен. Вызывающий уже держит r.mu.
func (r *Registry) stopLocked(e *registryEntry) chan struct{} {
	cancel := e.cancel
	done := e.done
	e.cancel = nil
	e.done = nil

	if cancel == nil {
		return nil
	}
	cancel()

	return done
}

// Add регистрирует и (если start) запускает новый выход name. Ошибка, если
// имя уже занято — в отличие от Register (доверенный путь при старте
// процесса, где уникальность имён уже проверена валидацией конфига), Add
// вызывается из HTTP-обработчика на пользовательский ввод.
func (r *Registry) Add(name string, w *Worker, start bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.items[name]; exists {
		return fmt.Errorf("output %q already exists", name)
	}

	e := &registryEntry{worker: w}
	r.items[name] = e
	if start {
		r.startLocked(e)
	}

	return nil
}

// Replace останавливает и убирает старый воркер под именем oldName (если он
// был) и ставит на его место w под именем newName (запуская его, если
// start) — используется при правке выхода: Worker неизменяем после создания
// (см. CLAUDE.md — задержка и остальные поля не меняются на лету), поэтому
// любая правка — это пересоздание воркера, а не изменение существующего.
// oldName != newName поддерживает переименование заодно со сменой полей.
func (r *Registry) Replace(oldName, newName string, w *Worker, start bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	old, ok := r.items[oldName]
	if !ok {
		return fmt.Errorf("unknown output %q", oldName)
	}

	if newName != oldName {
		if _, exists := r.items[newName]; exists {
			return fmt.Errorf("output %q already exists", newName)
		}
	}

	done := r.stopLocked(old)
	if done != nil {
		<-done
	}
	delete(r.items, oldName)

	e := &registryEntry{worker: w}
	r.items[newName] = e
	if start {
		r.startLocked(e)
	}

	return nil
}

// Remove останавливает (если запущен) и убирает выход name. Ошибка, если
// такого имени нет.
func (r *Registry) Remove(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.items[name]
	if !ok {
		return fmt.Errorf("unknown output %q", name)
	}

	done := r.stopLocked(e)
	if done != nil {
		<-done
	}
	delete(r.items, name)

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
