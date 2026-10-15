package gateway

import (
	"context"
	"errors"

	"github.com/iaia/telegramgw/internal/registry"
)

type telegramChatAuthorizationStore interface {
	TelegramChatAllowed(context.Context, string, int64) (bool, error)
}

func telegramChatAllowed(chats []int64, chat int64) bool {
	return len(chats) == 0 || containsChat(chats, chat)
}

// authorizeDeliveryDestination checks the actual API destination, not only the
// queue envelope: frozen chunks survive gateway restarts and policy changes.
func (s *Sender) authorizeDeliveryDestination(ctx context.Context, row registry.Delivery, chat int64) error {
	if !s.resolveDeliveryBot(row) {
		return s.suppressUnauthorizedDelivery(ctx, row)
	}
	if row.Kind == "picker_cleanup" || row.Kind == "input_reply_cleanup" {
		return nil // Deleting old content remains safe after access is revoked.
	}
	allowed := telegramChatAllowed(s.options.AllowedChatIDs, chat)
	if allowed {
		if store, ok := s.store.(telegramChatAuthorizationStore); ok {
			bot := row.BotID
			if bot == "" {
				bot = s.options.BotID
			}
			var err error
			allowed, err = store.TelegramChatAllowed(ctx, bot, chat)
			if err != nil {
				return err
			}
		}
	}
	// A policy read can block while the bot is disabled or its token changes.
	// Resolve again at the request boundary, retaining the durable bot ID.
	if allowed {
		allowed = s.resolveDeliveryBot(row) && telegramChatAllowed(s.options.AllowedChatIDs, chat)
	}
	if allowed {
		return nil
	}
	return s.suppressUnauthorizedDelivery(ctx, row)
}

func (s *Sender) resolveDeliveryBot(row registry.Delivery) bool {
	resolver := s.options.BotResolver
	if resolver == nil {
		return true
	}
	cfg, api, ok := resolver.ResolveTelegramBot(row.BotID)
	if !ok || api == nil {
		return false
	}
	s.api = api
	s.options.BotID = row.BotID
	s.options.OwnerID = cfg.Secrets.WLID
	s.options.AllowedChatIDs = cfg.AllowedChatIDs
	return true
}

func (s *Sender) suppressUnauthorizedDelivery(ctx context.Context, row registry.Delivery) error {
	store, ok := s.store.(interface {
		SkipDelivery(context.Context, string) error
	})
	if !ok {
		return errors.New("Telegram unauthorized delivery cancellation unavailable")
	}
	if err := store.SkipDelivery(ctx, row.ID); err != nil {
		return err
	}
	return errSessionDeliverySuppressed
}

func (s *Sender) sendAuthorizedMessage(ctx context.Context, row registry.Delivery, message SendMessage) (int64, error) {
	if err := s.authorizeDeliveryDestination(ctx, row, message.ChatID); err != nil {
		return 0, err
	}
	return s.api.Send(ctx, message)
}
