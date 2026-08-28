package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"multirtmp/internal/output"
)

func (s *Server) handleOutputsList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.outputs.List())
}

func (s *Server) handleOutputsCreate(w http.ResponseWriter, r *http.Request) {
	var in OutputInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	view, err := s.outputs.Add(in)
	if err != nil {
		writeOutputsError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, view)
}

func (s *Server) handleOutputsUpdate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	var in OutputInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	view, err := s.outputs.Update(name, in)
	if err != nil {
		writeOutputsError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleOutputsDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	if err := s.outputs.Delete(name); err != nil {
		writeOutputsError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
}

func writeOutputsError(w http.ResponseWriter, err error) {
	var nf *notFoundError
	if errors.As(err, &nf) {
		writeError(w, http.StatusNotFound, err)
		return
	}

	var ve *validationError
	if errors.As(err, &ve) {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	// ConflictError — start на уже запущенном, stop на уже остановленном:
	// the command is well-formed, just meaningless right now. Explicit 409
	// instead of a silent no-op — see internal/output/registry.go.
	var ce output.ConflictError
	if errors.As(err, &ce) {
		writeError(w, http.StatusConflict, err)
		return
	}

	writeError(w, http.StatusInternalServerError, err)
}
