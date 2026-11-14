// Package api — HTTP API + веб-интерфейс статуса и управления выходами.
// Запросы с самой этой машины (loopback) работают без токена; с других машин
// (api.addr слушает LAN, доступ с планшета) нужен токен — см. requireToken.
package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"multirtmp/internal/buffer"
	"multirtmp/internal/ingest"
	"multirtmp/internal/mediamtx"
	"multirtmp/internal/notify"
	"multirtmp/internal/output"
)

// NetworkInfo — то, что Server сообщает о своей LAN-доступности (см.
// cmd/multirtmp/lanurl.go, где эти значения вычисляются один раз при
// старте). Пустые поля означают "не применимо" (loopback-режим) — фронтенд
// прячет соответствующие элементы интерфейса.
type NetworkInfo struct {
	// LANAccess — api.addr слушает не только loopback, то есть у кнопки "Show
	// link for tablet" есть смысл. Не меняется без рестарта.
	LANAccess bool
	// LANURLs вычисляет ссылки на веб-интерфейс с токеном (см.
	// cmd/multirtmp/lanurl.go). Вызывается на каждый GET /network/lan-urls —
	// то есть по клику "Show link for tablet", а не на каждый секундный
	// /status: адрес машины на площадке меняется через DHCP, и ссылка,
	// вычисленная при старте, устаревает. Ошибка не мешает отдать то, что
	// вычислилось (ссылку с заглушкой вместо адреса). Ссылки отдаются только
	// клиенту, прошедшему requireToken: либо он уже знает этот токен, либо это
	// локальный запрос с самой этой машины, где токен и так лежит в
	// config.json — не новая утечка.
	LANURLs func() ([]string, error)
	// FirewallHint — готовая PowerShell-команда, если правило Windows
	// Firewall для текущего порта не найдено. Пусто — либо loopback, либо
	// правило на месте.
	FirewallHint string
}

// PreviewInfo — настройки необязательного WebRTC-превью в интерфейсе (см.
// config.PreviewConfig). URL уже склеен (<preview.url>/<preview.path>) —
// фронтенду достаточно проверить Enabled и подставить URL в iframe как есть.
type PreviewInfo struct {
	Enabled bool
	// URL — адрес плеера для iframe: путь под /preview/ на этом же сервере
	// (см. preview_proxy.go), напр. "/preview/live/preview/".
	URL string
	// Target — адрес WebRTC-сервера MediaMTX, куда проксирует /preview/
	// (preview.url, напр. "http://127.0.0.1:8889").
	Target string
	// MediaMTX — состояние дочернего MediaMTX (см. internal/mediamtx); nil,
	// если превью выключено. Снимок под мьютексом супервизора — дёшево на
	// каждый секундный /status.
	MediaMTX func() mediamtx.Status
}

// Server — HTTP API + статика поверх ingest/buffer/output.
type Server struct {
	addr    string
	token   string
	network NetworkInfo
	preview PreviewInfo
	logger  *slog.Logger

	ingest   *ingest.Server
	buf      *buffer.Buffer
	registry *output.Registry
	outputs  *outputsStore

	httpSrv *http.Server
}

// NewServer создаёт API-сервер, слушающий addr (например, "127.0.0.1:8080").
// token — из config.Resolved.APIToken; может быть пустым на loopback-адресах
// (тогда пускаются только локальные запросы — см. requireToken). cfgPath —
// config.json для персиста правок из раздела Destinations (см.
// outputs_store.go) — каждая мутация перечитывает его заново с диска, ничего
// не кешируется между запросами; notifier — для алертов down/up у выходов,
// добавленных через UI, наравне с выходами из стартового конфига.
func NewServer(addr, token string, network NetworkInfo, preview PreviewInfo, logger *slog.Logger, ingestSrv *ingest.Server, buf *buffer.Buffer, registry *output.Registry, cfgPath string, notifier *notify.Notifier) (*Server, error) {
	outputs, err := newOutputsStore(cfgPath, registry, buf, logger, notifier)
	if err != nil {
		return nil, fmt.Errorf("api: failed to initialize outputs store: %w", err)
	}

	s := &Server{
		addr:     addr,
		token:    token,
		network:  network,
		preview:  preview,
		logger:   logger,
		ingest:   ingestSrv,
		buf:      buf,
		registry: registry,
		outputs:  outputs,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /network/lan-urls", s.handleLANURLs)
	mux.HandleFunc("POST /outputs/{name}/start", s.handleOutputStart)
	mux.HandleFunc("POST /outputs/{name}/stop", s.handleOutputStop)
	mux.HandleFunc("GET /outputs", s.handleOutputsList)
	mux.HandleFunc("POST /outputs", s.handleOutputsCreate)
	mux.HandleFunc("PUT /outputs/{name}", s.handleOutputsUpdate)
	mux.HandleFunc("DELETE /outputs/{name}", s.handleOutputsDelete)
	mux.HandleFunc("GET /", s.handleIndex)

	if preview.Enabled && preview.Target != "" {
		proxy, err := newPreviewProxy(preview.Target, logger)
		if err != nil {
			return nil, err
		}
		// По методу на шаблон: "/preview/" без метода конфликтует с "GET /"
		// в ServeMux (ни один не уже другого). Разрешённые комбинации
		// метод+путь проверяет сам прокси.
		for _, m := range []string{"GET", "POST", "PATCH", "DELETE", "OPTIONS"} {
			mux.Handle(m+" "+previewPrefix, proxy)
		}
	}

	s.httpSrv = &http.Server{
		Addr:    addr,
		Handler: s.requireToken(mux),
	}

	return s, nil
}

// MaxOutputDelay — см. outputsStore.MaxOutputDelay; проброшено наружу для
// cmd/multirtmp/bufferauto.go, которому не нужно знать про outputsStore.
func (s *Server) MaxOutputDelay() time.Duration {
	return s.outputs.MaxOutputDelay()
}

// requireToken — единственная точка проверки доступа, оборачивает весь mux
// целиком (включая GET / — токен из query должен пускать и первую загрузку
// страницы). Порядок:
//
//  1. Верный токен (если он вообще задан в конфиге) — пропустить, без
//     проверки Host/Origin/адреса клиента: чужой сайт токена не знает, а
//     планшет может обращаться по имени компьютера или по IP, сменившемуся
//     через DHCP.
//  2. Иначе запрос с loopback-адреса (сама эта машина: ярлык на рабочем
//     столе, браузер на сервере) — пропустить без токена, но только если
//     Host — loopback-имя (защита от DNS rebinding) и, для изменяющих
//     методов, Origin отсутствует или совпадает с Host (защита от CSRF).
//  3. Иначе — 401.
//
// Токен ищется в заголовке Authorization: Bearer <token> (все запросы
// фронтенда после первой загрузки) и в ?token=<token> (первая загрузка
// страницы по ссылке).
//
// Ни здесь, ни где-либо ещё в этом пакете НЕ логируется r.URL.String()/
// RawQuery или заголовки целиком — там может быть токен. Строки про отказ
// логируют только r.URL.Path и причину.
func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, viaCookie := s.validToken(r); ok {
			if !viaCookie {
				s.setPreviewCookie(w, r)
			}
			next.ServeHTTP(w, r)
			return
		}

		if !isLoopbackRemote(r.RemoteAddr) {
			s.logger.Warn("api: rejected request with missing/invalid token", "path", r.URL.Path)
			writeError(w, http.StatusUnauthorized, errors.New("unauthorized: missing or invalid token"))
			return
		}

		if reason := loopbackRequestProblem(r); reason != "" {
			s.logger.Warn("api: rejected local request", "path", r.URL.Path, "reason", reason)
			writeError(w, http.StatusForbidden, errors.New("forbidden: "+reason))
			return
		}

		next.ServeHTTP(w, r)
	})
}

// validToken — true, только если токен в конфиге задан И запрос несёт
// ровно его. Проверка s.token != "" обязательна: ConstantTimeCompare двух
// пустых строк возвращает 1, и без неё в loopback-режиме (токена нет) любой
// запрос без токена считался бы "с верным токеном" и обходил проверки
// Host/Origin.
func (s *Server) validToken(r *http.Request) (ok, viaCookie bool) {
	if s.token == "" {
		return false, false
	}

	tok := r.URL.Query().Get("token")
	if tok == "" {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			tok = strings.TrimPrefix(auth, "Bearer ")
		}
	}
	if tok != "" {
		return subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) == 1, false
	}

	// Cookie принимается ТОЛЬКО для путей превью (плеер в iframe не умеет
	// слать заголовок) — остальное API по cookie не пускает.
	if strings.HasPrefix(r.URL.Path, previewPrefix) {
		if c, err := r.Cookie(previewCookie); err == nil {
			return subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.token)) == 1, true
		}
	}
	return false, false
}

// setPreviewCookie выдаёт cookie для путей превью клиенту, который только
// что предъявил верный токен (query или Bearer), — если превью включено и
// такой cookie у него ещё нет. Так планшет, открывший интерфейс по ссылке с
// токеном, получает доступ и к плееру в iframe.
func (s *Server) setPreviewCookie(w http.ResponseWriter, r *http.Request) {
	if !s.preview.Enabled {
		return
	}
	if c, err := r.Cookie(previewCookie); err == nil && c.Value == s.token {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     previewCookie,
		Value:    s.token,
		Path:     previewPrefix,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// isLoopbackRemote — пришло ли TCP-соединение с самой этой машины. Решение
// принимается только по фактическому адресу сокета, заголовки
// (X-Forwarded-For и т.п.) не учитываются вообще — их подделывает кто угодно.
func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// loopbackRequestProblem — почему запрос с loopback-адреса без токена
// нельзя пропустить; пустая строка — можно.
//
// Host: браузер на этой машине, открывший страницу чужого сайта, может через
// DNS rebinding (домен злоумышленника временно резолвится в 127.0.0.1)
// отправлять запросы как будто с того же origin и читать ответы — включая
// ключи потоков из GET /outputs. В таком запросе Host — домен
// злоумышленника, поэтому допускаем только loopback-имена.
//
// Origin: обработчики разбирают JSON независимо от Content-Type, так что
// чужой сайт может послать POST с text/plain без CORS-preflight (CSRF) —
// например, добавить выход на свой RTMP-сервер. Браузер всегда ставит Origin
// на кросс-доменные POST/PUT/DELETE; отсутствие Origin — не браузерный
// клиент (curl, сама подкоманда open) или запрос того же origin.
func loopbackRequestProblem(r *http.Request) string {
	if !isLoopbackHostName(hostWithoutPort(r.Host)) {
		return "unexpected Host header for a local request"
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return ""
	}

	origin := r.Header.Get("Origin")
	if origin == "" {
		return ""
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || !strings.EqualFold(u.Host, r.Host) {
		return "cross-origin request"
	}

	return ""
}

// hostWithoutPort — имя из заголовка Host без порта и без квадратных скобок
// IPv6 ("[::1]:8080" -> "::1", "localhost" -> "localhost").
func hostWithoutPort(hostHeader string) string {
	if h, _, err := net.SplitHostPort(hostHeader); err == nil {
		return h
	}
	return strings.TrimSuffix(strings.TrimPrefix(hostHeader, "["), "]")
}

func isLoopbackHostName(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// ListenAndServe блокируется, пока не будет вызван Close. При штатной
// остановке возвращает nil.
func (s *Server) ListenAndServe() error {
	s.logger.Info("api: listening for HTTP", "addr", s.addr)

	if err := s.httpSrv.ListenAndServe(); err != nil {
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("api: serve error: %w", err)
	}

	return nil
}

// Close останавливает сервер.
func (s *Server) Close() error {
	if err := s.httpSrv.Shutdown(context.Background()); err != nil {
		return fmt.Errorf("api: failed to stop server: %w", err)
	}
	return nil
}
