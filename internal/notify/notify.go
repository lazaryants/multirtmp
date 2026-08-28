// Package notify sends operator alerts to Telegram: source lost/recovered,
// output down/up, reconnect storms, excessive lag. Disabled (silent no-op)
// when not configured — never an error for the caller either way.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// throttleWindow — не чаще одного сообщения на throttle key за это время,
// чтобы шторм одинаковых событий не залил чат.
const throttleWindow = 60 * time.Second

const sendTimeout = 10 * time.Second

// Config — параметры Telegram-бота. Пустое значение = notify выключен.
type Config struct {
	BotToken string
	ChatID   string
}

// Notifier отправляет алерты в Telegram с троттлингом по ключу события.
// Нулевое значение (или nil-указатель) — выключенный notifier: Send —
// безопасный no-op, чтобы вызывающему коду не нужно было проверять nil
// повсюду.
type Notifier struct {
	cfg    Config
	client *http.Client
	logger *slog.Logger

	mu       sync.Mutex
	lastSent map[string]time.Time
}

// New создаёт Notifier. Если cfg пустой (BotToken или ChatID не заданы) —
// возвращённый Notifier выключен: Send ничего не делает и не ошибается.
func New(cfg Config, logger *slog.Logger) *Notifier {
	return &Notifier{
		cfg:      cfg,
		client:   &http.Client{Timeout: sendTimeout},
		logger:   logger,
		lastSent: map[string]time.Time{},
	}
}

func (n *Notifier) enabled() bool {
	return n != nil && n.cfg.BotToken != "" && n.cfg.ChatID != ""
}

// Send отправляет message в Telegram, если notify включён и throttleKey не
// отправлялся в последние throttleWindow. Не блокирует вызывающего —
// отправка идёт в фоновой горутине; ошибки только логируются (потеря одного
// алерта не должна ронять ingest/output).
func (n *Notifier) Send(throttleKey, message string) {
	if !n.enabled() {
		return
	}

	n.mu.Lock()
	if last, ok := n.lastSent[throttleKey]; ok && time.Since(last) < throttleWindow {
		n.mu.Unlock()
		return
	}
	n.lastSent[throttleKey] = time.Now()
	n.mu.Unlock()

	go n.send(message)
}

func (n *Notifier) send(message string) {
	body, err := json.Marshal(map[string]string{
		"chat_id": n.cfg.ChatID,
		"text":    message,
	})
	if err != nil {
		n.logger.Warn("notify: failed to encode telegram message", "error", err)
		return
	}

	endpoint := "https://api.telegram.org/bot" + url.PathEscape(n.cfg.BotToken) + "/sendMessage"

	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		n.logger.Warn("notify: failed to build telegram request", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		n.logger.Warn("notify: failed to send telegram message", "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		n.logger.Warn("notify: telegram API returned an error", "status", resp.StatusCode)
	}
}
