package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/iaia/telegramgw/internal/gateway"
)

// TelegramBotManager exposes safe bot metadata and explicit administrative
// operations. Credentials are accepted only on writes and never returned.
type TelegramBotManager interface {
	ListTelegramBots(context.Context) ([]gateway.TelegramBotSummary, error)
	RefreshTelegramBots(context.Context) ([]gateway.TelegramBotSummary, error)
	ListTelegramManagedBotIntents(context.Context) ([]gateway.TelegramManagedBotIntent, error)
	ConnectTelegramBot(context.Context, string, bool) (gateway.TelegramBotSummary, error)
	ReplaceTelegramBotToken(context.Context, string, string, bool) (gateway.TelegramBotSummary, error)
	SetTelegramBotEnabled(context.Context, string, bool) error
	SetTelegramBotSessionName(context.Context, string, bool) error
	SetTelegramBotBroadcastQuestions(context.Context, string, bool) error
	ResetTelegramBotProfilePhoto(context.Context, string) error
	RemoveTelegramBot(context.Context, string) error
	CreateManagedTelegramBot(context.Context, string, string, string) (gateway.TelegramManagedBotIntent, error)
	RemoveTelegramManagedBotIntent(context.Context, string) error
	RetryTelegramManagedBotIntent(context.Context, string) (gateway.TelegramManagedBotIntent, error)
}

func (s *Server) telegramBotCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		method(w)
		return
	}
	if r.Method == http.MethodGet {
		if _, ok := s.requireAuth(w, r, false); !ok {
			return
		}
	} else if _, ok := s.requireFreshAuth(w, r); !ok {
		return
	}
	if !s.requireTelegramBots(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if r.Method == http.MethodGet {
		bots, err := s.telegramBots.ListTelegramBots(ctx)
		if err != nil {
			telegramBotError(w, err)
			return
		}
		s.writeTelegramBotCollection(w, ctx, bots)
		return
	}
	var input struct {
		Token     string `json:"token"`
		ReplaceID string `json:"replace_id,omitempty"`
		Takeover  bool   `json:"takeover"`
	}
	if !decodeEnrollmentJSON(w, r, &input, false) {
		return
	}
	input.Token = strings.TrimSpace(input.Token)
	if input.Token == "" || len(input.Token) > 512 || (input.ReplaceID != "" && !validTelegramBotID(input.ReplaceID)) {
		telegramBotError(w, gateway.ErrTelegramBotInvalid)
		return
	}
	var bot gateway.TelegramBotSummary
	var err error
	status := http.StatusCreated
	if input.ReplaceID != "" {
		bot, err = s.telegramBots.ReplaceTelegramBotToken(ctx, input.ReplaceID, input.Token, input.Takeover)
		status = http.StatusOK
	} else {
		bot, err = s.telegramBots.ConnectTelegramBot(ctx, input.Token, input.Takeover)
	}
	if err != nil {
		telegramBotError(w, err)
		return
	}
	writeJSON(w, status, bot)
}

func (s *Server) telegramBotRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Refreshing Telegram metadata does not change bot configuration and does
	// not require recent authentication, but still requires Origin and CSRF.
	if _, ok := s.requireAuth(w, r, true); !ok {
		return
	}
	if !s.requireTelegramBots(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	bots, err := s.telegramBots.RefreshTelegramBots(ctx)
	if err != nil {
		telegramBotError(w, err)
		return
	}
	s.writeTelegramBotCollection(w, ctx, bots)
}

func (s *Server) writeTelegramBotCollection(w http.ResponseWriter, ctx context.Context, bots []gateway.TelegramBotSummary) {
	pending, err := s.telegramBots.ListTelegramManagedBotIntents(ctx)
	if err != nil {
		telegramBotError(w, err)
		return
	}
	if bots == nil {
		bots = []gateway.TelegramBotSummary{}
	}
	if pending == nil {
		pending = []gateway.TelegramManagedBotIntent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"bots": bots, "pending": pending})
}

func (s *Server) telegramBotMember(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "PATCH, DELETE")
		method(w)
		return
	}
	if _, ok := s.requireFreshAuth(w, r); !ok {
		return
	}
	if !s.requireTelegramBots(w) {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/tgw/api/v1/admin/telegram-bots/")
	if !validTelegramBotID(id) {
		bad(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var err error
	if r.Method == http.MethodPatch {
		var input telegramBotPatchInput
		if !decodeEnrollmentJSON(w, r, &input, false) {
			return
		}
		// Each request changes one setting. Check presence before decoding so
		// a second, null action cannot disguise an ambiguous request.
		if len(input) != 1 {
			bad(w)
			return
		}
		var setting string
		var value json.RawMessage
		for setting, value = range input {
		}
		var enabled *bool
		if json.Unmarshal(value, &enabled) != nil || enabled == nil {
			bad(w)
			return
		}
		switch setting {
		case "inherit_session_name":
			err = s.telegramBots.SetTelegramBotSessionName(ctx, id, *enabled)
		case "broadcast_questions":
			err = s.telegramBots.SetTelegramBotBroadcastQuestions(ctx, id, *enabled)
		case "enabled":
			err = s.telegramBots.SetTelegramBotEnabled(ctx, id, *enabled)
		}
	} else {
		var input struct {
			Confirmed bool `json:"confirmed"`
		}
		if !decodeEnrollmentJSON(w, r, &input, false) {
			return
		}
		if !input.Confirmed {
			bad(w)
			return
		}
		err = s.telegramBots.RemoveTelegramBot(ctx, id)
	}
	if err != nil {
		telegramBotError(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

type telegramBotPatchInput map[string]json.RawMessage

// Reject duplicate and unknown setting names as well as requests containing
// several distinct settings. JSON's usual last-key-wins behavior is ambiguous
// for administrative mutations.
func (input *telegramBotPatchInput) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return gateway.ErrTelegramBotInvalid
	}
	settings := telegramBotPatchInput{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return gateway.ErrTelegramBotInvalid
		}
		key, ok := token.(string)
		if !ok {
			return gateway.ErrTelegramBotInvalid
		}
		switch key {
		case "enabled", "inherit_session_name", "broadcast_questions":
		default:
			return gateway.ErrTelegramBotInvalid
		}
		if _, exists := settings[key]; exists {
			return gateway.ErrTelegramBotInvalid
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return gateway.ErrTelegramBotInvalid
		}
		settings[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return gateway.ErrTelegramBotInvalid
	}
	*input = settings
	return nil
}

func (s *Server) telegramManagedBotCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		method(w)
		return
	}
	if _, ok := s.requireFreshAuth(w, r); !ok {
		return
	}
	if !s.requireTelegramBots(w) {
		return
	}
	var input struct {
		ManagerID   string          `json:"manager_id"`
		DisplayName json.RawMessage `json:"display_name"`
		Username    json.RawMessage `json:"username"`
		Name        json.RawMessage `json:"name"`
	}
	if !decodeEnrollmentJSON(w, r, &input, false) {
		return
	}
	var username, displayName string
	valid := input.ManagerID == "" || validTelegramBotID(input.ManagerID)
	if len(input.DisplayName) != 0 {
		// New clients supply only the display name. Keep the explicit
		// username/name pair for existing clients, rejecting mixed formats.
		var name *string
		valid = valid && len(input.Username) == 0 && len(input.Name) == 0 && json.Unmarshal(input.DisplayName, &name) == nil && name != nil
		if name != nil {
			displayName = *name
		}
	} else {
		var user, name *string
		valid = valid && json.Unmarshal(input.Username, &user) == nil && user != nil && json.Unmarshal(input.Name, &name) == nil && name != nil
		if user != nil && name != nil {
			username, displayName = *user, *name
		}
		valid = valid && strings.TrimSpace(username) != "" && len(username) <= 64
	}
	if !valid || strings.TrimSpace(displayName) == "" || len(displayName) > 256 {
		telegramBotError(w, gateway.ErrTelegramBotInvalid)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	intent, err := s.telegramBots.CreateManagedTelegramBot(ctx, input.ManagerID, strings.TrimSpace(username), strings.TrimSpace(displayName))
	if err != nil {
		telegramBotError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, intent)
}

func (s *Server) telegramBotProfilePhotoReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.requireFreshAuth(w, r); !ok {
		return
	}
	if !s.requireTelegramBots(w) {
		return
	}
	id := r.PathValue("id")
	if !validTelegramBotID(id) {
		bad(w)
		return
	}
	var input struct {
		Confirmed bool `json:"confirmed"`
	}
	if !decodeEnrollmentJSON(w, r, &input, false) {
		return
	}
	if !input.Confirmed {
		bad(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.telegramBots.ResetTelegramBotProfilePhoto(ctx, id); err != nil {
		telegramBotError(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (s *Server) telegramManagedBotIntentRemove(w http.ResponseWriter, r *http.Request) {
	s.telegramManagedBotIntentAction(w, r, false)
}

func (s *Server) telegramManagedBotIntentRetry(w http.ResponseWriter, r *http.Request) {
	s.telegramManagedBotIntentAction(w, r, true)
}

func (s *Server) telegramManagedBotIntentAction(w http.ResponseWriter, r *http.Request, retry bool) {
	verb := http.MethodDelete
	if retry {
		verb = http.MethodPost
	}
	if r.Method != verb {
		w.Header().Set("Allow", verb)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.requireFreshAuth(w, r); !ok {
		return
	}
	if !s.requireTelegramBots(w) {
		return
	}
	id := r.PathValue("id")
	if !validTelegramBotID(id) {
		bad(w)
		return
	}
	var input struct {
		Confirmed bool `json:"confirmed"`
	}
	if !decodeEnrollmentJSON(w, r, &input, false) {
		return
	}
	if !input.Confirmed {
		bad(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if retry {
		intent, err := s.telegramBots.RetryTelegramManagedBotIntent(ctx, id)
		if err != nil {
			telegramBotError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, intent)
		return
	}
	if err := s.telegramBots.RemoveTelegramManagedBotIntent(ctx, id); err != nil {
		telegramBotError(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func validTelegramBotID(id string) bool {
	return id != "" && len(id) <= 160 && !strings.ContainsAny(id, "/\\?#\x00\r\n\t ")
}

func (s *Server) requireTelegramBots(w http.ResponseWriter) bool {
	if s.telegramBots != nil {
		return true
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"code": "telegram_bots_unavailable", "message": "Telegram bot management is unavailable on this gateway."})
	return false
}

// Do not forward Telegram or filesystem error text: either may contain a bot
// token, a webhook secret, or an operator-only path.
func telegramBotError(w http.ResponseWriter, err error) {
	status, code, message := http.StatusBadGateway, "telegram_bot_unavailable", "Could not complete the Telegram bot operation. Check connectivity and retry."
	switch {
	case errors.Is(err, gateway.ErrTelegramBotInvalid):
		status, code, message = http.StatusBadRequest, "invalid_telegram_bot", "Check the bot token, username, and display name."
	case errors.Is(err, gateway.ErrTelegramBotNotFound):
		status, code, message = http.StatusNotFound, "telegram_bot_not_found", "This bot is no longer connected. Refresh the list."
	case errors.Is(err, gateway.ErrTelegramManagedBotIntentConflict):
		status, code, message = http.StatusConflict, "telegram_managed_bot_intent_conflict", "This request is active or already linked to a bot and cannot be removed or retried. Refresh the list."
	case errors.Is(err, gateway.ErrTelegramBotDuplicate):
		status, code, message = http.StatusConflict, "telegram_bot_duplicate", "This bot is already connected or has a pending creation request."
	case errors.Is(err, gateway.ErrTelegramBotLegacy):
		status, code, message = http.StatusConflict, "telegram_bot_legacy", "The primary bot is managed by the gateway configuration. Update its credentials there."
	case errors.Is(err, gateway.ErrTelegramBotWebhookConflict):
		writeJSON(w, http.StatusConflict, map[string]any{"code": "telegram_bot_webhook_conflict", "message": "This bot is connected to another webhook. Confirm replacement to connect it here.", "takeover_required": true})
		return
	case errors.Is(err, gateway.ErrTelegramBotManagerUnavailable):
		status, code, message = http.StatusConflict, "telegram_bot_manager_unavailable", "Telegram still reports that the selected bot cannot manage bots after refreshing its profile. Enable Bot Management Mode for that bot in BotFather, then retry."
	case errors.Is(err, gateway.ErrTelegramBotOwnerChanged):
		status, code, message = http.StatusConflict, "telegram_bot_owner_changed", "This managed bot's owner changed. Wait for Telegram to verify the configured owner again, or remove it and explicitly reconnect it with its current token."
	case errors.Is(err, gateway.ErrTelegramBotDisabled):
		status, code, message = http.StatusConflict, "telegram_bot_disabled", "Enable this bot before resetting its profile photo."
	case errors.Is(err, gateway.ErrTelegramBotProfileWebhookConflict):
		status, code, message = http.StatusConflict, "telegram_bot_profile_webhook_conflict", "This bot's webhook no longer belongs to this gateway. Reconnect it here before resetting its profile photo."
	case errors.Is(err, gateway.ErrTelegramBotUsernameUnverified):
		status, code, message = http.StatusBadGateway, "telegram_bot_username_unverified", "Could not check Telegram usernames. Retry when Telegram is available."
	case errors.Is(err, gateway.ErrTelegramBotUsernameUnavailable):
		status, code, message = http.StatusConflict, "telegram_bot_username_unavailable", "Could not find an unused username within the attempt limit. Try a different display name."
	}
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}
