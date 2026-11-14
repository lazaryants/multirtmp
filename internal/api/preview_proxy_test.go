package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"multirtmp/internal/buffer"
	"multirtmp/internal/ingest"
	"multirtmp/internal/notify"
	"multirtmp/internal/output"
)

// fakeMediaMTX ведёт себя как WebRTC-сервер MediaMTX v1.21.3 в той части,
// что важна прокси: редирект на путь со слэшем и WHEP с Location —
// абсолютные пути без префикса.
type fakeMediaMTX struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (f *fakeMediaMTX) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r.Clone(context.Background()))
	f.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/live/preview":
		loc := "/live/preview/"
		if r.URL.RawQuery != "" {
			loc += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, loc, http.StatusFound)
	case r.Method == http.MethodPost && r.URL.Path == "/live/preview/whep":
		w.Header().Set("Location", "/live/preview/whep/abc")
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

func (f *fakeMediaMTX) last() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		return nil
	}
	return f.reqs[len(f.reqs)-1]
}

func (f *fakeMediaMTX) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

const proxyTestToken = "s3cret-token"

func newPreviewTestServer(t *testing.T) (*Server, *fakeMediaMTX) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	mtx := &fakeMediaMTX{}
	upstream := httptest.NewServer(mtx)
	t.Cleanup(upstream.Close)

	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"outputs": []}`), 0o644); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	buf := buffer.New(1024 * 1024)
	notifier := notify.New(notify.Config{}, logger)

	s, err := NewServer("0.0.0.0:8080", proxyTestToken, NetworkInfo{LANAccess: true},
		PreviewInfo{Enabled: true, URL: "/preview/live/preview/", Target: upstream.URL},
		logger, ingest.NewServer("127.0.0.1:0", logger, buf, 5*time.Second, notifier), buf,
		output.NewRegistry(ctx), cfgPath, notifier)
	if err != nil {
		t.Fatal(err)
	}
	return s, mtx
}

type reqOpt func(*http.Request)

func fromTablet(r *http.Request) { r.RemoteAddr = "192.168.1.50:51000"; r.Host = "192.168.1.12:8080" }
func fromServer(r *http.Request) { r.RemoteAddr = "127.0.0.1:51000"; r.Host = "127.0.0.1:8080" }
func withCookie(v string) reqOpt {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: previewCookie, Value: v}) }
}

func do(s *Server, method, target string, opts ...reqOpt) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, req)
	return rec
}

func TestPreviewProxyFromTablet(t *testing.T) {
	s, mtx := newPreviewTestServer(t)

	// Без токена с планшета — ни страницы плеера, ни WHEP.
	if rec := do(s, "GET", "/preview/live/preview/", fromTablet); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	if rec := do(s, "POST", "/preview/live/preview/whep", fromTablet); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token, whep: %d", rec.Code)
	}
	if mtx.count() != 0 {
		t.Fatal("unauthenticated request reached MediaMTX")
	}

	// Открыл интерфейс по ссылке с токеном — получил cookie для превью.
	rec := do(s, "GET", "/?token="+proxyTestToken, fromTablet)
	if rec.Code != http.StatusOK {
		t.Fatalf("index with token: %d", rec.Code)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == previewCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no preview cookie set")
	}
	if cookie.Value != proxyTestToken || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != previewPrefix {
		t.Fatalf("cookie = %+v", cookie)
	}

	// Уже есть cookie — секундный опрос /status с Bearer не выставляет её снова.
	rec = do(s, "GET", "/status", fromTablet, withCookie(proxyTestToken), func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+proxyTestToken)
	})
	if rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("status with cookie present: %d, cookies %v", rec.Code, rec.Result().Cookies())
	}

	// Плеер по cookie: путь без префикса, без cookie и токена на стороне MediaMTX.
	rec = do(s, "GET", "/preview/live/preview/", fromTablet, withCookie(proxyTestToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("player page: %d", rec.Code)
	}
	if up := mtx.last(); up.URL.Path != "/live/preview/" || up.Header.Get("Cookie") != "" || up.Header.Get("Authorization") != "" {
		t.Fatalf("upstream saw path %q cookie %q auth %q", up.URL.Path, up.Header.Get("Cookie"), up.Header.Get("Authorization"))
	}

	// Редирект MediaMTX: токен в MediaMTX не уходит, Location — под префиксом.
	rec = do(s, "GET", "/preview/live/preview?token="+proxyTestToken+"&muted=1", fromTablet)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/preview/live/preview/?muted=1" {
		t.Fatalf("redirect: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if q := mtx.last().URL.RawQuery; strings.Contains(q, "token") {
		t.Fatalf("token leaked to MediaMTX: %q", q)
	}

	// WHEP: создание сессии, Location под префиксом; трикл и закрытие — по cookie.
	rec = do(s, "POST", "/preview/live/preview/whep", fromTablet, withCookie(proxyTestToken))
	if rec.Code != http.StatusCreated || rec.Header().Get("Location") != "/preview/live/preview/whep/abc" {
		t.Fatalf("whep: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	for _, m := range []string{"PATCH", "DELETE"} {
		if rec := do(s, m, "/preview/live/preview/whep/abc", fromTablet, withCookie(proxyTestToken)); rec.Code != http.StatusOK {
			t.Fatalf("%s session: %d", m, rec.Code)
		}
	}

	// Неверная cookie — 401.
	if rec := do(s, "GET", "/preview/live/preview/", fromTablet, withCookie("wrong")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong cookie: %d", rec.Code)
	}

	// Cookie пускает ТОЛЬКО в превью — не в остальное API.
	if rec := do(s, "GET", "/outputs", fromTablet, withCookie(proxyTestToken)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("cookie on /outputs: %d, want 401", rec.Code)
	}
}

// Через прокси можно только смотреть: публикация (WHIP) и прочее — 403, до
// MediaMTX не доходит (он видит прокси как 127.0.0.1, которому публиковать можно).
func TestPreviewProxyWatchOnly(t *testing.T) {
	s, mtx := newPreviewTestServer(t)
	for _, c := range []struct{ method, path string }{
		{"POST", "/preview/live/hijack/whip"},
		{"PATCH", "/preview/live/hijack/whip/abc"},
		{"POST", "/preview/live/preview/"},
		{"DELETE", "/preview/live/preview/"},
		{"GET", "/preview/live/hijack/whip"},
	} {
		before := mtx.count()
		rec := do(s, c.method, c.path, fromTablet, withCookie(proxyTestToken))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: %d, want 403", c.method, c.path, rec.Code)
		}
		if mtx.count() != before {
			t.Errorf("%s %s reached MediaMTX", c.method, c.path)
		}
	}
}

// На самом сервере (окно с ярлыка) превью работает без токена, как и всё
// остальное.
func TestPreviewProxyFromServer(t *testing.T) {
	s, _ := newPreviewTestServer(t)
	if rec := do(s, "GET", "/preview/live/preview/", fromServer); rec.Code != http.StatusOK {
		t.Fatalf("loopback player page: %d", rec.Code)
	}
	if rec := do(s, "POST", "/preview/live/preview/whep", fromServer, func(r *http.Request) {
		r.Header.Set("Origin", "http://127.0.0.1:8080")
	}); rec.Code != http.StatusCreated {
		t.Fatalf("loopback whep: %d", rec.Code)
	}
}

// MediaMTX не запущен — понятная 502, а не зависание.
func TestPreviewProxyUpstreamDown(t *testing.T) {
	proxy, err := newPreviewProxy("http://127.0.0.1:1", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, httptest.NewRequest("GET", "/preview/live/preview/", nil))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "not reachable") {
		t.Fatalf("upstream down: %d %s", rec.Code, rec.Body.String())
	}
}
