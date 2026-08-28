package config

import (
	"fmt"
	"os"
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

  // Status/control HTTP API. 127.0.0.1 only, no authentication — never
  // expose this outside the local machine.
  "api": {
    "addr": "127.0.0.1:8080"
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

  // Outputs. url — without the stream key (the app part only, e.g.
  // "rtmp://a.rtmp.youtube.com/live2"); key — the stream key as its own
  // field, so it never appears in full in the logs. delay — this output's
  // delay relative to the source, changes only by restarting the process.
  // enabled — whether to start it automatically at launch (can also be
  // turned on later via POST /outputs/{name}/start).
  "outputs": [
    {
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
    }
  ]
}
`

// WriteSample создаёт по path образец конфига (с комментариями и
// плейсхолдерами) — вызывается, когда файла ещё нет. Ошибка, если файл уже
// существует: не хотим случайно затереть чужую правку.
func WriteSample(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("config: %s already exists, not overwriting", path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("config: failed to check %s: %w", path, err)
	}

	if err := os.WriteFile(path, []byte(sampleConfig), 0o644); err != nil {
		return fmt.Errorf("config: failed to write sample to %s: %w", path, err)
	}

	return nil
}
