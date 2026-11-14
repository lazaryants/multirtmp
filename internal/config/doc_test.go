package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestDocRoundTripPreservesUnknownSections — репродукция бага с потерей
// секции preview: config.json с секцией, которую эта версия multirtmp не
// знает вовсе (custom_tool), и с секцией preview (которую знает, но раньше
// хранил именованным полем, а не как есть) должен пережить LoadDoc -> Save
// (эмулирует любую операцию через POST/PUT/DELETE /outputs или
// POST /outputs/{name}/start|stop) без потери ни одной из них — байт в байт
// по значению, не только "что-то похожее".
func TestDocRoundTripPreservesUnknownSections(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	original := `{
  "ingest": {
    "addr": "127.0.0.1:1935"
  },
  "preview": {
    "enabled": true,
    "url": "http://127.0.0.1:8889",
    "path": "preview"
  },
  "custom_tool": {
    "some_field": "some_value",
    "nested": {
      "a": 1,
      "b": [1, 2, 3]
    }
  },
  "outputs": [
    {
      "name": "corp",
      "url": "rtmp://127.0.0.1:19350/live",
      "key": "REPLACE_ME",
      "delay": "0s",
      "enabled": true
    }
  ]
}
`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	doc, err := LoadDoc(path)
	if err != nil {
		t.Fatalf("LoadDoc failed: %v", err)
	}

	// Симулирует то, что делает outputsStore при любой мутации выхода:
	// меняем только Outputs, ничего больше не трогаем.
	doc.Outputs = append(doc.Outputs, OutputConfig{
		Name: "youtube", URL: "rtmp://a.rtmp.youtube.com/live2", Key: "REPLACE_ME", Delay: "15s",
	})

	if err := doc.Save(path); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read saved config: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(saved, &got); err != nil {
		t.Fatalf("saved config is not valid JSON: %v\n%s", err, saved)
	}

	assertSectionEqual(t, got, "preview", `{
    "enabled": true,
    "url": "http://127.0.0.1:8889",
    "path": "preview"
  }`)
	assertSectionEqual(t, got, "custom_tool", `{
    "some_field": "some_value",
    "nested": {
      "a": 1,
      "b": [1, 2, 3]
    }
  }`)
	assertSectionEqual(t, got, "ingest", `{
    "addr": "127.0.0.1:1935"
  }`)

	if _, ok := got["outputs"]; !ok {
		t.Fatalf("outputs section missing from saved config")
	}
	var outputs []OutputConfig
	if err := json.Unmarshal(got["outputs"], &outputs); err != nil {
		t.Fatalf("outputs section is not valid: %v", err)
	}
	if len(outputs) != 2 {
		t.Fatalf("expected 2 outputs (original + added), got %d: %+v", len(outputs), outputs)
	}
	if outputs[0].Name != "corp" || outputs[1].Name != "youtube" {
		t.Fatalf("unexpected outputs order/content: %+v", outputs)
	}

	// LoadDoc должен читать это же сохранённое значение обратно так же
	// корректно (round-trip не только "запись валидна", а "запись понятна
	// нашему же загрузчику").
	reloaded, err := LoadDoc(path)
	if err != nil {
		t.Fatalf("LoadDoc on saved file failed: %v", err)
	}
	if len(reloaded.Outputs) != 2 {
		t.Fatalf("reloaded doc has %d outputs, want 2", len(reloaded.Outputs))
	}
	if _, ok := reloaded.extra["preview"]; !ok {
		t.Fatalf("reloaded doc lost the preview section")
	}
	if _, ok := reloaded.extra["custom_tool"]; !ok {
		t.Fatalf("reloaded doc lost the unknown custom_tool section")
	}
}

// assertSectionEqual сравнивает секцию got[key] с wantJSON СЕМАНТИЧЕСКИ (как
// разобранные значения), а не байт в байт по тексту — Save переформатирует
// отступы, это ожидаемо и не является потерей данных. Данные, а не
// форматирование, обязаны совпадать в точности.
func assertSectionEqual(t *testing.T, got map[string]json.RawMessage, key, wantJSON string) {
	t.Helper()

	raw, ok := got[key]
	if !ok {
		t.Fatalf("section %q missing from saved config entirely", key)
	}

	var gotVal, wantVal any
	if err := json.Unmarshal(raw, &gotVal); err != nil {
		t.Fatalf("section %q in saved config is not valid JSON: %v", key, err)
	}
	if err := json.Unmarshal([]byte(wantJSON), &wantVal); err != nil {
		t.Fatalf("test bug: wantJSON for %q is not valid JSON: %v", key, err)
	}

	if !reflect.DeepEqual(gotVal, wantVal) {
		t.Fatalf("section %q changed across LoadDoc->Save:\n got:  %s\n want: %s", key, raw, wantJSON)
	}
}

// TestDocPreservesSectionWithNoOutputsChange — конфиг без единого выхода
// (или без секции outputs вовсе) не должен ронять Save и не должен трогать
// остальные секции.
func TestDocPreservesSectionWithNoOutputsChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	original := `{
  "notify": {
    "telegram_bot_token": "",
    "telegram_chat_id": ""
  }
}
`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	doc, err := LoadDoc(path)
	if err != nil {
		t.Fatalf("LoadDoc failed: %v", err)
	}
	if len(doc.Outputs) != 0 {
		t.Fatalf("expected no outputs, got %+v", doc.Outputs)
	}

	if err := doc.Save(path); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read saved config: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(saved, &got); err != nil {
		t.Fatalf("saved config is not valid JSON: %v", err)
	}
	assertSectionEqual(t, got, "notify", `{"telegram_bot_token": "", "telegram_chat_id": ""}`)
}

func TestValidatePublicHost(t *testing.T) {
	ok := []string{"192.168.1.12", "studio-pc", "studio-pc.local", "fd00::5", "[fd00::5]"}
	for _, h := range ok {
		if err := validatePublicHost(h); err != nil {
			t.Errorf("%q: unexpected error %v", h, err)
		}
	}
	bad := []string{"http://192.168.1.12", "192.168.1.12:8080", "studio-pc/", "a b", "[fd00::5]:8080"}
	for _, h := range bad {
		if err := validatePublicHost(h); err == nil {
			t.Errorf("%q: expected an error", h)
		}
	}
}
