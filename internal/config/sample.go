package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// sampleConfig — образец конфига с пояснениями. Комментарии `//` понимает
// наш загрузчик (stripLineComments), но это валидный вход только для него —
// сторонние JSON-парсеры такие комментарии не проглотят.
const sampleConfig = `{
  // RTMP ingest from vMix. addr — where to listen; source_timeout — how long
  // without a single tag counts as a lost source (logged at Error level).
  "ingest": {
    "addr": "127.0.0.1:1935",
    "source_timeout": "5s"
  },

  // Shared ring buffer of tags. If "size" is omitted (as here), it is
  // auto-sized: (max delay among outputs + 60s) * measured source bitrate,
  // clamped to [32MB, max_size]. Recalculated every 2s while publishing; a
  // "buffer: resized" log line appears whenever it actually changes. Set
  // "size" explicitly (e.g. "256MB") to disable autosizing and pin it for good.
  "buffer": {
    "max_size": "512MB"
  },

  // Status/control HTTP API + web UI. "addr": "127.0.0.1:8080" (default) is
  // local-only, no token required. To reach it from another device on the
  // LAN (e.g. a tablet), set addr to this machine's LAN IP or "0.0.0.0:8080"
  // — "token" then becomes required (refuses to start otherwise) and must be
  // sent as "Authorization: Bearer <token>" or "?token=<token>". A random
  // token is already filled in below so it's ready whenever you need it.
  // Requests from this computer itself never need the token.
  // "public_host" (optional): the name or IP other devices use to reach this
  // computer, e.g. "192.168.1.12" or "studio-pc" — no http://, no port. If
  // set, "Show link for tablet" uses only it; if empty, the links are built
  // from this computer's network adapters (VPN tunnels are skipped).
  "api": {
    "addr": "__API_ADDR__",
    "token": "__TOKEN__",
    "public_host": ""
  },

  // Log level for the file (next to the exe, rotated 10MB x5) and stdout
  // (when not running as a service). "debug" turns on the every-2s stats
  // lines — noticeable on disk over a day, keep "info" outside of debugging.
  "log": {
    "level": "info"
  },

  // Telegram alerts: source lost/recovered, output down/up, more than 5
  // reconnects per minute on one output, lag_s exceeding delay+30s. At most
  // one message per event per 60s. Leave both fields empty to disable
  // alerts entirely — that's not an error, just off.
  "notify": {
    "telegram_bot_token": "",
    "telegram_chat_id": ""
  },

  // Optional live preview player, embedded in the web UI below the status
  // section. multirtmp itself never decodes or re-packages anything for
  // this — the preview stream is just another entry in "outputs" below
  // (e.g. url "rtmp://127.0.0.1:19350/live", key "preview"), fed to
  // MediaMTX, which serves it back out over WebRTC. "url" is that MediaMTX's
  // WebRTC address, "path" is the stream's path on it ("live/preview" for
  // the example above) — the player embeds "<url>/<path>".
  // When "enabled" is true, multirtmp starts MediaMTX itself and restarts it
  // if it stops: by default mediamtx\mediamtx.exe next to multirtmp.exe,
  // with mediamtx.yml from the same folder; "mediamtx_exe" overrides the
  // path to mediamtx.exe (empty = default). If mediamtx.exe is missing,
  // multirtmp still runs normally — only the preview is unavailable.
  // Leave "enabled": false (default) to skip the preview entirely — on
  // weaker hardware, or when a preview simply isn't needed on site.
  "preview": {
    "enabled": __PREVIEW_ENABLED__,
    "url": "http://127.0.0.1:8889",
    "path": "__PREVIEW_PATH__",
    "mediamtx_exe": ""
  },

  // Outputs. url — without the stream key (the app part only, e.g.
  // "rtmp://a.rtmp.youtube.com/live2"); key — the stream key as its own
  // field, so it never appears in full in the logs. delay — this output's
  // delay relative to the source, changes only by restarting the process.
  // enabled — whether to start it automatically at launch (can also be
  // turned on later via POST /outputs/{name}/start). An output with an
  // empty url or key (or "REPLACE_ME") is "not configured": it never
  // connects and shows as NOT CONFIGURED in the web UI until you enter
  // its server URL and stream key there.
  "outputs": [
__OUTPUTS__
  ]
}
`

// sampleOutputs — назначения образца при ручном запуске exe без конфига.
const sampleOutputs = `    {
      "name": "corp",
      "url": "rtmp://127.0.0.1:19350/live",
      "key": "REPLACE_ME",
      "delay": "0s",
      "enabled": true
    },
    {
      "name": "youtube",
      "url": "rtmp://a.rtmp.youtube.com/live2",
      "key": "REPLACE_ME",
      "delay": "15s",
      "enabled": true
    }`

// installOutputs — назначения начального конфига, который создаёт install
// на площадке: youtube и company не настроены (ключи вводит оператор в
// интерфейсе), preview сразу работает через MediaMTX из комплекта.
const installOutputs = `    {
      "name": "youtube",
      "url": "rtmp://a.rtmp.youtube.com/live2",
      "key": "",
      "delay": "15s",
      "enabled": false
    },
    {
      "name": "company",
      "url": "",
      "key": "",
      "delay": "0s",
      "enabled": false
    },
    {
      "name": "preview",
      "url": "rtmp://127.0.0.1:19350/live",
      "key": "preview",
      "delay": "0s",
      "enabled": true
    }`

// WriteSample создаёт по path образец конфига (с комментариями и
// плейсхолдерами) — вызывается, когда exe запущен вручную, а файла ещё нет.
// Ошибка, если файл уже существует: не хотим случайно затереть чужую правку.
func WriteSample(path string) error {
	err := writeConfig(path, "127.0.0.1:8080", false, "preview", sampleOutputs)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("config: %s already exists, not overwriting", path)
	}
	return err
}

// WriteInstallConfig создаёт начальный config.json для площадки (его зовёт
// install): API доступен из локальной сети (0.0.0.0:8080) со своим случайным
// токеном на каждой машине, превью включено, назначения youtube и company
// не настроены (ключи вводит оператор в интерфейсе), preview — настроено.
//
// Существующий файл не трогается никогда — на площадках в нём ключи потоков:
// created == false, err == nil. Создание через O_EXCL, так что файл,
// появившийся между проверкой и записью, тоже не будет перезаписан.
func WriteInstallConfig(path string) (created bool, err error) {
	err = writeConfig(path, "0.0.0.0:8080", true, "live/preview", installOutputs)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func writeConfig(path, apiAddr string, previewEnabled bool, previewPath, outputs string) error {
	token, err := generateToken()
	if err != nil {
		return fmt.Errorf("config: failed to generate api token: %w", err)
	}

	content := strings.NewReplacer(
		"__API_ADDR__", apiAddr,
		"__TOKEN__", token,
		"__PREVIEW_ENABLED__", fmt.Sprint(previewEnabled),
		"__PREVIEW_PATH__", previewPath,
		"__OUTPUTS__", outputs,
	).Replace(sampleConfig)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return err
		}
		return fmt.Errorf("config: failed to create %s: %w", path, err)
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("config: failed to write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("config: failed to write %s: %w", path, err)
	}
	return nil
}

// generateToken делает случайный токен для api.token — 24 байта из
// crypto/rand, base64url без паддинга даёт ровно 32 символа. Заполняется
// сразу, даже при loopback-адресе (он не обязателен, пока addr не сменили на
// LAN), чтобы оператору не пришлось потом придумывать его самому.
func generateToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
