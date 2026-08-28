package api

import (
	_ "embed"
	"net/http"
)

//go:embed static/index.html
var indexHTML []byte

// handleIndex отдаёт единственную HTML-страницу интерфейса — один файл,
// без внешних CDN/фреймворков (см. CLAUDE.md), встроенный в бинарник через
// go:embed. no-store — чтобы после обновления exe браузер не подсовывал
// закэшированную старую версию интерфейса.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(indexHTML)
}
