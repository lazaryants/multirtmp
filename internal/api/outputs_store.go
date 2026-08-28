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
type outputsStore struct {
	mu sync.Mutex

	cfgPath  string
	doc      *config.Doc
	registry *output.Registry
	buf      *buffer.Buffer
	logger   *slog.Logger
	notifier *notify.Notifier

	maxDelay atomic.Int64 // наносекунды; см. MaxOutputDelay
}

func newOutputsStore(cfgPath string, doc *config.Doc, registry *output.Registry, buf *buffer.Buffer, logger *slog.Logger, notifier *notify.Notifier) *outputsStore {
	s := &outputsStore{cfgPath: cfgPath, doc: doc, registry: registry, buf: buf, logger: logger, notifier: notifier}
	s.recomputeMaxDelayLocked()
	return s
}

// List отдаёт все выходы, отсортированные по имени.
func (s *outputsStore) List() []OutputView {
	s.mu.Lock()
	outs := append([]config.OutputConfig{}, s.doc.Outputs...)
	s.mu.Unlock()

	live := map[string]output.Status{}
	for _, st := range s.registry.Snapshot() {
		live[st.Name] = st
	}

	views := make([]OutputView, 0, len(outs))
	for _, o := range outs {
		views = append(views, viewFrom(o, live[o.Name]))
	}

	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })

	return views
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

// Add валидирует, персистит и запускает (если Enabled) новый выход.
func (s *outputsStore) Add(in OutputInput) (OutputView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing := map[string]bool{}
	for _, o := range s.doc.Outputs {
		existing[o.Name] = true
	}

	name := strings.TrimSpace(in.Name)
	delay, err := validateOutputInput(in, name, existing)
	if err != nil {
		return OutputView{}, err
	}

	entry := config.OutputConfig{Name: name, URL: strings.TrimSpace(in.URL), Key: in.Key, Delay: delay.String(), Enabled: boolPtr(in.Enabled)}

	if err := s.persistLocked(append(append([]config.OutputConfig{}, s.doc.Outputs...), entry)); err != nil {
		return OutputView{}, err
	}

	w := output.NewWorker(name, config.CombineURLKey(entry.URL, entry.Key), delay, s.buf, s.logger, s.notifier)
	if err := s.registry.Add(name, w, in.Enabled); err != nil {
		return OutputView{}, fmt.Errorf("output saved to config but failed to start: %w", err)
	}

	s.recomputeMaxDelayLocked()

	return s.viewLocked(name), nil
}

// Update заменяет выход oldName (переименование, если in.Name отличается).
// Worker неизменяем после создания — правка всегда означает "остановить
// старый, поставить новый", итоговое состояние (запущен/остановлен) после
// сохранения равно in.Enabled, независимо от того, что было раньше.
func (s *outputsStore) Update(oldName string, in OutputInput) (OutputView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx := s.indexOfLocked(oldName)
	if idx < 0 {
		return OutputView{}, &notFoundError{oldName}
	}

	existing := map[string]bool{}
	for i, o := range s.doc.Outputs {
		if i != idx {
			existing[o.Name] = true
		}
	}

	name := strings.TrimSpace(in.Name)
	delay, err := validateOutputInput(in, name, existing)
	if err != nil {
		return OutputView{}, err
	}

	entry := config.OutputConfig{Name: name, URL: strings.TrimSpace(in.URL), Key: in.Key, Delay: delay.String(), Enabled: boolPtr(in.Enabled)}

	newOutputs := append([]config.OutputConfig{}, s.doc.Outputs...)
	newOutputs[idx] = entry
	if err := s.persistLocked(newOutputs); err != nil {
		return OutputView{}, err
	}

	w := output.NewWorker(name, config.CombineURLKey(entry.URL, entry.Key), delay, s.buf, s.logger, s.notifier)
	if err := s.registry.Replace(oldName, name, w, in.Enabled); err != nil {
		return OutputView{}, fmt.Errorf("output saved to config but failed to restart: %w", err)
	}

	s.recomputeMaxDelayLocked()

	return s.viewLocked(name), nil
}

// Delete останавливает (если запущен) и убирает выход name.
func (s *outputsStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx := s.indexOfLocked(name)
	if idx < 0 {
		return &notFoundError{name}
	}

	newOutputs := append(append([]config.OutputConfig{}, s.doc.Outputs[:idx]...), s.doc.Outputs[idx+1:]...)
	if err := s.persistLocked(newOutputs); err != nil {
		return err
	}

	if err := s.registry.Remove(name); err != nil {
		return fmt.Errorf("output removed from config but failed to stop: %w", err)
	}

	s.recomputeMaxDelayLocked()

	return nil
}

// SetEnabled — то, что теперь стоит за POST /outputs/{name}/start|stop:
// решение оператора персистится в config.json (enabled), а не только меняет
// рантайм — иначе после рестарта службы или нового publish от vMix (который
// пересобирает буфер, но не троганные им выходы) всё возвращалось бы к
// enabled из файла, будто оператор ничего не нажимал. Автозапуск при
// появлении источника по-прежнему только для enabled:true — это решается
// самим фактом персиста, отдельной логики не требуется.
func (s *outputsStore) SetEnabled(name string, enabled bool) (OutputView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx := s.indexOfLocked(name)
	if idx < 0 {
		return OutputView{}, &notFoundError{name}
	}

	newOutputs := append([]config.OutputConfig{}, s.doc.Outputs...)
	newOutputs[idx].Enabled = boolPtr(enabled)
	if err := s.persistLocked(newOutputs); err != nil {
		return OutputView{}, err
	}

	var err error
	if enabled {
		err = s.registry.Start(name)
	} else {
		err = s.registry.Stop(name)
	}
	if err != nil {
		verb := "start"
		if !enabled {
			verb = "stop"
		}
		return OutputView{}, fmt.Errorf("output config updated but failed to %s: %w", verb, err)
	}

	return s.viewLocked(name), nil
}

// MaxOutputDelay — наибольшая задержка среди всех выходов прямо сейчас
// (включая выключенные), для автоподбора размера буфера (см.
// cmd/multirtmp/bufferauto.go) — читается на каждом тике автоподбора, поэтому
// атомик, а не под mu.
func (s *outputsStore) MaxOutputDelay() time.Duration {
	return time.Duration(s.maxDelay.Load())
}

func (s *outputsStore) indexOfLocked(name string) int {
	for i, o := range s.doc.Outputs {
		if o.Name == name {
			return i
		}
	}
	return -1
}

func (s *outputsStore) persistLocked(outputs []config.OutputConfig) error {
	doc := *s.doc
	doc.Outputs = outputs
	if err := doc.Save(s.cfgPath); err != nil {
		return err
	}
	s.doc = &doc
	return nil
}

func (s *outputsStore) recomputeMaxDelayLocked() {
	var max time.Duration
	for _, o := range s.doc.Outputs {
		if d := parseDelayOrZero(o.Delay); d > max {
			max = d
		}
	}
	s.maxDelay.Store(int64(max))
}

func (s *outputsStore) viewLocked(name string) OutputView {
	for _, o := range s.doc.Outputs {
		if o.Name == name {
			live := map[string]output.Status{}
			for _, st := range s.registry.Snapshot() {
				live[st.Name] = st
			}
			return viewFrom(o, live[name])
		}
	}
	return OutputView{}
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
