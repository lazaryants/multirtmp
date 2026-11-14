package api

import (
	"encoding/json"
	"net/http"

	"multirtmp/internal/buildinfo"
	"multirtmp/internal/mediamtx"
	"multirtmp/internal/output"
)

type statusResponse struct {
	Build   buildJSON        `json:"build"`
	Network networkJSON      `json:"network"`
	Preview previewJSON      `json:"preview"`
	Source  sourceStatusJSON `json:"source"`
	Buffer  bufferStatusJSON `json:"buffer"`
	Outputs []output.Status  `json:"outputs"`
}

type networkJSON struct {
	LANAccess    bool   `json:"lan_access"`
	FirewallHint string `json:"firewall_hint,omitempty"`
}

type previewJSON struct {
	Enabled  bool             `json:"enabled"`
	URL      string           `json:"url,omitempty"`
	MediaMTX *mediamtx.Status `json:"mediamtx,omitempty"`
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
		Build:   buildJSON{Version: buildinfo.Version, Commit: buildinfo.Commit},
		Network: networkJSON{LANAccess: s.network.LANAccess, FirewallHint: s.network.FirewallHint},
		Preview: s.previewStatus(),
		Source:  sj,
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

// handleOutputStart/handleOutputStop persist the operator's choice as
// enabled in config.json (via outputsStore.SetEnabled), not just the live
// Registry state — see outputs_store.go for why.
func (s *Server) handleOutputStart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	view, err := s.outputs.SetEnabled(name, true)
	if err != nil {
		writeOutputsError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleOutputStop(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	view, err := s.outputs.SetEnabled(name, false)
	if err != nil {
		writeOutputsError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, view)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"ok": false, "error": err.Error()})
}

// handleLANURLs — ссылки для планшета, вычисленные в момент запроса (см.
// NetworkInfo.LANURLs). Пустой список — api.addr loopback, LAN-доступа нет.
func (s *Server) handleLANURLs(w http.ResponseWriter, r *http.Request) {
	var urls []string
	if s.network.LANURLs != nil {
		var err error
		urls, err = s.network.LANURLs()
		if err != nil {
			s.logger.Warn("api: could not detect LAN addresses for the tablet link, set api.public_host", "error", err)
		}
	}
	if urls == nil {
		urls = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"urls": urls})
}

func (s *Server) previewStatus() previewJSON {
	p := previewJSON{Enabled: s.preview.Enabled, URL: s.preview.URL}
	if s.preview.Enabled && s.preview.MediaMTX != nil {
		st := s.preview.MediaMTX()
		p.MediaMTX = &st
	}
	return p
}
