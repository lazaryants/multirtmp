package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"multirtmp/internal/config"
)

// sizeFlag — размер в байтах; принимает как голое число байт, так и число с
// суффиксом K/M/G или KB/MB/GB (двоичные единицы, 1024). Парсинг — в
// internal/config, чтобы -buffer и buffer.size из config.json понимали
// строки одинаково.
type sizeFlag int64

func (s *sizeFlag) String() string {
	return strconv.FormatInt(int64(*s), 10)
}

func (s *sizeFlag) Set(v string) error {
	n, err := config.ParseSize(v)
	if err != nil {
		return err
	}
	*s = sizeFlag(n)
	return nil
}

// outputSpec — один разобранный "-out url|delay".
type outputSpec struct {
	URL   string
	Delay time.Duration
}

// outputSpecs — повторяемый флаг -out, каждое вхождение добавляется в слайс.
type outputSpecs []outputSpec

func (o *outputSpecs) String() string {
	return fmt.Sprintf("%v", []outputSpec(*o))
}

func (o *outputSpecs) Set(v string) error {
	url, delayStr, ok := strings.Cut(v, "|")
	if !ok {
		return fmt.Errorf(`invalid -out %q, expected format "url|delay"`, v)
	}

	url = strings.TrimSpace(url)
	if url == "" {
		return fmt.Errorf("invalid -out %q: empty url", v)
	}

	delay, err := time.ParseDuration(strings.TrimSpace(delayStr))
	if err != nil {
		return fmt.Errorf("invalid -out %q: invalid delay: %w", v, err)
	}
	if delay < 0 {
		return fmt.Errorf("invalid -out %q: delay must not be negative", v)
	}

	*o = append(*o, outputSpec{URL: url, Delay: delay})
	return nil
}
