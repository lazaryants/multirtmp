package main

import (
	"log/slog"
	"path/filepath"

	"multirtmp/internal/mediamtx"
)

// newMediaMTXSupervisor — супервизор MediaMTX для превью. exeOverride —
// preview.mediamtx_exe: пусто — mediamtx\mediamtx.exe рядом с
// multirtmp.exe; относительный путь — от папки multirtmp.exe (у службы
// рабочая папка — System32, от неё считать бессмысленно). Конфиг и лог
// MediaMTX — в папке его exe.
//
// Если mediamtx.exe нет — предупреждение, а не отказ старта: релей важнее
// превью.
func newMediaMTXSupervisor(exeOverride string, logger *slog.Logger) *mediamtx.Supervisor {
	exeDir := filepath.Dir(mustExecutable())

	exe := exeOverride
	if exe == "" {
		exe = filepath.Join(exeDir, "mediamtx", "mediamtx.exe")
	} else if !filepath.IsAbs(exe) {
		exe = filepath.Join(exeDir, exe)
	}
	dir := filepath.Dir(exe)

	s := mediamtx.New(exe, filepath.Join(dir, "mediamtx.yml"), filepath.Join(dir, "mediamtx.log"), logger)
	if !s.Installed() {
		logger.Warn("main: preview is enabled but MediaMTX was not found — the relay runs normally, only the preview is unavailable",
			"expected_at", exe)
	}
	return s
}
