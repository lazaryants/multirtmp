package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Конфиги v1.0.0 (на площадках стоит она) должны загружаться текущим кодом
// без правок. testdata/v1.0.0-sample.json — образец конфига дословно из тега
// v1.0.0 (git show v1.0.0:internal/config/sample.go); v1.0.0-site.json — он
// же, как его заполняют на площадке: API на 127.0.0.1 без токена, ключи
// вписаны, превью включено со старым preview.url и выход превью на
// MediaMTX.

func TestLoadV100Sample(t *testing.T) {
	res, err := Load(filepath.Join("testdata", "v1.0.0-sample.json"))
	if err != nil {
		t.Fatalf("v1.0.0 sample does not load: %v", err)
	}
	if res.APIAddr != "127.0.0.1:8080" || res.IngestAddr != "127.0.0.1:1935" {
		t.Errorf("api %q ingest %q", res.APIAddr, res.IngestAddr)
	}
	// Ключи REPLACE_ME — теперь "не настроено", а не ошибка и не бесконечные
	// реконнекты.
	for _, o := range res.Outputs {
		if o.Configured {
			t.Errorf("%s: REPLACE_ME output must be not configured", o.Name)
		}
	}
	if res.PreviewEnabled {
		t.Error("preview must stay disabled as in the sample")
	}
}

func TestLoadV100SiteConfig(t *testing.T) {
	res, err := Load(filepath.Join("testdata", "v1.0.0-site.json"))
	if err != nil {
		t.Fatalf("v1.0.0 site config does not load: %v", err)
	}

	if res.APIAddr != "127.0.0.1:8080" || res.APIToken != "" {
		t.Errorf("api = %q token %q, want 127.0.0.1:8080 without token", res.APIAddr, res.APIToken)
	}
	if !IsLoopbackAddr(res.APIAddr) {
		t.Error("api must be loopback (no token required)")
	}

	if !res.PreviewEnabled || res.PreviewBaseURL != "http://127.0.0.1:8889" || res.PreviewPath != "live/preview" {
		t.Errorf("preview = %v %q %q", res.PreviewEnabled, res.PreviewBaseURL, res.PreviewPath)
	}
	if res.PreviewMediaMTXExe != "" {
		t.Errorf("mediamtx_exe = %q, want default (empty)", res.PreviewMediaMTXExe)
	}

	want := map[string]struct {
		url   string
		delay time.Duration
	}{
		"corp":    {"rtmp://10.1.2.3/live/corp-key-123", 0},
		"youtube": {"rtmp://a.rtmp.youtube.com/live2/abcd-efgh-ijkl-mnop", 15 * time.Second},
		"preview": {"rtmp://127.0.0.1:19350/live/preview", 0},
	}
	if len(res.Outputs) != len(want) {
		t.Fatalf("outputs = %+v", res.Outputs)
	}
	for _, o := range res.Outputs {
		w := want[o.Name]
		if !o.Configured || !o.Enabled || o.URL != w.url || o.Delay != w.delay {
			t.Errorf("%s = %+v, want %+v configured+enabled", o.Name, o, w)
		}
	}
}

// Правка через интерфейс (LoadDoc -> Save) на старом конфиге не теряет его
// секции и не ломает его для следующей загрузки.
func TestV100SiteConfigSurvivesUIEdit(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", "v1.0.0-site.json"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, src, 0o644); err != nil {
		t.Fatal(err)
	}

	doc, err := LoadDoc(path)
	if err != nil {
		t.Fatalf("LoadDoc: %v", err)
	}
	if err := doc.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	res, err := Load(path)
	if err != nil {
		t.Fatalf("reload after Save: %v", err)
	}
	if !res.PreviewEnabled || res.PreviewPath != "live/preview" || len(res.Outputs) != 3 || res.APIAddr != "127.0.0.1:8080" {
		t.Fatalf("after Save: preview %v %q, outputs %d, api %q", res.PreviewEnabled, res.PreviewPath, len(res.Outputs), res.APIAddr)
	}
}
