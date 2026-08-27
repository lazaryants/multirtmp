// Package config загружает и валидирует config.json: адреса ingest/API,
// размер буфера, список выходов. Невалидный конфиг — отказ стартовать, а не
// тихая работа с умолчаниями (кроме полей, у которых умолчание определено
// явно и описано в примере конфига).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultIngestAddr    = "127.0.0.1:1935"
	DefaultSourceTimeout = 5 * time.Second
	DefaultBufferSize    = 256 * 1024 * 1024
	DefaultAPIAddr       = "127.0.0.1:8080"
)

// Config — конфиг как он есть в JSON-файле.
type Config struct {
	Ingest  IngestConfig   `json:"ingest"`
	Buffer  BufferConfig   `json:"buffer"`
	API     APIConfig      `json:"api"`
	Outputs []OutputConfig `json:"outputs"`
}

type IngestConfig struct {
	Addr          string `json:"addr"`
	SourceTimeout string `json:"source_timeout"`
}

type BufferConfig struct {
	Size string `json:"size"`
}

type APIConfig struct {
	Addr string `json:"addr"`
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
	BufferSize    int64
	APIAddr       string
	Outputs       []ResolvedOutput
}

// ResolvedOutput — один выход. URL уже включает ключ потока (см.
// CombineURLKey) — готов к передаче в output.NewWorker как есть.
type ResolvedOutput struct {
	Name    string
	URL     string
	Delay   time.Duration
	Enabled bool
}

// Load читает файл по path, снимает `//`-комментарии, разбирает JSON,
// применяет умолчания, валидирует и возвращает уже готовый к использованию
// Resolved. Если что-то не так — возвращает ошибку со всеми найденными
// проблемами разом (не только первой), с номером строки для синтаксических
// ошибок JSON и путём поля (например "outputs[1].name") для смысловых.
func Load(path string) (*Resolved, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: failed to read %s: %w", path, err)
	}

	stripped := stripLineComments(raw)

	var cfg Config
	if err := json.Unmarshal(stripped, &cfg); err != nil {
		return nil, fmt.Errorf("config: %s", describeJSONError(stripped, err))
	}

	return resolve(&cfg)
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

	// buffer.size
	if cfg.Buffer.Size == "" {
		res.BufferSize = DefaultBufferSize
	} else if n, err := ParseSize(cfg.Buffer.Size); err != nil {
		addf("buffer.size: %s", err)
	} else if n <= 0 {
		addf("buffer.size: must be positive, got %q", cfg.Buffer.Size)
	} else {
		res.BufferSize = n
	}

	// api.addr
	res.APIAddr = cfg.API.Addr
	if res.APIAddr == "" {
		res.APIAddr = DefaultAPIAddr
	}
	if _, _, err := net.SplitHostPort(res.APIAddr); err != nil {
		addf("api.addr: invalid address %q: %s", res.APIAddr, err)
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
