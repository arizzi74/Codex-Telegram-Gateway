package admin

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/gateway"
)

// BotAPI deliberately exposes only read operations to the admin dashboard.
type BotAPI interface {
	Identity(context.Context) (gateway.TelegramUser, error)
	GetWebhook(context.Context) (gateway.WebhookInfo, error)
}

type BotInfo struct {
	ID                 int64      `json:"id,omitempty"`
	Username           string     `json:"username,omitempty"`
	DisplayName        string     `json:"display_name,omitempty"`
	ConfiguredUsername string     `json:"configured_username,omitempty"`
	Status             string     `json:"status"`
	WebhookStatus      string     `json:"webhook_status"`
	WebhookURL         string     `json:"webhook_url,omitempty"`
	PendingUpdates     *int       `json:"pending_updates,omitempty"`
	LastError          string     `json:"last_error,omitempty"`
	LastErrorAt        *time.Time `json:"last_error_at,omitempty"`
	CheckedAt          time.Time  `json:"checked_at"`
	AllowedUserCount   int        `json:"allowed_user_count"`
	AllowedChatCount   int        `json:"allowed_chat_count"`
}

type botMonitor struct {
	mu                         sync.Mutex
	api                        BotAPI
	username, expectedWebhook  string
	allowedUsers, allowedChats int
	redactor                   *auth.Redactor
	cached                     BotInfo
}

func newBotMonitor(cfg Config) *botMonitor {
	return &botMonitor{api: cfg.BotAPI, username: strings.TrimPrefix(cfg.BotUsername, "@"),
		expectedWebhook: strings.TrimRight(cfg.Origin, "/") + "/api/v1/telegram/webhook",
		allowedUsers:    cfg.AllowedUserCount, allowedChats: cfg.AllowedChatCount, redactor: cfg.Redactor}
}

func (m *botMonitor) snapshot(ctx context.Context) BotInfo {
	// A dashboard refresh never starts another Telegram request inside this
	// cache window. Concurrent tabs share the same bounded check.
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.cached.CheckedAt.IsZero() && time.Since(m.cached.CheckedAt) < 30*time.Second {
		return m.cached
	}
	info := BotInfo{ConfiguredUsername: m.username, Username: m.username,
		Status: "not_configured", WebhookStatus: "unavailable",
		AllowedUserCount: m.allowedUsers, AllowedChatCount: m.allowedChats}
	if m.api != nil {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		var identity gateway.TelegramUser
		var webhook gateway.WebhookInfo
		var identityErr, webhookErr error
		var checks sync.WaitGroup
		checks.Go(func() { identity, identityErr = m.api.Identity(ctx) })
		checks.Go(func() { webhook, webhookErr = m.api.GetWebhook(ctx) })
		checks.Wait()
		info.Status = "unavailable"
		if identityErr == nil && identity.ID > 0 && identity.IsBot {
			info.ID, info.Username = identity.ID, identity.Username
			info.DisplayName = strings.TrimSpace(identity.FirstName + " " + identity.LastName)
			info.Status = "connected"
		} else {
			info.LastError = "Could not verify the bot with Telegram. Check its credentials and gateway connectivity."
		}
		if webhookErr == nil {
			pending := max(0, webhook.PendingUpdates)
			info.PendingUpdates = &pending
			info.WebhookURL = safeWebhookURL(webhook.URL)
			switch {
			case webhook.URL == "":
				info.WebhookStatus = "not_set"
			case webhook.URL == m.expectedWebhook:
				info.WebhookStatus = "active"
			default:
				info.WebhookStatus = "mismatch"
			}
			if webhook.LastError != "" {
				info.LastError = m.clean(webhook.LastError)
			}
			if webhook.LastErrorDate > 0 {
				at := time.Unix(webhook.LastErrorDate, 0).UTC()
				info.LastErrorAt = &at
			}
		} else if info.LastError == "" {
			info.LastError = "Could not read the Telegram webhook status."
		}
	}
	info.Username = m.clean(info.Username)
	info.ConfiguredUsername = m.clean(info.ConfiguredUsername)
	info.DisplayName = m.clean(info.DisplayName)
	info.WebhookURL = m.clean(info.WebhookURL)
	info.CheckedAt = time.Now().UTC()
	m.cached = info
	return info
}

func (m *botMonitor) clean(value string) string {
	if m.redactor != nil {
		value = m.redactor.Redact(value)
	}
	chars := []rune(value)
	if len(chars) > 600 {
		return string(chars[:600]) + "…"
	}
	return value
}

func safeWebhookURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return ""
	}
	// Webhook destinations may be configured outside this application. Show
	// their destination without basic-auth credentials, queries or fragments.
	u.User, u.RawQuery, u.Fragment, u.ForceQuery = nil, "", "", false
	return u.String()
}
