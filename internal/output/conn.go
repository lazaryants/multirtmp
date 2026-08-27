package output

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/bluenviron/gortmplib"
	"github.com/bluenviron/gortmplib/pkg/bytecounter"
	"github.com/bluenviron/gortmplib/pkg/message"
	"github.com/bluenviron/gortmplib/pkg/rawmessage"
)

const (
	// rtmpMessageStreamID — значение MessageStreamID для единственного
	// активного потока публикации. gortmplib сам всегда использует именно
	// это значение (не то, что вернул createStream) — следуем той же
	// конвенции для совместимости с тем, как peer его уже видел на handshake.
	rtmpMessageStreamID = 0x1000000

	// rtmpChunkSize — размер чанка, который мы согласовываем на handshake
	// (Client.Initialize сам шлёт SetChunkSize с этим значением) и которым
	// пользуется наш собственный rawmessage.Writer для тегов.
	rtmpChunkSize = 65536

	writeTimeout = 5 * time.Second
)

// dataChunkStreamID — тот же chunk stream ID, что gortmplib использует для
// Audio{} и DataAMF0{} (см. pkg/message: AudioChunkStreamID=4).
const dataChunkStreamID = message.AudioChunkStreamID

// chunkStreamIDFor возвращает chunk stream ID для тега данного типа —
// video отдельно (как VideoChunkStreamID), audio и data (onMetaData) вместе,
// как это делает сам gortmplib.
func chunkStreamIDFor(kind uint8) byte {
	if kind == kindVideo {
		return message.VideoChunkStreamID
	}
	return dataChunkStreamID
}

// parseOutputURL превращает привычный плоский RTMP URL
// (rtmp://host:port/app/key) в форму, которую ждёт gortmplib —
// с ключом потока во фрагменте (rtmp://host:port/app#key). Последний
// сегмент пути становится ключом, всё остальное — app.
func parseOutputURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid RTMP URL: %w", err)
	}

	if u.Fragment != "" {
		// уже в форме app#key — ничего конвертировать не нужно.
		return u, nil
	}

	path := strings.Trim(u.Path, "/")
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return nil, fmt.Errorf("RTMP URL must have at least app/streamKey in path: %s", raw)
	}

	u.Path = "/" + path[:idx]
	u.Fragment = path[idx+1:]

	return u, nil
}

// MaskURL разбирает RTMP URL (ключ последним сегментом пути или во
// фрагменте) на host и маскированный намёк на ключ — для логов и сводки
// конфига. Полный ключ наружу никогда не отдаётся (оператору так безопаснее
// править конфиг, и ключ не мелькает в логах целиком).
func MaskURL(raw string) (host, keyHint string) {
	u, err := url.Parse(raw)
	if err != nil {
		return raw, "***"
	}

	key := u.Fragment
	if key == "" {
		path := strings.Trim(u.Path, "/")
		if idx := strings.LastIndex(path, "/"); idx >= 0 {
			key = path[idx+1:]
		} else {
			key = path
		}
	}

	return u.Host, maskKey(key)
}

func maskKey(key string) string {
	if key == "" {
		return "(none)"
	}
	n := 4
	if len(key) < n {
		n = len(key)
	}
	return key[:n] + "…"
}

// conn — установленное RTMP-соединение для публикации: handshake сделан
// через gortmplib.Client, а дальнейшая отправка тегов идёт байт в байт через
// низкоуровневый rawmessage.Writer поверх того же net.Conn, без пересборки
// через типизированный API (важно для sequence header/onMetaData).
type conn struct {
	client *gortmplib.Client
	nc     net.Conn
	rw     *rawmessage.Writer
}

func dial(ctx context.Context, rawURL string) (*conn, error) {
	u, err := parseOutputURL(rawURL)
	if err != nil {
		return nil, err
	}

	client := &gortmplib.Client{URL: u, Publish: true}
	if err := client.Initialize(ctx); err != nil {
		return nil, fmt.Errorf("output: handshake failed: %w", err)
	}

	nc := client.NetConn()
	bc := bytecounter.NewWriter(nc)
	rw := rawmessage.NewWriter(bc, bc, false)
	rw.SetChunkSize(rtmpChunkSize)

	return &conn{client: client, nc: nc, rw: rw}, nil
}

func (c *conn) close() {
	c.client.Close()
}

// sendTag пишет тег байт в байт: ChunkStreamID и Type берутся из kind/тега,
// Timestamp — уже пересчитанный относительно rebase этого соединения.
// SetWriteDeadline обязателен на каждой записи (полумёртвый TCP в Windows
// может висеть в Write минутами).
func (c *conn) sendTag(kind uint8, ts uint32, data []byte) error {
	if err := c.nc.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return fmt.Errorf("output: failed to set write deadline: %w", err)
	}

	return c.rw.Write(&rawmessage.Message{
		ChunkStreamID:   chunkStreamIDFor(kind),
		Timestamp:       time.Duration(ts) * time.Millisecond,
		Type:            kind,
		MessageStreamID: rtmpMessageStreamID,
		Body:            data,
	})
}
