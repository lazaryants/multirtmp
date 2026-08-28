package api

import (
	"encoding/json"
	"net/http"

	"multirtmp/internal/buildinfo"
	"multirtmp/internal/output"
)

type statusResponse struct {
	Build   buildJSON        `json:"build"`
	Source  sourceStatusJSON `json:"source"`
	Buffer  bufferStatusJSON `json:"buffer"`
	Outputs []output.Status  `json:"outputs"`
}

type buildJSON struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

type sourceStatusJSON struct {
	Connected         bool     `json:"connected"`
	RemoteAddr        string   `json:"remote_addr,omitempty"`
	App               string   `json:"app,omitempty"`
	StreamKey         string   `json:"stream_key,omitempty"`
	VideoKbps         int64    `json:"video_kbps"`
	AudioKbps         int64    `json:"audio_kbps"`
	FPS               float64  `json:"fps"`
	KeyframeIntervalS *float64 `json:"keyframe_interval_s,omitempty"`
	WatchdogOK        bool     `json:"watchdog_ok"`
	WatchdogGapS      float64  `json:"watchdog_gap_s"`
}

type bufferStatusJSON struct {
	Bytes     int64   `json:"bytes"`
	Tags      int     `json:"tags"`
	DepthS    float64 `json:"depth_s"`
	Keyframes int     `json:"keyframes"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	src := s.ingest.SourceStatus()

	sj := sourceStatusJSON{
		Connected:    src.Connected,
		RemoteAddr:   src.RemoteAddr,
		App:          src.App,
		StreamKey:    src.StreamKey,
		VideoKbps:    src.VideoKbps,
		AudioKbps:    src.AudioKbps,
		FPS:          src.FPS,
		WatchdogOK:   !src.WatchdogLost,
		WatchdogGapS: round(src.WatchdogGapS, 3),
	}
	if src.HaveKeyframeInterval {
		v := src.KeyframeIntervalS
		sj.KeyframeIntervalS = &v
	}

	bufStats := s.buf.Stats()

	resp := statusResponse{
		Build:  buildJSON{Version: buildinfo.Version, Commit: buildinfo.Commit},
		Source: sj,
		Buffer: bufferStatusJSON{
			Bytes:     bufStats.Bytes,
			Tags:      bufStats.Tags,
			DepthS:    round(bufStats.DepthS, 3),
			Keyframes: bufStats.Keyframes,
		},
		Outputs: s.registry.Snapshot(),
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleOutputStart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	if err := s.registry.Start(name); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
}

func (s *Server) handleOutputStop(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	if err := s.registry.Stop(name); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"ok": false, "error": err.Error()})
}
