package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"multirtmp/internal/logrotate"
)

const (
	logMaxBytes = 10 * 1024 * 1024
	logMaxFiles = 5
)

// newLogger открывает ротирующий лог-файл (multirtmp.log, 10MB x5) рядом с
// exeDir и строит логгер, пишущий туда — плюс в stdout, если это не служба
// (под SCM консоли нет, писать в stdout бессмысленно). Уровень — через
// *slog.LevelVar, чтобы можно было поднять/опустить его позже (когда
// известен log.level из конфига), не пересоздавая логгер: до этого момента
// уже что-то может понадобиться залогировать (резолв пути конфига и т.п.).
func newLogger(exeDir string, isService bool, initialLevel slog.Level) (*slog.Logger, *slog.LevelVar, io.Closer, error) {
	logPath := filepath.Join(exeDir, "multirtmp.log")

	rw, err := logrotate.New(logPath, logMaxBytes, logMaxFiles)
	if err != nil {
		return nil, nil, nil, err
	}

	levelVar := &slog.LevelVar{}
	levelVar.Set(initialLevel)

	var w io.Writer = rw
	if !isService {
		w = io.MultiWriter(rw, os.Stdout)
	}

	logger := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: levelVar}))

	return logger, levelVar, rw, nil
}
