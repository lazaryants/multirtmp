package ingest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/yutopp/go-rtmp"
	rtmpmsg "github.com/yutopp/go-rtmp/message"

	"multirtmp/internal/buffer"
	"multirtmp/internal/notify"
)

const statsInterval = 2 * time.Second

var _ rtmp.Handler = (*handler)(nil)

// handler — обработчик одного RTMP-соединения. Разбирает теги, считает
// статистику и пишет каждый тег в общий кольцевой буфер, откуда его читают
// выходы.
type handler struct {
	rtmp.DefaultHandler

	server        *Server
	remoteAddr    string
	logger        *slog.Logger
	buf           *buffer.Buffer
	sourceTimeout time.Duration
	notifier      *notify.Notifier

	conn *rtmp.Conn

	app       string
	streamKey string

	stats    *stats
	sticky   *stickyState
	watchdog *watchdog

	cancelPublish context.CancelFunc
}

func newHandler(server *Server, remoteAddr string, logger *slog.Logger, buf *buffer.Buffer, sourceTimeout time.Duration, notifier *notify.Notifier) *handler {
	return &handler{
		server:        server,
		remoteAddr:    remoteAddr,
		logger:        logger.With("remote", remoteAddr),
		buf:           buf,
		sourceTimeout: sourceTimeout,
		notifier:      notifier,
	}
}

// recoverPanic защищает горутину соединения от паники в одном колбэке:
// логирует и, если errp != nil, возвращает ошибку вместо падения процесса.
func (h *handler) recoverPanic(where string, errp *error) {
	if r := recover(); r != nil {
		h.logger.Error("ingest: panic recovered", "where", where, "panic", r)
		if errp != nil {
			*errp = fmt.Errorf("ingest: panic in %s: %v", where, r)
		}
	}
}

// closeConn принудительно закрывает соединение — используется, когда его
// вытесняет новый publisher (см. Server.takeOver).
func (h *handler) closeConn() {
	if h.conn != nil {
		_ = h.conn.Close()
	}
}

func (h *handler) OnServe(conn *rtmp.Conn) {
	h.conn = conn
}

func (h *handler) OnConnect(timestamp uint32, cmd *rtmpmsg.NetConnectionConnect) (err error) {
	defer h.recoverPanic("OnConnect", &err)

	h.app = cmd.Command.App
	h.logger.Info("ingest: client connected", "app", h.app)

	return nil
}

func (h *handler) OnPublish(_ *rtmp.StreamContext, timestamp uint32, cmd *rtmpmsg.NetStreamPublish) (err error) {
	defer h.recoverPanic("OnPublish", &err)

	if cmd.PublishingName == "" {
		return fmt.Errorf("ingest: empty stream key")
	}

	h.streamKey = cmd.PublishingName
	h.logger = h.logger.With("stream_key", h.streamKey)
	h.stats = newStats()
	h.sticky = newStickyState()
	h.watchdog = newWatchdog()

	// Новый publish — буфер сбрасывается полностью, старые теги и липкие
	// теги предыдущей публикации больше не актуальны.
	h.buf.Reset()

	// Ровно один активный источник: этот publish вытесняет предыдущий, если он есть.
	h.server.takeOver(h)

	h.logger.Info("ingest: publish started", "app", h.app, "type", cmd.PublishingType)

	ctx, cancel := context.WithCancel(context.Background())
	h.cancelPublish = cancel
	go h.stats.run(ctx, h.logger, statsInterval)
	go h.watchdog.run(ctx, h.logger, h.sourceTimeout, h.notifier, h.streamKey)

	return nil
}

func (h *handler) OnSetDataFrame(timestamp uint32, data *rtmpmsg.NetStreamSetDataFrame) (err error) {
	defer h.recoverPanic("OnSetDataFrame", &err)

	if h.stats != nil {
		h.stats.addOther()
	}
	if h.watchdog != nil {
		h.watchdog.touch()
	}

	_, meta, decErr := decodeMetaData(data.Payload)
	if decErr != nil {
		h.logger.Warn("ingest: failed to parse onMetaData", "error", decErr)
		return nil
	}

	if h.sticky != nil {
		h.sticky.onMetaData(h.logger, meta)
	}

	h.buf.SetMetadata(buffer.Tag{
		Kind:    kindData,
		TS:      timestamp,
		Arrival: time.Now(),
		Data:    data.Payload,
	})

	return nil
}

func (h *handler) OnAudio(timestamp uint32, payload io.Reader) (err error) {
	defer h.recoverPanic("OnAudio", &err)

	data, readErr := io.ReadAll(payload)
	if readErr != nil {
		return fmt.Errorf("ingest: failed to read audio tag: %w", readErr)
	}

	info, parseErr := parseAudioTag(data)
	if parseErr != nil {
		h.logger.Warn("ingest: invalid audio tag", "error", parseErr)
		return nil
	}

	if h.stats != nil {
		h.stats.addAudio(len(data))
	}
	if h.watchdog != nil {
		h.watchdog.touch()
	}

	if info.isAAC && info.isSeqHeader && h.sticky != nil {
		h.sticky.onAACHeader(h.logger)
	}

	tag := buffer.Tag{
		Kind:    kindAudio,
		TS:      timestamp,
		Arrival: time.Now(),
		Data:    data,
	}
	// Sticky-слот обновляем ДО Push: Push будит спящие курсоры, и они не
	// должны увидеть ещё не обновлённый sticky для тега, который сами же разбудили.
	if info.isAAC && info.isSeqHeader {
		h.buf.SetAACHeader(tag)
	}
	h.buf.Push(tag)

	return nil
}

func (h *handler) OnVideo(timestamp uint32, payload io.Reader) (err error) {
	defer h.recoverPanic("OnVideo", &err)

	data, readErr := io.ReadAll(payload)
	if readErr != nil {
		return fmt.Errorf("ingest: failed to read video tag: %w", readErr)
	}

	info, parseErr := parseVideoTag(data)
	if parseErr != nil {
		h.logger.Warn("ingest: invalid video tag", "error", parseErr)
		return nil
	}

	now := time.Now()

	if h.stats != nil {
		h.stats.addVideo(len(data), info.isKeyFrame, timestamp, now)
	}
	if h.watchdog != nil {
		h.watchdog.touch()
	}

	if info.isAVC && info.isSeqHeader && h.sticky != nil {
		h.sticky.onAVCHeader(h.logger)
	}

	tag := buffer.Tag{
		Kind:    kindVideo,
		TS:      timestamp,
		Arrival: now,
		Key:     info.isKeyFrame,
		Data:    data,
	}
	// Sticky-слот обновляем ДО Push: см. комментарий в OnAudio.
	if info.isAVC && info.isSeqHeader {
		h.buf.SetAVCHeader(tag)
	}
	h.buf.Push(tag)

	return nil
}

func (h *handler) OnClose() {
	defer h.recoverPanic("OnClose", nil)

	if h.cancelPublish != nil {
		h.cancelPublish()
	}

	h.server.clearCurrent(h)

	h.logger.Info("ingest: client disconnected")
}
