package admin

import (
	"errors"
	"net/http"

	"github.com/iaia/telegramgw/internal/audio"
)

func (s *Server) audioStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.requireAuth(w, r, false); !ok {
		return
	}
	if !s.requireAudio(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.audio.Snapshot())
}

func (s *Server) audioSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.requireFreshAuth(w, r); !ok {
		return
	}
	if !s.requireAudio(w) {
		return
	}
	var input audio.Settings
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if !decode(w, r, &input) {
		return
	}
	if err := s.audio.Apply(input); err != nil {
		switch {
		case errors.Is(err, audio.ErrBusy):
			writeJSON(w, http.StatusConflict, map[string]string{"code": "audio_busy", "message": "Audio settings are already being applied."})
		case errors.Is(err, audio.ErrClosed):
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"code": "audio_unavailable", "message": "Audio settings are unavailable while the gateway is stopping."})
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_audio_settings", "message": err.Error()})
		}
		return
	}
	writeJSON(w, http.StatusAccepted, s.audio.Snapshot())
}

func (s *Server) requireAudio(w http.ResponseWriter) bool {
	if s.audio != nil {
		return true
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"code": "audio_unavailable", "message": "Audio settings are unavailable on this gateway."})
	return false
}
