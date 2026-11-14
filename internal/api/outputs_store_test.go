package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"multirtmp/internal/buffer"
	"multirtmp/internal/notify"
	"multirtmp/internal/output"
)

func newTestStore(t *testing.T, cfgPath string) *outputsStore {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := output.NewRegistry(ctx)
	buf := buffer.New(1024 * 1024)
	notifier := notify.New(notify.Config{}, logger)

	s, err := newOutputsStore(cfgPath, registry, buf, logger, notifier)
	if err != nil {
		t.Fatalf("newOutputsStore failed: %v", err)
	}

	return s
}

func readRawSection(t *testing.T, path, key string) (json.RawMessage, bool) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}

	v, ok := m[key]
	return v, ok
}

// TestOutputsStoreSurvivesOutOfBandEdit — репродукция самого бага: конфиг
// содержит секцию preview; ПОСЛЕ того, как процесс её один раз прочитал (при
// старте), но ДО следующего API-вызова, кто-то (оператор руками, другой
// процесс) правит файл на диске напрямую. Раньше outputsStore кешировал
// *config.Doc с самого первого чтения и каждая следующая запись (даже по
// совершенно не связанной причине — например, Add нового выхода) переписывала
// файл версией "как было при старте процесса", стирая правку. Теперь каждая
// операция перечитывает файл заново прямо перед записью — правка обязана
// пережить любую последующую мутацию.
func TestOutputsStoreSurvivesOutOfBandEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	initial := `{
  "ingest": { "addr": "127.0.0.1:1935" },
  "outputs": [
    { "name": "corp", "url": "rtmp://127.0.0.1:19350/live", "key": "REPLACE_ME", "delay": "0s", "enabled": true }
  ]
}
`
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatalf("failed to write initial config: %v", err)
	}

	// Процесс "стартует": store читает файл один раз при конструировании,
	// точно как это делает newOutputsStore в реальном запуске.
	s := newTestStore(t, path)

	// Кто-то правит файл вручную, ПОКА процесс уже работает — добавляет
	// preview, которого store никогда не видел.
	edited := `{
  "ingest": { "addr": "127.0.0.1:1935" },
  "preview": { "enabled": true, "url": "http://127.0.0.1:8889", "path": "preview" },
  "outputs": [
    { "name": "corp", "url": "rtmp://127.0.0.1:19350/live", "key": "REPLACE_ME", "delay": "0s", "enabled": true }
  ]
}
`
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatalf("failed to write out-of-band edit: %v", err)
	}

	// Оператор кликает что-то в интерфейсе — совершенно не связанное с
	// preview (добавляет второй выход). Раньше именно это стирало правку выше.
	if _, err := s.Add(OutputInput{Name: "youtube", URL: "rtmp://a.rtmp.youtube.com/live2", Key: "abcd-1234-efgh", Delay: "15s", Enabled: false}); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	raw, ok := readRawSection(t, path, "preview")
	if !ok {
		t.Fatalf("preview section is GONE after an unrelated Add — the out-of-band edit was silently discarded")
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("preview section is not valid JSON: %v", err)
	}
	if got["url"] != "http://127.0.0.1:8889" || got["path"] != "preview" || got["enabled"] != true {
		t.Fatalf("preview section survived but was corrupted: %+v", got)
	}

	// And the actual mutation should have gone through correctly too.
	outputsRaw, ok := readRawSection(t, path, "outputs")
	if !ok {
		t.Fatalf("outputs section missing")
	}
	var outputs []map[string]any
	if err := json.Unmarshal(outputsRaw, &outputs); err != nil {
		t.Fatalf("outputs section invalid: %v", err)
	}
	if len(outputs) != 2 {
		t.Fatalf("expected 2 outputs after Add, got %d: %+v", len(outputs), outputs)
	}
}

// TestOutputsStoreNeverWritesWithoutAMutation — конструирование стора (то,
// что происходит при каждом старте службы) само по себе не должно трогать
// config.json. Молчаливая перезапись файла при старте, даже если оператор
// ничего не менял через API, недопустима.
func TestOutputsStoreNeverWritesWithoutAMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	initial := `{
  "ingest": { "addr": "127.0.0.1:1935" },
  "outputs": [
    { "name": "corp", "url": "rtmp://127.0.0.1:19350/live", "key": "REPLACE_ME", "delay": "0s", "enabled": true }
  ]
}
`
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatalf("failed to write initial config: %v", err)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read config before: %v", err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("failed to stat config before: %v", err)
	}

	s := newTestStore(t, path)

	// A read-only call (what a passive /outputs poll does) must not write
	// either.
	if _, err := s.List(); err != nil {
		t.Fatalf("List failed: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read config after: %v", err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("failed to stat config after: %v", err)
	}

	if string(before) != string(after) {
		t.Fatalf("config.json content changed without any API mutation:\nbefore: %s\nafter:  %s", before, after)
	}
	if !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatalf("config.json mtime changed (%v -> %v) without any API mutation — it was rewritten on disk even though content happens to match",
			beforeInfo.ModTime(), afterInfo.ModTime())
	}

	// No .tmp file should have been left behind either (Doc.Save writes one
	// transiently, but only when it actually runs).
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("a .tmp file exists even though nothing was saved: %v", err)
	}
}

// Через API выход сохраняется только настроенным: пустой ключ и плейсхолдер
// REPLACE_ME — ошибка ввода (ненастроенные выходы появляются только из
// config.json, их создаёт install).
func TestValidateOutputInputRejectsPlaceholders(t *testing.T) {
	cases := []struct {
		name string
		in   OutputInput
		ok   bool
	}{
		{"real", OutputInput{URL: "rtmp://a.rtmp.youtube.com/live2", Key: "abcd-1234", Delay: "15s"}, true},
		{"empty key", OutputInput{URL: "rtmp://a.rtmp.youtube.com/live2", Key: ""}, false},
		{"placeholder key", OutputInput{URL: "rtmp://a.rtmp.youtube.com/live2", Key: "REPLACE_ME"}, false},
		{"placeholder key lowercase", OutputInput{URL: "rtmp://a.rtmp.youtube.com/live2", Key: " replace_me "}, false},
		{"empty url", OutputInput{URL: "  ", Key: "abcd"}, false},
	}
	for _, tc := range cases {
		_, err := validateOutputInput(tc.in, "youtube", map[string]bool{})
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

// Ненастроенный выход из config.json: в списке — not_configured, Start —
// 409-конфликт, а config.json при этом не переписывается.
func TestNotConfiguredOutputCannotStart(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	raw := `{"outputs": [{"name": "youtube", "url": "rtmp://a.rtmp.youtube.com/live2", "key": "", "delay": "15s", "enabled": false}]}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t, cfgPath)
	s.registry.Register("youtube", nil)

	views, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].State != output.StateNotConfigured {
		t.Fatalf("views = %+v", views)
	}

	_, err = s.SetEnabled("youtube", true)
	var ce output.ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("SetEnabled(true): got %v, want a ConflictError", err)
	}

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != raw {
		t.Fatalf("config.json was rewritten on a refused start:\n%s", after)
	}
}
