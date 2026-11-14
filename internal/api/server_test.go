package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequireToken(t *testing.T) {
	const tok = "s3cret-token"

	cases := []struct {
		name       string
		cfgToken   string
		remote     string
		method     string
		host       string
		origin     string
		bearer     string
		queryToken string
		want       int
	}{
		// Верный токен — пропускается при любом Host (DHCP, имя компьютера).
		{name: "lan, valid token, arbitrary Host", cfgToken: tok, remote: "192.168.1.50:51000", method: "GET", host: "studio-pc:8080", bearer: tok, want: 200},
		{name: "lan, valid token, other IP Host, POST", cfgToken: tok, remote: "192.168.1.50:51000", method: "POST", host: "10.0.0.99:8080", origin: "http://10.0.0.99:8080", bearer: tok, want: 200},
		{name: "lan, valid token in query", cfgToken: tok, remote: "192.168.1.50:51000", method: "GET", host: "studio-pc:8080", queryToken: tok, want: 200},
		{name: "lan, no token", cfgToken: tok, remote: "192.168.1.50:51000", method: "GET", host: "127.0.0.1:8080", want: 401},
		{name: "lan, wrong token", cfgToken: tok, remote: "192.168.1.50:51000", method: "GET", host: "studio-pc:8080", bearer: "nope", want: 401},

		// Loopback без токена — проверки Host/Origin.
		{name: "loopback, no token, GET", cfgToken: tok, remote: "127.0.0.1:51000", method: "GET", host: "127.0.0.1:8080", want: 200},
		{name: "loopback, localhost, same-origin POST", cfgToken: tok, remote: "127.0.0.1:51000", method: "POST", host: "localhost:8080", origin: "http://localhost:8080", want: 200},
		{name: "loopback ipv6", cfgToken: tok, remote: "[::1]:51000", method: "GET", host: "[::1]:8080", want: 200},
		{name: "loopback, POST without Origin (curl)", cfgToken: tok, remote: "127.0.0.1:51000", method: "POST", host: "127.0.0.1:8080", want: 200},
		{name: "loopback, foreign Host (DNS rebinding)", cfgToken: tok, remote: "127.0.0.1:51000", method: "GET", host: "evil.example:8080", want: 403},
		{name: "loopback, cross-origin POST (CSRF)", cfgToken: tok, remote: "127.0.0.1:51000", method: "POST", host: "127.0.0.1:8080", origin: "http://evil.example", want: 403},
		{name: "loopback, Origin null", cfgToken: tok, remote: "127.0.0.1:51000", method: "DELETE", host: "127.0.0.1:8080", origin: "null", want: 403},
		{name: "loopback, valid token beats foreign Host/Origin", cfgToken: tok, remote: "127.0.0.1:51000", method: "POST", host: "evil.example:8080", origin: "http://evil.example", bearer: tok, want: 200},

		// Токен в конфиге пуст (loopback-режим): "" == "" не должно считаться
		// верным токеном и обходить проверки Host/Origin.
		{name: "empty cfg token, foreign Host", cfgToken: "", remote: "127.0.0.1:51000", method: "GET", host: "evil.example:8080", want: 403},
		{name: "empty cfg token, empty bearer, CSRF", cfgToken: "", remote: "127.0.0.1:51000", method: "POST", host: "127.0.0.1:8080", origin: "http://evil.example", bearer: "", want: 403},
		{name: "empty cfg token, local GET", cfgToken: "", remote: "127.0.0.1:51000", method: "GET", host: "127.0.0.1:8080", want: 200},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{token: tc.cfgToken, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			h := s.requireToken(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			target := "/status"
			if tc.queryToken != "" {
				target += "?token=" + tc.queryToken
			}
			req := httptest.NewRequest(tc.method, target, nil)
			req.RemoteAddr = tc.remote
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tc.bearer)
			}

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("got %d, want %d (body: %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// Без токена с другого устройства: браузер за страницей получает понятную
// HTML-страницу, скрипт интерфейса за JSON — прежний JSON. Код 401 в обоих
// случаях.
func TestUnauthorizedPageForBrowsers(t *testing.T) {
	s := &Server{token: "tok", logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := s.requireToken(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	cases := []struct {
		name, accept string
		html         bool
	}{
		{"browser page", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", true},
		{"api fetch", "*/*", false},
		{"api json", "application/json", false},
		{"no accept", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = "192.168.1.50:51000"
			req.Host = "192.168.1.12:8080"
			if tc.accept != "" {
				req.Header.Set("Accept", tc.accept)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("code = %d, want 401", rec.Code)
			}
			ct := rec.Header().Get("Content-Type")
			body := rec.Body.String()
			if tc.html {
				if !strings.HasPrefix(ct, "text/html") || !strings.Contains(body, "Show link for tablet") || strings.Contains(body, `"error"`) {
					t.Fatalf("want HTML page, got %q: %s", ct, body)
				}
			} else if !strings.Contains(ct, "json") || !strings.Contains(body, "unauthorized: missing or invalid token") {
				t.Fatalf("want JSON, got %q: %s", ct, body)
			}
		})
	}
}
