package admin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/registry"
	"github.com/iaia/telegramgw/internal/webpush"
)

func (s *Server) webuiPushRoutes() {
	s.mux.HandleFunc("/tgw/api/v1/webui/push/config", s.webuiPushConfig)
	s.mux.HandleFunc("/tgw/api/v1/webui/push/subscribe", s.webuiPushSubscribe)
	s.mux.HandleFunc("/tgw/api/v1/webui/push/unsubscribe", s.webuiPushUnsubscribe)
}

func (s *Server) webPushKeys(ctx context.Context) (registry.WebPushKeys, error) {
	keys, err := s.store.EnsureWebPushKeys(ctx, "", "")
	if !errors.Is(err, registry.ErrWebPushKeysMissing) {
		return keys, err
	}
	candidate, err := webpush.GenerateKeys()
	if err != nil {
		return registry.WebPushKeys{}, err
	}
	return s.store.EnsureWebPushKeys(ctx, candidate.PrivateKey, candidate.PublicKey)
}

func (s *Server) webuiPushConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		method(w)
		return
	}
	if _, ok := s.requireAuth(w, r, false); !ok {
		return
	}
	keys, err := s.webPushKeys(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	subscribed := false
	if value := r.URL.Query().Get("subscription_id"); value != "" {
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil {
			bad(w)
			return
		}
		token, _ := r.Cookie(adminCookie)
		_, err = s.store.GetWebPushSubscription(r.Context(), token.Value, id)
		if err != nil && !errors.Is(err, registry.ErrWebPushSubscriptionNotFound) {
			s.webPushError(w, err)
			return
		}
		subscribed = err == nil
	}
	writeJSON(w, http.StatusOK, map[string]any{"supported": true, "public_key": keys.PublicKey, "scope": "all", "subscribed": subscribed})
}

func (s *Server) webuiPushSubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		method(w)
		return
	}
	if _, ok := s.requireAuth(w, r, true); !ok {
		return
	}
	var in struct {
		Subscription webpush.Subscription `json:"subscription"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if !decode(w, r, &in) {
		return
	}
	if err := webpush.ValidateSubscription(in.Subscription); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "This browser returned an invalid or unsupported push subscription."})
		return
	}
	// Ensure the application key is durable before accepting devices that will
	// need it after a gateway restart.
	if _, err := s.webPushKeys(r.Context()); err != nil {
		fail(w, err)
		return
	}
	token, _ := r.Cookie(adminCookie)
	subscription, err := s.store.UpsertWebPushSubscription(r.Context(), token.Value, registry.WebPushSubscription{Endpoint: in.Subscription.Endpoint, P256DH: in.Subscription.Keys.P256DH, Auth: in.Subscription.Keys.Auth})
	if err != nil {
		s.webPushError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscription_id": subscription.ID.String(), "scope": "all", "subscribed": true})
}

func (s *Server) webuiPushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		method(w)
		return
	}
	if _, ok := s.requireAuth(w, r, true); !ok {
		return
	}
	var in struct {
		SubscriptionID string `json:"subscription_id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if !decode(w, r, &in) {
		return
	}
	id, err := uuid.Parse(in.SubscriptionID)
	if err != nil || id == uuid.Nil {
		bad(w)
		return
	}
	token, _ := r.Cookie(adminCookie)
	if err := s.store.DeleteWebPushSubscription(r.Context(), token.Value, id); err != nil {
		s.webPushError(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (s *Server) webPushError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, registry.ErrAdminSessionInvalid):
		unauthorized(w)
	case errors.Is(err, registry.ErrWebPushSubscriptionNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "This notification subscription is no longer registered."})
	case errors.Is(err, registry.ErrWebPushSubscriptionLimit):
		writeJSON(w, http.StatusConflict, map[string]string{"message": "Too many notification devices are registered. Disable notifications on another device first."})
	case errors.Is(err, registry.ErrWebPushInvalidSubscription):
		bad(w)
	default:
		fail(w, err)
	}
}

// RunWebPush is a single bounded dispatcher in the gateway process. The queue
// itself is durable; disconnecting the page or restarting the gateway does not
// lose accepted completions. Cancellation stops network calls before DB close.
func (s *Server) RunWebPush(ctx context.Context, logger *slog.Logger) {
	keys, err := s.webPushKeys(ctx)
	if err != nil {
		if logger != nil {
			logger.Error("web push initialization failed")
		}
		return
	}
	sender, err := webpush.NewSender(webpush.Keys{PrivateKey: keys.PrivateKey, PublicKey: keys.PublicKey}, s.origin)
	if err != nil {
		if logger != nil {
			logger.Error("web push initialization failed")
		}
		return
	}
	defer sender.Close()
	s.runWebPush(ctx, logger, sender)
}

type webPushSender interface {
	Send(context.Context, webpush.Subscription, uuid.UUID, uuid.UUID, time.Time) string
}

func (s *Server) runWebPush(ctx context.Context, logger *slog.Logger, sender webPushSender) {
	changes, unsubscribe := s.store.SubscribeWebPushNotifications()
	defer unsubscribe()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		// Claim one at a time so a sign-out or passkey revocation is checked
		// immediately before that device's network request, not a large batch.
		more := false
		for range 4 {
			if ctx.Err() != nil {
				return
			}
			deliveries, err := s.store.ClaimWebPushDeliveries(ctx, 1)
			if err != nil {
				if logger != nil {
					logger.Warn("web push queue unavailable")
				}
				break
			}
			if len(deliveries) == 0 {
				more = false
				break
			}
			more = true
			delivery := deliveries[0]
			subscription := webpush.Subscription{Endpoint: delivery.Subscription.Endpoint}
			subscription.Keys.P256DH, subscription.Keys.Auth = delivery.Subscription.P256DH, delivery.Subscription.Auth
			result := sender.Send(ctx, subscription, delivery.EventID, delivery.SessionID, delivery.ExpiresAt)
			if ctx.Err() != nil {
				return
			}
			if err := s.store.FinishWebPushDelivery(ctx, delivery.ID, delivery.LeaseID, result); err != nil && logger != nil {
				logger.Warn("web push delivery checkpoint failed")
			}
		}
		if more {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case _, ok := <-changes:
			if !ok {
				return
			}
		case <-ticker.C:
		}
	}
}
