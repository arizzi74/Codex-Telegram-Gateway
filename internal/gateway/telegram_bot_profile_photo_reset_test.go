package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTelegramBotProfilePhotoResetIncludesPrimaryAndAdditional(t *testing.T) {
	m, _, fake := newTelegramBotManagerTest(t)
	additional, err := m.ConnectTelegramBot(t.Context(), fake.add(222, "ExistingBot", false), false)
	if err != nil {
		t.Fatal(err)
	}
	primary := m.get(m.cfg.Secrets.BotName)
	fake.mu.Lock()
	fake.webhooks[primary.cfg.Secrets.BotToken] = m.webhookURL(primary.record)
	fake.mu.Unlock()
	beforeWebhooks := fake.count("setWebhook")
	for _, id := range []string{primary.record.ID, additional.ID} {
		bot := m.get(id)
		fake.mu.Lock()
		fake.profilePhotos[bot.cfg.Secrets.BotToken] = 1
		fake.mu.Unlock()
		if err := m.ResetTelegramBotProfilePhoto(t.Context(), id); err != nil {
			t.Fatalf("reset %s: %v", id, err)
		}
		fake.mu.Lock()
		uploads := fake.photoUploads[bot.cfg.Secrets.BotToken]
		fake.mu.Unlock()
		if uploads != 1 || !m.get(id).record.Enabled {
			t.Fatalf("reset %s uploads=%d enabled=%v", id, uploads, m.get(id).record.Enabled)
		}
	}
	if fake.count("getUserProfilePhotos") != 0 || fake.count("setWebhook") != beforeWebhooks {
		t.Fatal("explicit reset checked photos instead of overwriting, or changed webhook ownership")
	}
	if fake.count("getWebhookInfo") < 2 || fake.count("setMyProfilePhoto") != 2 {
		t.Fatal("reset did not verify webhooks or upload for each bot")
	}
}

type profileResetRoundTrip func(*http.Request) (*http.Response, error)

func (f profileResetRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestTelegramBotProfilePhotoResetRejectsUnsafeTargets(t *testing.T) {
	for _, test := range []struct {
		name string
		want error
	}{
		{name: "missing", want: ErrTelegramBotNotFound},
		{name: "disabled", want: ErrTelegramBotDisabled},
		{name: "owner changed", want: ErrTelegramBotOwnerChanged},
		{name: "owner mismatch", want: ErrTelegramBotOwnerChanged},
		{name: "foreign webhook", want: ErrTelegramBotProfileWebhookConflict},
		{name: "missing webhook", want: ErrTelegramBotProfileWebhookConflict},
		{name: "identity mismatch"},
		{name: "not bot"},
		{name: "identity unavailable"},
		{name: "webhook unavailable"},
		{name: "upload unavailable"},
		{name: "upload not applied"},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, _, fake := newTelegramBotManagerTest(t)
			token := fake.add(222, "ExistingBot", false)
			summary, err := m.ConnectTelegramBot(t.Context(), token, false)
			if err != nil {
				t.Fatal(err)
			}
			id := summary.ID
			switch test.name {
			case "missing":
				id = "missing"
			case "disabled":
				if err := m.SetTelegramBotEnabled(t.Context(), id, false); err != nil {
					t.Fatal(err)
				}
			case "owner changed":
				bot := *m.get(id)
				bot.record.ManagedBy = m.cfg.Secrets.BotName
				m.put(&bot)
				if code := postManaged(t, m, m.cfg.Secrets.BotName, managedUpdate(201, 99, 222, "ExistingBot"), "original-secret"); code != 200 {
					t.Fatal(code)
				}
			case "owner mismatch":
				bot := *m.get(id)
				bot.record.OwnerID = 99
				m.put(&bot)
			case "foreign webhook", "missing webhook":
				fake.mu.Lock()
				fake.webhooks[token] = "https://foreign.example/botsecret"
				if test.name == "missing webhook" {
					fake.webhooks[token] = ""
				}
				fake.mu.Unlock()
			case "identity mismatch", "not bot":
				fake.mu.Lock()
				identity := fake.identities[token]
				identity.ID = 999
				if test.name == "not bot" {
					identity.ID, identity.IsBot = 222, false
				}
				fake.identities[token] = identity
				fake.mu.Unlock()
			case "identity unavailable":
				fake.mu.Lock()
				delete(fake.identities, token)
				fake.mu.Unlock()
			case "webhook unavailable":
				m.newClient = func(token string) *TelegramClient {
					client := fake.client(token)
					transport := client.http.Transport
					client.http = &http.Client{Transport: profileResetRoundTrip(func(request *http.Request) (*http.Response, error) {
						if strings.HasSuffix(request.URL.Path, "/getWebhookInfo") {
							return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error_code":500,"description":"` + token + `"}`)), Header: http.Header{}}, nil
						}
						return transport.RoundTrip(request)
					})}
					return client
				}
			case "upload unavailable":
				fake.photoErrorCode = 500
			case "upload not applied":
				fake.photoResultFalse = true
			}
			beforeWebhooks := fake.count("setWebhook")
			err = m.ResetTelegramBotProfilePhoto(t.Context(), id)
			if err == nil || (test.want != nil && !errors.Is(err, test.want)) || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "foreign.example") || strings.Contains(err.Error(), "botsecret") {
				t.Fatalf("reset error = %v; want %v", err, test.want)
			}
			fake.mu.Lock()
			uploads := fake.photoUploads[token]
			fake.mu.Unlock()
			if uploads != 0 || fake.count("getUserProfilePhotos") != 0 || fake.count("setWebhook") != beforeWebhooks {
				t.Fatal("failed reset changed a profile or webhook")
			}
			if test.name != "upload unavailable" && test.name != "upload not applied" && fake.count("setMyProfilePhoto") != 0 {
				t.Fatal("unsafe target reached profile upload")
			}
		})
	}
}

func TestTelegramBotProfilePhotoResetCancellationSerializesDisable(t *testing.T) {
	m, _, fake := newTelegramBotManagerTest(t)
	summary, err := m.ConnectTelegramBot(t.Context(), fake.add(222, "ExistingBot", false), false)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	m.newClient = func(token string) *TelegramClient {
		client := fake.client(token)
		transport := client.http.Transport
		client.http = &http.Client{Transport: profileResetRoundTrip(func(request *http.Request) (*http.Response, error) {
			if strings.HasSuffix(request.URL.Path, "/setMyProfilePhoto") {
				deadline, bounded := request.Context().Deadline()
				if !bounded || time.Until(deadline) > 20*time.Second {
					t.Error("reset request had no bounded context")
				}
				close(started)
				<-request.Context().Done()
				return nil, request.Context().Err()
			}
			return transport.RoundTrip(request)
		})}
		return client
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reset := make(chan error, 1)
	go func() { reset <- m.ResetTelegramBotProfilePhoto(ctx, summary.ID) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("reset upload never started")
	}
	if _, _, enabled := m.ResolveTelegramBot(summary.ID); !enabled {
		t.Fatal("in-flight reset blocked the connected bot")
	}
	disabled := make(chan error, 1)
	go func() { disabled <- m.SetTelegramBotEnabled(t.Context(), summary.ID, false) }()
	cancel()
	select {
	case err := <-reset:
		if err == nil || strings.Contains(err.Error(), m.get(summary.ID).cfg.Secrets.BotToken) {
			t.Fatalf("cancelled reset error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled reset did not finish")
	}
	select {
	case err := <-disabled:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent disable did not finish")
	}
	if m.get(summary.ID).record.Enabled || fake.count("setMyProfilePhoto") != 0 {
		t.Fatal("cancelled reset overwrote the concurrent disable")
	}
}
