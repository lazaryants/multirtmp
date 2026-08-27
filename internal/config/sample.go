package config

import (
	"fmt"
	"os"
)

// sampleConfig — образец конфига с пояснениями. Комментарии `//` понимает
// наш загрузчик (stripLineComments), но это валидный вход только для него —
// сторонние JSON-парсеры такие комментарии не проглотят.
const sampleConfig = `{
  // Приём RTMP от vMix. addr — куда слушать; source_timeout — сколько без
  // единого тега считается потерей источника (лог уровня Error).
  "ingest": {
    "addr": "127.0.0.1:1935",
    "source_timeout": "5s"
  },

  // Общий кольцевой буфер тегов. size — K/M/G или KB/MB/GB (двоичные, 1024).
  // Ориентир: max_delay * bitrate * 4.
  "buffer": {
    "size": "256MB"
  },

  // HTTP API статуса/управления. Только 127.0.0.1, без аутентификации —
  // наружу не открывать.
  "api": {
    "addr": "127.0.0.1:8080"
  },

  // Выходы. url — без ключа потока (app-часть, например
  // "rtmp://a.rtmp.youtube.com/live2"); key — ключ потока отдельным полем,
  // чтобы не мелькал в логах целиком. delay — задержка этого выхода
  // относительно источника, меняется только рестартом процесса. enabled —
  // стартовать ли автоматически при запуске (можно включить позже через
  // POST /outputs/{name}/start).
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
