package output

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
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

// opState — операция жизненного цикла выхода, которая сейчас выполняется
// (если выполняется) — отдельно от Worker.status.state (что делает уже
// запущенный воркер: waiting_for_source/connecting/streaming). Пока op !=
// opIdle, Snapshot показывает его вместо обычного состояния — оператору
// нужно видеть "starting"/"stopping", а не гадать по таймауту, дошёл ли клик
// вообще до сервера (см. правку после отчёта про незащищённые повторные
// клики Start/Stop/Edit/Delete).
type opState int32

const (
	opIdle opState = iota
	opStarting
	opStopping
)

type registryEntry struct {
	// opMu сериализует Start/Stop/Replace/Remove для ЭТОГО конкретного
	// выхода — пока один из них выполняется (включая потенциально
	// многосекундное ожидание в Stop, см. stopLocked), следующий блокируется
	// здесь, а не молча стартует второе подключение поверх первого. Выходы
	// друг друга не блокируют — у каждого свой opMu, только Registry.mu
	// (карта целиком) держится кратко, как и раньше.
	opMu sync.Mutex

	worker *Worker
	cancel context.CancelFunc // nil, если не запущен; под Registry.mu
	done   chan struct{}      // закрывается, когда Run() вернулась; nil, если не запущен; под Registry.mu

	// op — атомик, не под opMu/Registry.mu: Snapshot читает его без
	// блокировки на каждый запрос статуса, а меняется он только тем, кто уже
	// держит opMu этого выхода.
	op atomic.Int32
}

// ConflictError — общий маркер для "команда не имеет смысла в текущем
// состоянии" (start уже запущенного, stop уже остановленного) — HTTP-слой
// (internal/api) превращает его в 409, а не в 404/500.
type ConflictError interface {
	error
	outputConflict()
}

type alreadyRunningError struct{ name string }

func (e *alreadyRunningError) Error() string {
	return fmt.Sprintf("output %q is already running", e.name)
}
func (e *alreadyRunningError) outputConflict() {}

type alreadyStoppedError struct{ name string }

func (e *alreadyStoppedError) Error() string {
	return fmt.Sprintf("output %q is already stopped", e.name)
}
func (e *alreadyStoppedError) outputConflict() {}

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

// lockEntry находит entry по имени и захватывает его opMu — единая точка
// входа для Start/Stop/Replace/Remove. После захвата ПЕРЕПРОВЕРЯЕТ, что это
// всё ещё текущая запись под этим именем: пока мы ждали opMu, Replace/Remove
// мог успеть убрать её из карты и завести другую (например, переименование
// through PUT) — без этой проверки можно было бы наткнуться на призрак:
// запустить воркер, который уже нигде не зарегистрирован и которого никто
// потом не остановит. Возвращает entry и функцию разблокировки opMu — вызов
// обязателен (defer) в каждом месте использования.
func (r *Registry) lockEntry(name string) (*registryEntry, func(), error) {
	r.mu.Lock()
	e, ok := r.items[name]
	r.mu.Unlock()
	if !ok {
		return nil, nil, fmt.Errorf("unknown output %q", name)
	}

	e.opMu.Lock()

	r.mu.Lock()
	current, stillThere := r.items[name]
	r.mu.Unlock()
	if !stillThere || current != e {
		e.opMu.Unlock()
		return nil, nil, fmt.Errorf("unknown output %q", name)
	}

	return e, e.opMu.Unlock, nil
}

// Start запускает выход name. Ошибка ConflictError (alreadyRunningError),
// если он уже запущен — вызывающий (HTTP-слой) обязан довести это до
// оператора как 409, а не молча сделать вид, что всё в порядке: именно
// молчаливый повторный Start и был риском случайного второго подключения к
// назначению при повторном клике.
func (r *Registry) Start(name string) error {
	e, unlock, err := r.lockEntry(name)
	if err != nil {
		return err
	}
	defer unlock()

	r.mu.Lock()
	running := e.cancel != nil
	r.mu.Unlock()
	if running {
		return &alreadyRunningError{name: name}
	}

	e.op.Store(int32(opStarting))
	defer e.op.Store(int32(opIdle))

	r.mu.Lock()
	r.startLocked(e)
	r.mu.Unlock()

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

// Stop останавливает выход name и дожидается фактического завершения его
// горутины (Run реагирует на отмену ctx быстро — см. Cursor.Next(ctx) —
// кроме случая зависшей записи в сокет назначения, см. SetWriteDeadline в
// internal/output/conn.go: тогда Stop может занять до этого дедлайна).
// Ошибка ConflictError (alreadyStoppedError), если уже остановлен — та же
// причина, что и у Start: явный 409 вместо тихого "ничего не произошло".
func (r *Registry) Stop(name string) error {
	e, unlock, err := r.lockEntry(name)
	if err != nil {
		return err
	}
	defer unlock()

	r.mu.Lock()
	running := e.cancel != nil
	r.mu.Unlock()
	if !running {
		return &alreadyStoppedError{name: name}
	}

	e.op.Store(int32(opStopping))
	defer e.op.Store(int32(opIdle))

	r.mu.Lock()
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
	if _, exists := r.items[name]; exists {
		r.mu.Unlock()
		return fmt.Errorf("output %q already exists", name)
	}
	e := &registryEntry{worker: w}
	r.items[name] = e
	r.mu.Unlock()

	if start {
		e.op.Store(int32(opStarting))
		r.mu.Lock()
		r.startLocked(e)
		r.mu.Unlock()
		e.op.Store(int32(opIdle))
	}

	return nil
}

// Replace останавливает и убирает старый воркер под именем oldName и ставит
// на его место w под именем newName (запуская его, если start) —
// используется при правке выхода: Worker неизменяем после создания (см.
// CLAUDE.md — задержка и остальные поля не меняются на лету), поэтому любая
// правка — это пересоздание воркера, а не изменение существующего. oldName
// != newName поддерживает переименование заодно со сменой полей.
func (r *Registry) Replace(oldName, newName string, w *Worker, start bool) error {
	old, unlock, err := r.lockEntry(oldName)
	if err != nil {
		return err
	}
	defer unlock()

	if newName != oldName {
		r.mu.Lock()
		_, exists := r.items[newName]
		r.mu.Unlock()
		if exists {
			return fmt.Errorf("output %q already exists", newName)
		}
	}

	old.op.Store(int32(opStopping))
	r.mu.Lock()
	done := r.stopLocked(old)
	r.mu.Unlock()
	if done != nil {
		<-done
	}
	old.op.Store(int32(opIdle))

	e := &registryEntry{worker: w}
	r.mu.Lock()
	delete(r.items, oldName)
	r.items[newName] = e
	r.mu.Unlock()

	if start {
		e.op.Store(int32(opStarting))
		r.mu.Lock()
		r.startLocked(e)
		r.mu.Unlock()
		e.op.Store(int32(opIdle))
	}

	return nil
}

// Remove останавливает (если запущен) и убирает выход name.
func (r *Registry) Remove(name string) error {
	e, unlock, err := r.lockEntry(name)
	if err != nil {
		return err
	}
	defer unlock()

	e.op.Store(int32(opStopping))
	r.mu.Lock()
	done := r.stopLocked(e)
	r.mu.Unlock()
	if done != nil {
		<-done
	}
	e.op.Store(int32(opIdle))

	r.mu.Lock()
	delete(r.items, name)
	r.mu.Unlock()

	return nil
}

// StartAll запускает все зарегистрированные выходы, для которых names
// содержит их имя (обычно — те, у кого enabled=true в конфиге). Вызывается
// один раз при старте процесса, до того как заведомо ничего не запущено —
// ConflictError здесь невозможен, поэтому ошибка осознанно отбрасывается.
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
// отсортированное по имени. Пока идёт операция Start/Stop/Replace/Remove над
// выходом, его State — StateStarting/StateStopping независимо от того, что
// сообщает сам воркер (см. opState) — это то, что видит оператор в /status,
// пока клик обрабатывается. Не запущенные и без операции в процессе
// показывают StateStopped независимо от того, что было в них последним,
// пока работали.
func (r *Registry) Snapshot() []Status {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]Status, 0, len(r.items))
	for _, e := range r.items {
		st := e.worker.Status()
		switch opState(e.op.Load()) {
		case opStarting:
			st.State = StateStarting
		case opStopping:
			st.State = StateStopping
		default:
			if e.cancel == nil {
				st.State = StateStopped
			}
		}
		out = append(out, st)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out
}
