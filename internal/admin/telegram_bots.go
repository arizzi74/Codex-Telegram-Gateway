package admin

import (
	"context"
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
	RemoveTelegramBot(context.Context, string) error
	CreateManagedTelegramBot(context.Context, string, string, string) (gateway.TelegramManagedBotIntent, error)
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
		var input struct {
			Enabled *bool `json:"enabled"`
		}
		if !decodeEnrollmentJSON(w, r, &input, false) {
			return
		}
		if input.Enabled == nil {
			bad(w)
			return
		}
		err = s.telegramBots.SetTelegramBotEnabled(ctx, id, *input.Enabled)
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
		ManagerID string `json:"manager_id"`
		Username  string `json:"username"`
		Name      string `json:"name"`
	}
	if !decodeEnrollmentJSON(w, r, &input, false) {
		return
	}
	if (input.ManagerID != "" && !validTelegramBotID(input.ManagerID)) || strings.TrimSpace(input.Username) == "" || len(input.Username) > 64 || strings.TrimSpace(input.Name) == "" || len(input.Name) > 256 {
		telegramBotError(w, gateway.ErrTelegramBotInvalid)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	intent, err := s.telegramBots.CreateManagedTelegramBot(ctx, input.ManagerID, strings.TrimSpace(input.Username), strings.TrimSpace(input.Name))
	if err != nil {
		telegramBotError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, intent)
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
	}
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}
