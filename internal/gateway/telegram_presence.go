package gateway

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/iaia/telegramgw/internal/registry"
)

const (
	typingPollInterval    = time.Second
	typingRefreshInterval = 4 * time.Second
	typingRequestTimeout  = 3 * time.Second
	typingTargetLimit     = 100
	typingConcurrency     = 8
)

type TelegramPresenceStore interface {
	ListTelegramTypingTargets(context.Context, int) ([]registry.TelegramTypingTarget, error)
}

type telegramPresenceVisibilityStore interface {
	IsTelegramTypingTargetActive(context.Context, registry.TelegramTypingTarget) (bool, error)
}

type TelegramChatActionAPI interface {
	SendChatAction(context.Context, ChatAction) error
}

type PresenceOptions struct {
	AllowedChatIDs []int64
}

// Presence periodically renews Telegram's short-lived typing indicator from
// durable registry state. API failures are best effort and never affect the
// underlying command or delivery lifecycle.
type Presence struct {
	store   TelegramPresenceStore
	api     TelegramChatActionAPI
	log     *slog.Logger
	last    map[registry.TelegramTypingTarget]time.Time
	options PresenceOptions
}

func NewPresence(store TelegramPresenceStore, api TelegramChatActionAPI, logger *slog.Logger, options ...PresenceOptions) *Presence {
	if logger == nil {
		logger = slog.Default()
	}
	var option PresenceOptions
	if len(options) > 0 {
		option = options[0]
	}
	return &Presence{store: store, api: api, log: logger, last: make(map[registry.TelegramTypingTarget]time.Time), options: option}
}

func (p *Presence) Run(ctx context.Context) error {
	ticker := time.NewTicker(typingPollInterval)
	defer ticker.Stop()
	for {
		if err := p.flush(ctx, time.Now()); err != nil && ctx.Err() == nil {
			p.log.Warn("Telegram typing target refresh failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (p *Presence) flush(ctx context.Context, now time.Time) error {
	targets, err := p.store.ListTelegramTypingTargets(ctx, typingTargetLimit)
	if err != nil {
		return err
	}
	active := make(map[registry.TelegramTypingTarget]struct{}, len(targets))
	due := make([]registry.TelegramTypingTarget, 0, len(targets))
	for _, target := range targets {
		if !telegramChatAllowed(p.options.AllowedChatIDs, target.ChatID) {
			continue
		}
		active[target] = struct{}{}
		if sent, ok := p.last[target]; !ok || now.Sub(sent) >= typingRefreshInterval {
			// Record attempts as well as successes so a failing Telegram endpoint
			// cannot turn the poll cadence into an API retry storm.
			p.last[target] = now
			due = append(due, target)
		}
	}
	for target := range p.last {
		if _, ok := active[target]; !ok {
			delete(p.last, target)
		}
	}
	if p.api == nil || len(due) == 0 {
		return nil
	}

	semaphore := make(chan struct{}, typingConcurrency)
	var group sync.WaitGroup
	for _, target := range due {
		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			group.Wait()
			return nil
		}
		group.Add(1)
		go func(target registry.TelegramTypingTarget) {
			defer group.Done()
			defer func() { <-semaphore }()
			requestCtx, cancel := context.WithTimeout(ctx, typingRequestTimeout)
			defer cancel()
			// A chat may switch to an idle session while another typing request
			// occupies the semaphore. Recheck the selection at the send boundary.
			if store, ok := p.store.(telegramPresenceVisibilityStore); ok {
				active, err := store.IsTelegramTypingTargetActive(requestCtx, target)
				if err != nil || !active {
					return
				}
			}
			// Recheck after queueing and visibility reads, at the actual request.
			if !telegramChatAllowed(p.options.AllowedChatIDs, target.ChatID) {
				return
			}
			if store, ok := p.store.(telegramChatAuthorizationStore); ok {
				allowed, err := store.TelegramChatAllowed(requestCtx, target.BotID, target.ChatID)
				if err != nil || !allowed {
					return
				}
			}
			if err := p.api.SendChatAction(requestCtx, ChatAction{ChatID: target.ChatID, TopicID: target.TopicID, Action: "typing"}); err != nil && ctx.Err() == nil {
				p.log.Debug("Telegram typing indicator unavailable", "chat_id", target.ChatID, "topic_id", target.TopicID, "error", err)
			}
		}(target)
	}
	group.Wait()
	return nil
}
