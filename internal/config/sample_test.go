package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteInstallConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	created, err := WriteInstallConfig(path)
	if err != nil || !created {
		t.Fatalf("WriteInstallConfig = %v, %v", created, err)
	}

	res, err := Load(path)
	if err != nil {
		t.Fatalf("the generated config does not load: %v", err)
	}

	if res.APIAddr != "0.0.0.0:8080" {
		t.Errorf("api.addr = %q", res.APIAddr)
	}
	if len(res.APIToken) != 32 {
		t.Errorf("api.token length = %d, want 32", len(res.APIToken))
	}
	if !res.PreviewEnabled || res.PreviewURL != "http://127.0.0.1:8889/live/preview" {
		t.Errorf("preview = %v %q", res.PreviewEnabled, res.PreviewURL)
	}

	type want struct {
		delay               time.Duration
		enabled, configured bool
	}
	wants := map[string]want{
		"youtube": {15 * time.Second, false, false},
		"company": {0, false, false},
		"preview": {0, true, true},
	}
	if len(res.Outputs) != len(wants) {
		t.Fatalf("outputs = %+v", res.Outputs)
	}
	for _, o := range res.Outputs {
		w, ok := wants[o.Name]
		if !ok {
			t.Fatalf("unexpected output %q", o.Name)
		}
		if o.Delay != w.delay || o.Enabled != w.enabled || o.Configured != w.configured {
			t.Errorf("%s = %+v, want %+v", o.Name, o, w)
		}
	}
	for _, o := range res.Outputs {
		if o.Name == "preview" && o.URL != "rtmp://127.0.0.1:19350/live/preview" {
			t.Errorf("preview url = %q", o.URL)
		}
	}

	// Повторный install: файл не трогается ни на байт.
	before, _ := os.ReadFile(path)
	created, err = WriteInstallConfig(path)
	if err != nil || created {
		t.Fatalf("second WriteInstallConfig = %v, %v; want false, nil", created, err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("existing config.json was modified")
	}

	// Своя случайная строка на каждой машине.
	other := filepath.Join(dir, "other.json")
	if _, err := WriteInstallConfig(other); err != nil {
		t.Fatal(err)
	}
	res2, err := Load(other)
	if err != nil {
		t.Fatal(err)
	}
	if res2.APIToken == res.APIToken {
		t.Fatal("two installs got the same token")
	}
}

// Файл с ключами площадки (любое содержимое, даже невалидное) — не трогается.
func TestWriteInstallConfigKeepsExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	keys := `{"outputs":[{"name":"youtube","url":"rtmp://a.rtmp.youtube.com/live2","key":"real-key"}]} // hand edit`
	if err := os.WriteFile(path, []byte(keys), 0o644); err != nil {
		t.Fatal(err)
	}
	created, err := WriteInstallConfig(path)
	if err != nil || created {
		t.Fatalf("WriteInstallConfig = %v, %v", created, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != keys {
		t.Fatalf("file changed:\n%s", got)
	}
}

// Образец при ручном запуске без конфига по-прежнему валиден и создаётся
// только один раз.
func TestWriteSampleStillValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := WriteSample(path); err != nil {
		t.Fatal(err)
	}
	res, err := Load(path)
	if err != nil {
		t.Fatalf("sample does not load: %v", err)
	}
	if res.APIAddr != "127.0.0.1:8080" || res.PreviewEnabled {
		t.Errorf("sample api/preview changed: %q %v", res.APIAddr, res.PreviewEnabled)
	}
	if err := WriteSample(path); err == nil {
		t.Fatal("second WriteSample: expected an error")
	}
}

// Хук для deploy\installer\test\test-installer-parse.ps1: если задана
// MULTIRTMP_WRITE_INSTALL_CONFIG, пишет туда настоящий конфиг install —
// на нём проверяется разбор конфига в коде установщика. Без переменной
// ничего не делает.
func TestWriteInstallConfigForInstallerTest(t *testing.T) {
	path := os.Getenv("MULTIRTMP_WRITE_INSTALL_CONFIG")
	if path == "" {
		t.Skip("MULTIRTMP_WRITE_INSTALL_CONFIG not set")
	}
	if created, err := WriteInstallConfig(path); err != nil || !created {
		t.Fatalf("WriteInstallConfig(%s) = %v, %v", path, created, err)
	}
}
