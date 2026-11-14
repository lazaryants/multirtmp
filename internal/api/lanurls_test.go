package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"multirtmp/internal/buffer"
	"multirtmp/internal/ingest"
	"multirtmp/internal/notify"
	"multirtmp/internal/output"
)

// Ссылки для планшета вычисляются на каждый GET /network/lan-urls (адрес
// машины меняется через DHCP) и НЕ вычисляются на секундный GET /status
// (список адаптеров не должен дёргаться каждую секунду).
func TestLANURLsComputedPerRequestNotOnStatus(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"outputs": []}`), 0o644); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	buf := buffer.New(1024 * 1024)
	notifier := notify.New(notify.Config{}, logger)

	calls := 0
	addrs := [][]string{{"http://192.168.1.12:8080/"}, {"http://192.168.1.57:8080/"}}
	network := NetworkInfo{
		LANAccess: true,
		LANURLs: func() ([]string, error) {
			u := addrs[calls%len(addrs)]
			calls++
			return u, nil
		},
	}

	s, err := NewServer("127.0.0.1:0", "", network, PreviewInfo{}, logger,
		ingest.NewServer("127.0.0.1:0", logger, buf, 5*time.Second, notifier), buf,
		output.NewRegistry(ctx), cfgPath, notifier)
	if err != nil {
		t.Fatal(err)
	}

	get := func(path string) map[string]any {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "127.0.0.1:50000"
		req.Host = "127.0.0.1:8080"
		rec := httptest.NewRecorder()
		s.httpSrv.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", path, rec.Code, rec.Body.String())
		}
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	for i := 0; i < 5; i++ {
		st := get("/status")
		if la := st["network"].(map[string]any)["lan_access"]; la != true {
			t.Fatalf("lan_access = %v, want true", la)
		}
	}
	if calls != 0 {
		t.Fatalf("/status computed LAN URLs %d times, want 0", calls)
	}

	// Адрес "сменился" между двумя открытиями окна — второй ответ уже новый.
	first := get("/network/lan-urls")["urls"]
	second := get("/network/lan-urls")["urls"]
	if !reflect.DeepEqual(first, []any{"http://192.168.1.12:8080/"}) || !reflect.DeepEqual(second, []any{"http://192.168.1.57:8080/"}) {
		t.Fatalf("got %v then %v", first, second)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}
