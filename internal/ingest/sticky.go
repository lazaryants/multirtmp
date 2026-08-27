package ingest

import (
	"log/slog"
	"reflect"
	"sync"
)

// stickyState отслеживает факт получения липких тегов (onMetaData, AVC/AAC
// sequence header) и логирует первое появление и последующие обновления.
type stickyState struct {
	mu            sync.Mutex
	haveMetadata  bool
	haveAVCHeader bool
	haveAACHeader bool
}

func newStickyState() *stickyState {
	return &stickyState{}
}

func (s *stickyState) onMetaData(logger *slog.Logger, meta interface{}) {
	s.mu.Lock()
	first := !s.haveMetadata
	s.haveMetadata = true
	s.mu.Unlock()

	fields := metaDataLogFields(meta)
	if first {
		logger.Info("ingest: received onMetaData", fields...)
	} else {
		logger.Warn("ingest: onMetaData updated again", fields...)
	}
}

func (s *stickyState) onAVCHeader(logger *slog.Logger) {
	s.mu.Lock()
	first := !s.haveAVCHeader
	s.haveAVCHeader = true
	s.mu.Unlock()

	if first {
		logger.Info("ingest: received AVC sequence header")
	} else {
		logger.Warn("ingest: AVC sequence header updated again")
	}
}

func (s *stickyState) onAACHeader(logger *slog.Logger) {
	s.mu.Lock()
	first := !s.haveAACHeader
	s.haveAACHeader = true
	s.mu.Unlock()

	if first {
		logger.Info("ingest: received AAC sequence header")
	} else {
		logger.Warn("ingest: AAC sequence header updated again")
	}
}

// metaDataLogFields достаёт интересующие поля из декодированного onMetaData
// (amf0.ECMAArray или map[string]interface{}) для структурного лога.
func metaDataLogFields(meta interface{}) []any {
	rv := reflect.ValueOf(meta)
	if rv.Kind() != reflect.Map {
		return []any{"raw", meta}
	}

	keys := []string{
		"width", "height", "framerate",
		"videocodecid", "audiocodecid",
		"videodatarate", "audiodatarate",
	}

	fields := make([]any, 0, len(keys)*2)
	for _, k := range keys {
		v := rv.MapIndex(reflect.ValueOf(k))
		if v.IsValid() {
			fields = append(fields, k, v.Interface())
		}
	}

	return fields
}
