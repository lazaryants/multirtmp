// Package config загружает и валидирует config.json: адреса ingest/API,
// размер буфера, список выходов. Невалидный конфиг — отказ стартовать, а не
// тихая работа с умолчаниями (кроме полей, у которых умолчание определено
// явно и описано в примере конфига).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"multirtmp/internal/notify"
)

const (
	DefaultIngestAddr    = "127.0.0.1:1935"
	DefaultSourceTimeout = 5 * time.Second
	DefaultAPIAddr       = "127.0.0.1:8080"

	// DefaultBufferMaxSize — потолок автоподбора размера буфера (buffer.max_size),
	// если в конфиге не указан свой.
	DefaultBufferMaxSize = 512 * 1024 * 1024

	// BufferMinSize — жёсткая нижняя граница автоподбора, не настраивается.
	BufferMinSize = 32 * 1024 * 1024

	DefaultLogLevel = slog.LevelInfo
)

// Config — конфиг как он есть в JSON-файле.
type Config struct {
	Ingest  IngestConfig   `json:"ingest"`
	Buffer  BufferConfig   `json:"buffer"`
	API     APIConfig      `json:"api"`
	Log     LogConfig      `json:"log"`
	Notify  NotifyConfig   `json:"notify"`
	Preview PreviewConfig  `json:"preview"`
	Outputs []OutputConfig `json:"outputs"`
}

type LogConfig struct {
	Level string `json:"level"` // debug/info/warn/error, по умолчанию info
}

type NotifyConfig struct {
	TelegramBotToken string `json:"telegram_bot_token"`
	TelegramChatID   string `json:"telegram_chat_id"`
}

type IngestConfig struct {
	Addr          string `json:"addr"`
	SourceTimeout string `json:"source_timeout"`
}

type BufferConfig struct {
	Size    string `json:"size"`     // явный размер; если пусто — автоподбор по задержкам/битрейту
	MaxSize string `json:"max_size"` // потолок автоподбора, по умолчанию 512MB; игнорируется, если Size задан
}

type APIConfig struct {
	Addr  string `json:"addr"`
	Token string `json:"token"` // обязателен, если addr не loopback (см. IsLoopbackAddr)
	// PublicHost — необязательный адрес (имя или IP, без порта), под которым
	// эту машину видят устройства в LAN. Если задан, ссылка для планшета
	// строится только с ним, без угадывания по адаптерам.
	PublicHost string `json:"public_host"`
}

// PreviewConfig — необязательный оперативный превью-плеер в интерфейсе, под
// блоком STATUS. Сам релей ничего не декодирует и не перепаковывает: поток в
// превью подаётся обычным выходом из outputs (оператор сам добавляет
// назначение вида rtmp://.../preview) на внешний MediaMTX, который отдаёт
// его через WebRTC — сюда попадает только адрес готовой плеер-страницы.
type PreviewConfig struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"`  // база MediaMTX WebRTC, напр. "http://127.0.0.1:8889"
	Path    string `json:"path"` // ключ потока/путь на MediaMTX, напр. "preview"
}

type OutputConfig struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Key     string `json:"key"`
	Delay   string `json:"delay"`
	Enabled *bool  `json:"enabled"` // nil = не указано, по умолчанию true
}

// Resolved — конфиг после применения умолчаний, парсинга длительностей и
// размеров, валидации и подстановки CLI-переопределений. Именно с этим main
// работает дальше.
type Resolved struct {
	IngestAddr    string
	SourceTimeout time.Duration

	// BufferAuto — true, если buffer.size не задан явно: размер буфера
	// подбирается автоматически как (MaxOutputDelay + 60s) * измеренный
	// битрейт, ограниченный [BufferMinSize, BufferMaxSize]. BufferSize в
	// этом случае — только начальное значение (BufferMinSize) до первого
	// измерения.
	BufferAuto    bool
	BufferSize    int64
	BufferMaxSize int64

	APIAddr string
	// APIToken — пусто, если api.addr — loopback (аутентификация выключена).
	// Не-loopback addr без токена — ошибка валидации, не тихий проброс наружу
	// без защиты.
	APIToken string
	// APIPublicHost — api.public_host как есть; пусто — адрес для ссылки на
	// планшет подбирается по сетевым адаптерам.
	APIPublicHost string

	LogLevel slog.Level
	Notify   notify.Config // нулевое значение = notify выключен

	// PreviewURL — уже склеенный <url>/<path> для iframe в интерфейсе; пусто,
	// если preview.enabled=false — фронтенду не нужно ничего склеивать самому,
	// просто проверить, что поле не пусто.
	PreviewEnabled bool
	PreviewURL     string

	Outputs []ResolvedOutput
}

// ResolvedOutput — один выход. URL уже включает ключ потока (см.
// CombineURLKey) — готов к передаче в output.NewWorker как есть.
type ResolvedOutput struct {
	Name    string
	URL     string
	Delay   time.Duration
	Enabled bool
}

// ConfigError оборачивает ошибку загрузки/валидации конфига — сигнал
// вызывающему коду (в частности, обвязке Windows-службы), что перезапуск
// процесса не поможет: нужен человек, который поправит config.json. Служба
// должна в этом случае завершиться "чисто" (код 0), а не уйти в бесконечный
// цикл recovery-рестартов на опечатке в конфиге.
type ConfigError struct {
	err error
}

func (e *ConfigError) Error() string { return e.err.Error() }
func (e *ConfigError) Unwrap() error { return e.err }

// Load читает файл по path, снимает `//`-комментарии, разбирает JSON,
// применяет умолчания, валидирует и возвращает уже готовый к использованию
// Resolved. Если что-то не так — возвращает *ConfigError со всеми найденными
// проблемами разом (не только первой), с номером строки для синтаксических
// ошибок JSON и путём поля (например "outputs[1].name") для смысловых.
func Load(path string) (*Resolved, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, &ConfigError{err: fmt.Errorf("config: failed to read %s: %w", path, err)}
	}

	stripped := stripLineComments(raw)

	var cfg Config
	if err := json.Unmarshal(stripped, &cfg); err != nil {
		return nil, &ConfigError{err: fmt.Errorf("config: %s", describeJSONError(stripped, err))}
	}

	res, err := resolve(&cfg)
	if err != nil {
		return nil, &ConfigError{err: err}
	}

	return res, nil
}

func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid level %q (want debug/info/warn/error)", s)
	}
}

// describeJSONError переводит ошибку encoding/json в сообщение с номером
// строки, если это вообще возможно (SyntaxError/UnmarshalTypeError несут
// byte offset; остальные виды ошибок номера строки не имеют).
func describeJSONError(src []byte, err error) string {
	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		return fmt.Sprintf("line %d: invalid JSON: %s", offsetToLine(src, syn.Offset), syn.Error())
	}

	var ute *json.UnmarshalTypeError
	if errors.As(err, &ute) {
		return fmt.Sprintf("line %d: field %q: expected %s, got %s",
			offsetToLine(src, ute.Offset), ute.Field, ute.Type, ute.Value)
	}

	return fmt.Sprintf("invalid JSON: %s", err.Error())
}

// resolve применяет умолчания и валидирует cfg, возвращая Resolved. Собирает
// все проблемы, а не останавливается на первой.
func resolve(cfg *Config) (*Resolved, error) {
	var problems []string
	addf := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	res := &Resolved{}

	// ingest.addr
	res.IngestAddr = cfg.Ingest.Addr
	if res.IngestAddr == "" {
		res.IngestAddr = DefaultIngestAddr
	}
	if _, _, err := net.SplitHostPort(res.IngestAddr); err != nil {
		addf("ingest.addr: invalid address %q: %s", res.IngestAddr, err)
	}

	// ingest.source_timeout
	if cfg.Ingest.SourceTimeout == "" {
		res.SourceTimeout = DefaultSourceTimeout
	} else if d, err := time.ParseDuration(cfg.Ingest.SourceTimeout); err != nil {
		addf("ingest.source_timeout: invalid duration %q: %s", cfg.Ingest.SourceTimeout, err)
	} else if d <= 0 {
		addf("ingest.source_timeout: must be positive, got %q", cfg.Ingest.SourceTimeout)
	} else {
		res.SourceTimeout = d
	}

	// buffer.size / buffer.max_size
	if cfg.Buffer.Size != "" {
		if n, err := ParseSize(cfg.Buffer.Size); err != nil {
			addf("buffer.size: %s", err)
		} else if n <= 0 {
			addf("buffer.size: must be positive, got %q", cfg.Buffer.Size)
		} else {
			res.BufferSize = n
		}
	} else {
		res.BufferAuto = true
		res.BufferSize = BufferMinSize // стартовое значение до первого измерения битрейта

		if cfg.Buffer.MaxSize == "" {
			res.BufferMaxSize = DefaultBufferMaxSize
		} else if n, err := ParseSize(cfg.Buffer.MaxSize); err != nil {
			addf("buffer.max_size: %s", err)
		} else if n < BufferMinSize {
			addf("buffer.max_size: must be at least %d bytes (32MB), got %q", BufferMinSize, cfg.Buffer.MaxSize)
		} else {
			res.BufferMaxSize = n
		}
	}

	// api.addr
	res.APIAddr = cfg.API.Addr
	if res.APIAddr == "" {
		res.APIAddr = DefaultAPIAddr
	}
	if _, _, err := net.SplitHostPort(res.APIAddr); err != nil {
		addf("api.addr: invalid address %q: %s", res.APIAddr, err)
	} else if !IsLoopbackAddr(res.APIAddr) && cfg.API.Token == "" {
		addf("api.token: required when api.addr is not localhost (%q) — anyone on the network could reach the API otherwise", res.APIAddr)
	}
	res.APIToken = cfg.API.Token
	if h := cfg.API.PublicHost; h != "" {
		if err := validatePublicHost(h); err != nil {
			addf("api.public_host: %s", err)
		}
		res.APIPublicHost = h
	}

	// log.level
	if cfg.Log.Level == "" {
		res.LogLevel = DefaultLogLevel
	} else if lvl, err := parseLogLevel(cfg.Log.Level); err != nil {
		addf("log.level: %s", err)
	} else {
		res.LogLevel = lvl
	}

	// preview — необязательный WebRTC-плеер поверх стороннего MediaMTX;
	// url/path обязательны только когда включён.
	res.PreviewEnabled = cfg.Preview.Enabled
	if cfg.Preview.Enabled {
		if cfg.Preview.URL == "" {
			addf("preview.url: required when preview.enabled is true")
		} else if u, err := url.Parse(cfg.Preview.URL); err != nil || u.Scheme == "" || u.Host == "" {
			addf("preview.url: invalid URL %q", cfg.Preview.URL)
		}
		if cfg.Preview.Path == "" {
			addf("preview.path: required when preview.enabled is true")
		}
		if cfg.Preview.URL != "" && cfg.Preview.Path != "" {
			res.PreviewURL = strings.TrimRight(cfg.Preview.URL, "/") + "/" + strings.TrimLeft(cfg.Preview.Path, "/")
		}
	}

	// notify (telegram) — либо оба поля заданы, либо ни одного
	hasToken := cfg.Notify.TelegramBotToken != ""
	hasChatID := cfg.Notify.TelegramChatID != ""
	switch {
	case hasToken && hasChatID:
		res.Notify = notify.Config{BotToken: cfg.Notify.TelegramBotToken, ChatID: cfg.Notify.TelegramChatID}
	case hasToken != hasChatID:
		addf("notify: both telegram_bot_token and telegram_chat_id are required, or neither")
	}

	// outputs
	seenNames := map[string]bool{}
	for i, o := range cfg.Outputs {
		path := fmt.Sprintf("outputs[%d]", i)

		if o.Name == "" {
			addf("%s.name: required", path)
		} else if seenNames[o.Name] {
			addf("%s.name: duplicate name %q", path, o.Name)
		} else {
			seenNames[o.Name] = true
		}

		if o.URL == "" {
			addf("%s.url: required", path)
		}
		if o.Key == "" {
			addf("%s.key: required", path)
		}

		delay := time.Duration(0)
		if o.Delay != "" {
			d, err := time.ParseDuration(o.Delay)
			if err != nil {
				addf("%s.delay: invalid duration %q: %s", path, o.Delay, err)
			} else if d < 0 {
				addf("%s.delay: must not be negative, got %q", path, o.Delay)
			} else {
				delay = d
			}
		}

		enabled := true
		if o.Enabled != nil {
			enabled = *o.Enabled
		}

		if o.Name != "" && o.URL != "" && o.Key != "" {
			res.Outputs = append(res.Outputs, ResolvedOutput{
				Name:    o.Name,
				URL:     CombineURLKey(o.URL, o.Key),
				Delay:   delay,
				Enabled: enabled,
			})
		}
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("config: invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}

	return res, nil
}

// validatePublicHost — api.public_host: имя или IP без схемы, пути и порта
// (порт всегда берётся из api.addr). IPv6 — как есть или в квадратных
// скобках.
func validatePublicHost(h string) error {
	if strings.Contains(h, "://") {
		return fmt.Errorf("%q: host only, without http:// (e.g. \"192.168.1.12\" or \"studio-pc\")", h)
	}
	if strings.ContainsAny(h, "/?#@ \t") {
		return fmt.Errorf("%q: host only (a name or an IP), no path or spaces", h)
	}
	if _, _, err := net.SplitHostPort(h); err == nil {
		return fmt.Errorf("%q: without a port — the port is taken from api.addr", h)
	}
	return nil
}

// IsLoopbackAddr сообщает, слушает ли addr (host:port) только локальную
// машину. Пустой host (":8080") означает "все интерфейсы" — это НЕ loopback,
// даже если по факту среди них есть 127.0.0.1. Хост, не являющийся ни
// "localhost", ни разбираемым IP (например опечатка), тоже трактуется как
// не-loopback — безопаснее потребовать токен зря, чем не потребовать его,
// когда он на самом деле нужен.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// CombineURLKey склеивает app-URL и ключ потока в единый RTMP URL вида
// rtmp://host/app/key — именно так его ждёт output.NewWorker (ключ последним
// сегментом пути).
func CombineURLKey(url, key string) string {
	return strings.TrimRight(url, "/") + "/" + strings.TrimLeft(key, "/")
}

// ParseSize парсит размер в байтах: голое число или число с суффиксом
// K/M/G либо KB/MB/GB (двоичные единицы, 1024).
func ParseSize(v string) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, fmt.Errorf("empty size")
	}

	mult := int64(1)
	upper := strings.ToUpper(v)

	switch {
	case strings.HasSuffix(upper, "GB"):
		mult, v = 1024*1024*1024, v[:len(v)-2]
	case strings.HasSuffix(upper, "MB"):
		mult, v = 1024*1024, v[:len(v)-2]
	case strings.HasSuffix(upper, "KB"):
		mult, v = 1024, v[:len(v)-2]
	case strings.HasSuffix(upper, "G"):
		mult, v = 1024*1024*1024, v[:len(v)-1]
	case strings.HasSuffix(upper, "M"):
		mult, v = 1024*1024, v[:len(v)-1]
	case strings.HasSuffix(upper, "K"):
		mult, v = 1024, v[:len(v)-1]
	}

	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", v, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("size must not be negative: %q", v)
	}

	return n * mult, nil
}
