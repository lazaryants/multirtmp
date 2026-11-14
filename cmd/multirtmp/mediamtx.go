package main

import (
	"log/slog"
	"net"
	"net/url"
	"path/filepath"
	"strings"

	"multirtmp/internal/mediamtx"
)

// previewUDPPort — UDP-порт ICE (медиа WebRTC) MediaMTX из комплектного
// mediamtx.yml (webrtcLocalUDPAddress: :8189). install открывает его в
// брандмауэре для частных сетей: без него превью на планшете не получит
// картинку, хотя сигнализация (через прокси multirtmp) пройдёт.
const previewUDPPort = "8189"

// newMediaMTXSupervisor — супервизор MediaMTX для превью. exeOverride —
// preview.mediamtx_exe: пусто — mediamtx\mediamtx.exe рядом с
// multirtmp.exe; относительный путь — от папки multirtmp.exe (у службы
// рабочая папка — System32, от неё считать бессмысленно). Конфиг и лог
// MediaMTX — в папке его exe. previewBaseURL — preview.url (адрес
// WebRTC-сервера MediaMTX).
//
// Если mediamtx.exe нет — предупреждение, а не отказ старта: релей важнее
// превью.
func newMediaMTXSupervisor(exeOverride, previewBaseURL string, logger *slog.Logger) *mediamtx.Supervisor {
	exeDir := filepath.Dir(mustExecutable())

	exe := exeOverride
	if exe == "" {
		exe = filepath.Join(exeDir, "mediamtx", "mediamtx.exe")
	} else if !filepath.IsAbs(exe) {
		exe = filepath.Join(exeDir, exe)
	}
	dir := filepath.Dir(exe)

	env := func() []string { return mediamtxEnv(previewBaseURL, logger) }

	s := mediamtx.New(exe, filepath.Join(dir, "mediamtx.yml"), filepath.Join(dir, "mediamtx.log"), env, logger)
	if !s.Installed() {
		logger.Warn("main: preview is enabled but MediaMTX was not found — the relay runs normally, only the preview is unavailable",
			"expected_at", exe)
	}
	return s
}

// mediamtxEnv — переменные окружения, которыми multirtmp перекрывает
// mediamtx.yml (MediaMTX читает MTX_<ПАРАМЕТР> поверх файла; проверено на
// v1.21.3). Перекрываются из кода, а не берутся из yml, чтобы поведение не
// зависело от того, какой yml лежит на площадке:
//
//   - MTX_WEBRTCADDRESS=127.0.0.1:<порт из preview.url> — сигнализация WebRTC
//     только на loopback: снаружи её отдаёт прокси multirtmp (/preview/) под
//     тем же токеном, что и интерфейс. В сеть открыт только UDP для медиа.
//   - MTX_WEBRTCIPSFROMINTERFACESLIST — ICE-кандидаты только с адаптера,
//     через который машина выходит в сеть, и с loopback, без VPN-туннелей
//     (см. webrtcInterfaces). Если адаптеры прочитать не удалось — не
//     задаётся, MediaMTX раздаёт кандидаты со всех интерфейсов (работает,
//     просто с лишними кандидатами).
//
// Вызывается перед каждым запуском MediaMTX.
func mediamtxEnv(previewBaseURL string, logger *slog.Logger) []string {
	var env []string

	if u, err := url.Parse(previewBaseURL); err == nil && u.Port() != "" {
		env = append(env, "MTX_WEBRTCADDRESS="+net.JoinHostPort("127.0.0.1", u.Port()))
	}

	adapters, err := readAdapters()
	if err != nil {
		logger.Warn("main: could not list network adapters for the preview server; it will offer all interfaces", "error", err)
		return env
	}
	if names := webrtcInterfaces(adapters); len(names) > 0 {
		env = append(env, "MTX_WEBRTCIPSFROMINTERFACESLIST="+strings.Join(names, ","))
	}
	return env
}
