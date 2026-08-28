package api

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"multirtmp/internal/buffer"
	"multirtmp/internal/config"
	"multirtmp/internal/notify"
	"multirtmp/internal/output"
)

// OutputView — один выход для GET /outputs: конфиг (url/key/delay/enabled —
// то, что хранится в config.json) склеенный по имени с текущим рантайм-
// состоянием из Registry (state/last_error/... — то же, что и в GET
// /status, только тут ещё и полный url/key для формы редактирования: они и
// так лежат в config.json открытым текстом, маскировка нужна только в
// логах/алертах).
type OutputView struct {
	Name       string       `json:"name"`
	URL        string       `json:"url"`
	Key        string       `json:"key"`
	Delay      string       `json:"delay"`
	Enabled    bool         `json:"enabled"`
	State      output.State `json:"state"`
	KBPS       int64        `json:"kbps"`
	LagS       float64      `json:"lag_s"`
	Reconnects int64        `json:"reconnects"`
	LastError  string       `json:"last_error"`
	UptimeS    float64      `json:"uptime_s"`
}

// OutputInput — тело POST/PUT /outputs.
type OutputInput struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Key     string `json:"key"`
	Delay   string `json:"delay"`
	Enabled bool   `json:"enabled"`
}

type notFoundError struct{ name string }

func (e *notFoundError) Error() string { return fmt.Sprintf("unknown output %q", e.name) }

type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }

// outputsStore — единственная точка мутации выходов через HTTP: валидация ->
// запись в config.json -> и только потом изменение живого Registry. Такой
// порядок (не наоборот) на случай сбоя записи на диск: если Save упал,
// рантайм остаётся ровно таким же, каким был, конфиг на диске тоже не
// тронут — ничего не расходится.
//
// НИКАКОГО закешированного *config.Doc между вызовами — это было причиной
// реальной потери пользовательских данных: если между двумя HTTP-запросами
// оператор (или что угодно ещё) поправил config.json вручную, закешированная
// версия ничего об этом не знает, и следующая же запись (даже по совершенно
// не связанной причине — например, обычный клик Start/Stop) стирает ручную
// правку, потому что пишет то, что было на диске в момент СТАРТА ПРОЦЕССА, а
// не то, что там сейчас. Каждый метод здесь перечитывает config.json заново
// прямо перед тем, как что-то в нём менять — единственный способ гарантировать,
// что запись не отменит правку, сделанную снаружи между вызовами.
type outputsStore struct {
	mu sync.Mutex

	cfgPath  string
	registry *output.Registry
	buf      *buffer.Buffer
	logger   *slog.Logger
	notifier *notify.Notifier

	maxDelay atomic.Int64 // наносекунды; см. MaxOutputDelay
}

func newOutputsStore(cfgPath string, registry *output.Registry, buf *buffer.Buffer, logger *slog.Logger, notifier *notify.Notifier) (*outputsStore, error) {
	s := &outputsStore{cfgPath: cfgPath, registry: registry, buf: buf, logger: logger, notifier: notifier}

	doc, err := config.LoadDoc(cfgPath)
	if err != nil {
		return nil, err
	}
	s.recomputeMaxDelay(doc.Outputs)

	return s, nil
}

// List отдаёт все выходы, отсортированные по имени. Не под s.mu — это
// чистое чтение (свежий LoadDoc + Registry.Snapshot), ему нечего сериализовать
// с мутациями других методов.
func (s *outputsStore) List() ([]OutputView, error) {
	doc, err := config.LoadDoc(s.cfgPath)
	if err != nil {
		return nil, err
	}

	live := map[string]output.Status{}
	for _, st := range s.registry.Snapshot() {
		live[st.Name] = st
	}

	views := make([]OutputView, 0, len(doc.Outputs))
	for _, o := range doc.Outputs {
		views = append(views, viewFrom(o, live[o.Name]))
	}

	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })

	return views, nil
}

func viewFrom(o config.OutputConfig, st output.Status) OutputView {
	delay := o.Delay
	if delay == "" {
		delay = "0s"
	}

	v := OutputView{
		Name:    o.Name,
		URL:     o.URL,
		Key:     o.Key,
		Delay:   delay,
		Enabled: o.Enabled == nil || *o.Enabled,
		State:   output.StateStopped,
	}

	if st.Name != "" {
		v.State = st.State
		v.KBPS = st.KBPS
		v.LagS = st.LagS
		v.Reconnects = st.Reconnects
		v.LastError = st.LastError
		v.UptimeS = st.UptimeS
	}

	return v
}

// viewFromOutputs строит OutputView из уже имеющегося на руках outputs (то,
// что только что записали) плюс свежий снимок Registry — используется сразу
// после Add/Update, чтобы не делать ещё один LoadDoc ради одного элемента,
// который мы и так только что сформировали.
func viewFromOutputs(name string, outputs []config.OutputConfig, reg *output.Registry) OutputView {
	live := map[string]output.Status{}
	for _, st := range reg.Snapshot() {
		live[st.Name] = st
	}
	for _, o := range outputs {
		if o.Name == name {
			return viewFrom(o, live[name])
		}
	}
	return OutputView{}
}

// Add валидирует, персистит и запускает (если Enabled) новый выход.
func (s *outputsStore) Add(in OutputInput) (OutputView, error) {
	s.mu.Lock()

	doc, err := config.LoadDoc(s.cfgPath)
	if err != nil {
		s.mu.Unlock()
		return OutputView{}, fmt.Errorf("failed to reload config: %w", err)
	}

	existing := map[string]bool{}
	for _, o := range doc.Outputs {
		existing[o.Name] = true
	}

	name := strings.TrimSpace(in.Name)
	delay, err := validateOutputInput(in, name, existing)
	if err != nil {
		s.mu.Unlock()
		return OutputView{}, err
	}

	entry := config.OutputConfig{Name: name, URL: strings.TrimSpace(in.URL), Key: in.Key, Delay: delay.String(), Enabled: boolPtr(in.Enabled)}
	newOutputs := append(append([]config.OutputConfig{}, doc.Outputs...), entry)

	if err := s.persistLocked(doc, newOutputs); err != nil {
		s.mu.Unlock()
		return OutputView{}, err
	}
	s.recomputeMaxDelay(newOutputs)
	s.mu.Unlock()

	w := output.NewWorker(name, config.CombineURLKey(entry.URL, entry.Key), delay, s.buf, s.logger, s.notifier)
	if err := s.registry.Add(name, w, in.Enabled); err != nil {
		return OutputView{}, fmt.Errorf("output saved to config but failed to start: %w", err)
	}

	return viewFromOutputs(name, newOutputs, s.registry), nil
}

// Update заменяет выход oldName (переименование, если in.Name отличается).
// Worker неизменяем после создания — правка всегда означает "остановить
// старый, поставить новый", итоговое состояние (запущен/остановлен) после
// сохранения равно in.Enabled, независимо от того, что было раньше.
//
// registry.Replace вызывается БЕЗ удержания s.mu (как и в SetEnabled) — он
// может ждать реального завершения горутины старого воркера, а s.mu общий
// для всех выходов; удержи его на это время, и один медленно
// останавливающийся выход заморозил бы add/edit/delete/start/stop ЛЮБОГО
// другого выхода.
func (s *outputsStore) Update(oldName string, in OutputInput) (OutputView, error) {
	s.mu.Lock()

	doc, err := config.LoadDoc(s.cfgPath)
	if err != nil {
		s.mu.Unlock()
		return OutputView{}, fmt.Errorf("failed to reload config: %w", err)
	}

	idx := indexOf(doc.Outputs, oldName)
	if idx < 0 {
		s.mu.Unlock()
		return OutputView{}, &notFoundError{oldName}
	}

	existing := map[string]bool{}
	for i, o := range doc.Outputs {
		if i != idx {
			existing[o.Name] = true
		}
	}

	name := strings.TrimSpace(in.Name)
	delay, err := validateOutputInput(in, name, existing)
	if err != nil {
		s.mu.Unlock()
		return OutputView{}, err
	}

	entry := config.OutputConfig{Name: name, URL: strings.TrimSpace(in.URL), Key: in.Key, Delay: delay.String(), Enabled: boolPtr(in.Enabled)}

	newOutputs := append([]config.OutputConfig{}, doc.Outputs...)
	newOutputs[idx] = entry
	if err := s.persistLocked(doc, newOutputs); err != nil {
		s.mu.Unlock()
		return OutputView{}, err
	}
	s.recomputeMaxDelay(newOutputs)
	s.mu.Unlock()

	w := output.NewWorker(name, config.CombineURLKey(entry.URL, entry.Key), delay, s.buf, s.logger, s.notifier)
	if err := s.registry.Replace(oldName, name, w, in.Enabled); err != nil {
		return OutputView{}, fmt.Errorf("output saved to config but failed to restart: %w", err)
	}

	return viewFromOutputs(name, newOutputs, s.registry), nil
}

// Delete останавливает (если запущен) и убирает выход name. registry.Remove
// тоже вызывается вне s.mu — см. комментарий у Update.
func (s *outputsStore) Delete(name string) error {
	s.mu.Lock()

	doc, err := config.LoadDoc(s.cfgPath)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("failed to reload config: %w", err)
	}

	idx := indexOf(doc.Outputs, name)
	if idx < 0 {
		s.mu.Unlock()
		return &notFoundError{name}
	}

	newOutputs := append(append([]config.OutputConfig{}, doc.Outputs[:idx]...), doc.Outputs[idx+1:]...)
	if err := s.persistLocked(doc, newOutputs); err != nil {
		s.mu.Unlock()
		return err
	}
	s.recomputeMaxDelay(newOutputs)
	s.mu.Unlock()

	if err := s.registry.Remove(name); err != nil {
		return fmt.Errorf("output removed from config but failed to stop: %w", err)
	}

	return nil
}

// SetEnabled — то, что стоит за POST /outputs/{name}/start|stop: решение
// оператора персистится в config.json (enabled), а не только меняет рантайм
// — иначе после рестарта службы или нового publish от vMix всё возвращалось
// бы к enabled из файла, будто оператор ничего не нажимал. Автозапуск при
// появлении источника по-прежнему только для enabled:true — это решается
// самим фактом персиста, отдельной логики не требуется.
//
// registry.Start/Stop вызываются БЕЗ удержания s.mu — см. Update. Отдельно:
// если registry-вызов вернёт ConflictError (уже запущен/уже остановлен),
// config.json не читается и не трогается вовсе — нет смысла тратить чтение
// и запись на команду, которая не изменит состояние.
func (s *outputsStore) SetEnabled(name string, enabled bool) (OutputView, error) {
	// Быстрая предварительная проверка существования — не под локом, просто
	// чтобы не запускать registry-операцию для явно несуществующего имени;
	// не обязана быть безупречно консистентной (реальная проверка — ниже,
	// под локом, прямо перед записью), поэтому ошибку самого LoadDoc здесь
	// просто игнорируем и даём дойти до registry.Start/Stop, у которого есть
	// собственная проверка "нет такого выхода".
	if doc, err := config.LoadDoc(s.cfgPath); err == nil {
		if indexOf(doc.Outputs, name) < 0 {
			return OutputView{}, &notFoundError{name}
		}
	}

	var opErr error
	if enabled {
		opErr = s.registry.Start(name)
	} else {
		opErr = s.registry.Stop(name)
	}
	if opErr != nil {
		return OutputView{}, opErr
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Перечитываем СЕЙЧАС, а не полагаемся на то, что видели в
	// предварительной проверке выше — пока registry.Start/Stop выполнялся,
	// конфиг на диске мог измениться (в т.ч. и этот самый выход мог пропасть
	// через конкурентный Delete).
	doc, err := config.LoadDoc(s.cfgPath)
	if err != nil {
		return OutputView{}, fmt.Errorf("output %s but config could not be reloaded to save it: %w",
			map[bool]string{true: "started", false: "stopped"}[enabled], err)
	}
	idx := indexOf(doc.Outputs, name)
	if idx < 0 {
		return OutputView{}, &notFoundError{name}
	}

	newOutputs := append([]config.OutputConfig{}, doc.Outputs...)
	newOutputs[idx].Enabled = boolPtr(enabled)
	if err := s.persistLocked(doc, newOutputs); err != nil {
		return OutputView{}, err
	}
	s.recomputeMaxDelay(newOutputs)

	return viewFromOutputs(name, newOutputs, s.registry), nil
}

// MaxOutputDelay — наибольшая задержка среди всех выходов, какой она была в
// момент последней мутации (Add/Update/Delete/SetEnabled) или старта
// процесса — для автоподбора размера буфера (см. cmd/multirtmp/bufferauto.go),
// читается на каждом тике автоподбора, поэтому атомик. Не пересчитывается на
// правки config.json вручную мимо API между мутациями — тот же компромисс,
// что и у остального автоподбора буфера: он реагирует на то, что происходит
// через API, а не полит файл на диске постоянно.
func (s *outputsStore) MaxOutputDelay() time.Duration {
	return time.Duration(s.maxDelay.Load())
}

func indexOf(outputs []config.OutputConfig, name string) int {
	for i, o := range outputs {
		if o.Name == name {
			return i
		}
	}
	return -1
}

// persistLocked дописывает outputs в doc (уже свежепрочитанный вызывающим) и
// сохраняет. Вызывающий обязан держать s.mu на всё время между LoadDoc и
// этим вызовом — иначе именно это окно и открывает гонку "прочитали одно,
// записали поверх другого", то есть саму дыру, которую эта переработка
// закрывает.
func (s *outputsStore) persistLocked(doc *config.Doc, outputs []config.OutputConfig) error {
	doc.Outputs = outputs
	return doc.Save(s.cfgPath)
}

func (s *outputsStore) recomputeMaxDelay(outputs []config.OutputConfig) {
	var max time.Duration
	for _, o := range outputs {
		if d := parseDelayOrZero(o.Delay); d > max {
			max = d
		}
	}
	s.maxDelay.Store(int64(max))
}

// validateOutputInput проверяет name/url/key/delay в in. name — уже
// TrimSpace'нутое имя (переданное отдельно, чтобы Add/Update не считали его
// дважды). existing — имена остальных выходов (для Update — без самого
// правящегося). Возвращает разобранный delay при успехе.
func validateOutputInput(in OutputInput, name string, existing map[string]bool) (time.Duration, error) {
	var problems []string

	if name == "" {
		problems = append(problems, "name: required")
	} else if existing[name] {
		problems = append(problems, fmt.Sprintf("name: %q already exists", name))
	}

	if strings.TrimSpace(in.URL) == "" {
		problems = append(problems, "url: required")
	}
	if in.Key == "" {
		problems = append(problems, "key: required")
	}

	delayStr := in.Delay
	if delayStr == "" {
		delayStr = "0s"
	}
	delay, err := time.ParseDuration(delayStr)
	switch {
	case err != nil:
		problems = append(problems, fmt.Sprintf("delay: invalid duration %q: %s", in.Delay, err))
	case delay < 0:
		problems = append(problems, "delay: must not be negative")
	}

	if len(problems) > 0 {
		return 0, &validationError{msg: "invalid output: " + strings.Join(problems, "; ")}
	}

	return delay, nil
}

func parseDelayOrZero(s string) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}

func boolPtr(b bool) *bool { return &b }
