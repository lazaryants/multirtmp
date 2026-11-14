package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// previewPrefix — под этим путём multirtmp отдаёт плеер MediaMTX: тот же хост
// и порт, что и у интерфейса, поэтому iframe превью открывается по тому
// адресу, по которому открыта страница (127.0.0.1 на сервере, LAN-IP или
// имя компьютера на планшете), и закрыт тем же токеном (см. requireToken и
// previewCookie). Сам MediaMTX слушает WebRTC-сигнализацию только на
// 127.0.0.1 (см. MTX_WEBRTCADDRESS в cmd/multirtmp/mediamtx.go).
const previewPrefix = "/preview/"

// previewCookie — cookie с токеном для путей превью. iframe и плеер MediaMTX
// внутри него не умеют слать заголовок Authorization, а query-строку плеер
// передаёт только в первый WHEP-запрос (PATCH/DELETE идут на адрес из
// Location без неё) — поэтому токен для /preview/ ходит в cookie:
// HttpOnly (недоступна скриптам), SameSite=Strict (не уходит с чужих
// сайтов), Path=/preview/ (не принимается остальным API).
const previewCookie = "multirtmp_preview"

// newPreviewProxy — обратный прокси на WebRTC-сервер MediaMTX по адресу base
// (preview.url, напр. "http://127.0.0.1:8889").
func newPreviewProxy(base string, logger *slog.Logger) (http.Handler, error) {
	target, err := url.Parse(base)
	if err != nil || target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("api: invalid preview url %q", base)
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, strings.TrimSuffix(previewPrefix, "/"))
			pr.Out.URL.RawPath = ""

			// Токен MediaMTX незачем: он не знает о нём, а в его логи и
			// в его собственную проверку прав (query-параметры) ему лучше
			// не попадать.
			q := pr.In.URL.Query()
			q.Del("token")
			pr.Out.URL.RawQuery = q.Encode()
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Authorization")
		},
		ModifyResponse: func(resp *http.Response) error {
			// MediaMTX отдаёт абсолютные пути (редирект на "/live/preview/",
			// адрес WHEP-сессии) — без префикса они ушли бы мимо прокси.
			if loc := resp.Header.Get("Location"); strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") {
				resp.Header.Set("Location", strings.TrimSuffix(previewPrefix, "/")+loc)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Debug("api: preview proxy: MediaMTX not reachable", "path", r.URL.Path, "error", err)
			writeError(w, http.StatusBadGateway, errors.New("preview server is not reachable"))
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !previewRequestAllowed(r.Method, r.URL.Path) {
			writeError(w, http.StatusForbidden, errors.New("forbidden: only watching the preview is allowed here"))
			return
		}
		rp.ServeHTTP(w, r)
	}), nil
}

// previewRequestAllowed — через прокси можно только СМОТРЕТЬ: страница и
// скрипты плеера (GET), WHEP — создание сессии (POST .../whep), трикл ICE и
// закрытие (PATCH/DELETE .../whep/<id>). Всё остальное — 403. Это важно:
// MediaMTX видит запросы прокси как пришедшие с 127.0.0.1, а с него
// разрешена публикация (WHIP), то есть без этого фильтра планшет с токеном
// мог бы подменить поток превью.
func previewRequestAllowed(method, path string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return !strings.HasSuffix(path, "/whip")
	case http.MethodPost:
		return strings.HasSuffix(path, "/whep")
	case http.MethodPatch, http.MethodDelete:
		return strings.Contains(path, "/whep/")
	}
	return false
}
