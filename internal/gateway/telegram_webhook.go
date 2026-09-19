package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/registry"
	"github.com/iaia/telegramgw/internal/telegramcommands"
)

type TelegramRegistry interface {
	AcceptTelegram(context.Context, registry.IncomingUpdate) (registry.AcceptResult, error)
	PrepareTelegramImage(context.Context, registry.IncomingUpdate) (registry.IncomingUpdate, error)
}
type Webhook struct {
	store  TelegramRegistry
	cfg    config.GatewayConfig
	secret string
	api    TelegramAPI
	log    *slog.Logger
}

func NewWebhook(store TelegramRegistry, cfg config.GatewayConfig, secret string, api TelegramAPI, logger *slog.Logger) *Webhook {
	return &Webhook{store, cfg, secret, api, logger}
}

func (h *Webhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !auth.ValidateWebhookSecret(r.Header.Get("X-Telegram-Bot-Api-Secret-Token"), h.secret) {
		http.Error(w, "unauthorized", 401)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "update too large", 413)
		return
	}
	var update TelegramUpdate
	if err = json.Unmarshal(raw, &update); err != nil || update.ID < 0 {
		http.Error(w, "invalid update", 400)
		return
	}
	var from TelegramUser
	var message *TelegramMessage
	if update.Callback != nil {
		from = update.Callback.From
		message = update.Callback.Message
	} else if update.Message != nil && update.Message.From != nil {
		from = *update.Message.From
		message = update.Message
	} else {
		w.WriteHeader(200)
		return
	}
	if !auth.AuthorizedTelegramUser(from.ID, h.cfg.AllowedUserIDs) {
		http.Error(w, "forbidden", 403)
		return
	}
	if message == nil || message.Chat.ID == 0 {
		w.WriteHeader(200)
		return
	}
	if len(h.cfg.AllowedChatIDs) > 0 {
		allowed := false
		for _, id := range h.cfg.AllowedChatIDs {
			if id == message.Chat.ID {
				allowed = true
			}
		}
		if !allowed {
			http.Error(w, "forbidden", 403)
			return
		}
	}
	in := registry.IncomingUpdate{BotID: h.cfg.Secrets.BotName, UpdateID: update.ID, UserID: from.ID, ChatID: message.Chat.ID, TopicID: message.TopicID, Text: message.Text, Raw: raw, CommandTTL: h.cfg.CommandExpiry}
	if message.ReplyTo != nil {
		in.ReplyToMessageID = message.ReplyTo.ID
	}
	if update.Callback != nil {
		if !strings.HasPrefix(update.Callback.Data, "cb:") || len(update.Callback.Data) > 64 {
			http.Error(w, "invalid callback", 400)
			return
		}
		in.CallbackToken = strings.TrimPrefix(update.Callback.Data, "cb:")
	} else if message.hasMedia() {
		// Captions are prompt text, including any leading slash. Never execute
		// a caption as a gateway command while discarding its attachment.
		in.Action = "text"
		in.Text = strings.TrimSpace(message.Caption)
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		in, err = h.store.PrepareTelegramImage(ctx, in)
		cancel()
		if err != nil {
			h.log.Warn("Telegram image target could not be resolved", "telegram_update_id", update.ID)
			http.Error(w, "registry unavailable", 503)
			return
		}
		if in.MediaError == "" {
			ctx, cancel = context.WithTimeout(r.Context(), 15*time.Second)
			in.Images, in.MediaError = h.downloadMessageImage(ctx, message)
			cancel()
		}
		if in.MediaError != "" {
			in.Action = "media_error"
		}
	} else {
		action, target, text, ignore := parseTelegramText(message.Text, h.cfg.Secrets.BotName)
		if ignore {
			w.WriteHeader(200)
			return
		}
		in.Action = action
		in.Target = target
		in.Text = text
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, err = h.store.AcceptTelegram(ctx, in)
	if err != nil {
		h.log.Warn("Telegram update could not be persisted", "telegram_update_id", update.ID, "error", err)
		http.Error(w, "registry unavailable", 503)
		return
	}
	w.WriteHeader(200)
	// Callback spinners do not wait for command execution. Durable feedback is a
	// separate delivery; this best-effort acknowledgement carries no routing state.
	if update.Callback != nil && h.api != nil {
		callbackID := update.Callback.ID
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := h.api.AnswerCallback(ctx, callbackID, ""); err != nil {
				h.log.Debug("callback acknowledgement unavailable")
			}
		}()
	}
}

func parseTelegramText(text, botName string) (action, target, prompt string, ignore bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", "", "", true
	}
	if !strings.HasPrefix(text, "/") {
		return "text", "", text, false
	}
	head, tail := text, ""
	if split := strings.IndexFunc(text, unicode.IsSpace); split >= 0 {
		head, tail = text[:split], text[split:]
	}
	tail = strings.TrimSpace(tail)
	name, mention, hasMention := strings.Cut(strings.TrimPrefix(head, "/"), "@")
	if hasMention && !strings.EqualFold(strings.TrimPrefix(botName, "@"), mention) {
		return "", "", "", true
	}
	name = strings.ToLower(name)
	switch name {
	case "start", "tgstart", "tghelp":
		return "help", "", "", false
	case "tginstances":
		return "instances", "", "", false
	case "tgsessions", "tgstatus":
		return strings.TrimPrefix(name, "tg"), tail, "", false
	case "tgdeletesession":
		return "delete_session", tail, "", false
	case "tgdisconnect":
		return "disconnect", "", "", false
	case "tghistory":
		return "history", "", tail, false
	case "tglastmessages":
		return "last_messages", "", tail, false
	case "tgmultisession":
		return "multisession", "", tail, false
	case "tgsteer":
		return "steer", "", tail, false
	case "tginterrupt":
		return "interrupt", "", "", false
	case "tginput":
		return "input_command", "", tail, false
	}
	// Telegram's native menu only accepts underscores. Typed shortcuts use
	// /-; normalize just the prefix to the same stable stored menu alias.
	if strings.HasPrefix(name, "-") {
		name = "_" + strings.TrimPrefix(name, "-")
	}
	if strings.HasPrefix(name, "_") {
		return "session_alias", name, tail, false
	}
	if canonical, ok := telegramcommands.Canonical(name); ok {
		return "codex", canonical, tail, false
	}
	return "unknown_command", name, "", false
}
