package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/registry"
)

const maxWebUITelegramBotBody = 1024

// webuiTelegramBots exposes only display names and the configured owner's
// private-chat selection. Telegram identities and destinations remain inside
// the registry; the caller supplies only a session and an opaque bot ID.
func (s *Server) webuiTelegramBots(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.requireAuth(w, r, r.Method == http.MethodPost); !ok {
		return
	}
	var snapshot registry.WebUITelegramBotSnapshot
	var err error
	if r.Method == http.MethodGet {
		snapshot, err = s.store.WebUITelegramBots(r.Context())
	} else {
		session, botID, ok := decodeWebUITelegramBot(w, r)
		if !ok {
			return
		}
		snapshot, err = s.store.SetWebUITelegramBot(r.Context(), session, botID)
	}
	switch {
	case errors.Is(err, registry.ErrTelegramTarget):
		http.Error(w, "Conversation is unavailable. Refresh the session list.", http.StatusNotFound)
	case errors.Is(err, registry.ErrTelegramBotRecordNotFound):
		http.Error(w, "Telegram bot is unavailable. Refresh the bot list.", http.StatusBadRequest)
	case err != nil:
		fail(w, err)
	default:
		writeJSON(w, http.StatusOK, snapshot)
	}
}

func decodeWebUITelegramBot(w http.ResponseWriter, r *http.Request) (uuid.UUID, string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxWebUITelegramBotBody)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	// Read exact keys rather than Go's case-insensitive struct matching, and
	// reject duplicates so a target cannot be interpreted two different ways.
	open, err := decoder.Token()
	if err != nil || open != json.Delim('{') || r.URL.RawQuery != "" {
		bad(w)
		return uuid.Nil, "", false
	}
	values := make(map[string]string, 2)
	for decoder.More() {
		token, err := decoder.Token()
		key, isString := token.(string)
		_, duplicate := values[key]
		var value *string
		if err != nil || !isString || (key != "session_id" && key != "bot_id") || duplicate || decoder.Decode(&value) != nil || value == nil {
			bad(w)
			return uuid.Nil, "", false
		}
		values[key] = *value
	}
	close, err := decoder.Token()
	sessionID, parseErr := uuid.Parse(values["session_id"])
	if err != nil || close != json.Delim('}') || decoder.Decode(&struct{}{}) != io.EOF || len(values) != 2 || parseErr != nil || sessionID == uuid.Nil {
		bad(w)
		return uuid.Nil, "", false
	}
	return sessionID, values["bot_id"], true
}
