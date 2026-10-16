package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/registry"
)

func TestTelegramManagedBotUsernameGeneration(t *testing.T) {
	for _, test := range []struct {
		name   string
		number int
		want   string
	}{
		{"My Friendly Bot", 0, "my_friendly_bot_bot"},
		{"A", 0, "a_bot"},
		{"News & updates!", 2, "news___updates_2_bot"},
		{"123", 0, "bot_123_bot"},
		{"🤖", 0, "bot___bot"},
		{"Écho", 0, "bot__cho_bot"},
		{strings.Repeat("A", 64), 0, strings.Repeat("a", 28) + "_bot"},
		{strings.Repeat("A", 64), 123, strings.Repeat("a", 25) + "123_bot"},
	} {
		got := telegramManagedBotUsername(test.name, test.number)
		if got != test.want || !validTelegramBotUsername(got) {
			t.Errorf("generate(%q, %d) = %q; want valid %q", test.name, test.number, got, test.want)
		}
	}
}

// These fixtures contain only the structural signals observed in Telegram's
// public profile/contact pages; profile copy is deliberately not interpreted.
func telegramUsernameProfileFixture(username string, exists bool) string {
	if exists {
		return fmt.Sprintf(`<html><head><title>Telegram: Launch @%s</title><meta property="og:title" content="Example"><meta property="og:site_name" content="Telegram"></head><body><div class="tgme_page_title">Example</div><a class="tgme_action_button_new" href="tg://resolve?domain=%s">Start Bot</a></body></html>`, username, username)
	}
	return fmt.Sprintf(`<html><head><title>Telegram: Contact @%s</title><meta property="og:title" content="Telegram: Contact @%s"><meta property="og:site_name" content="Telegram"></head><body><a class="tgme_username_link" href="tg://resolve?domain=%s">@%s</a><a class="tgme_action_button_new" href="tg://resolve?domain=%s">Send Message</a></body></html>`, username, username, username, username, username)
}

func TestTelegramPublicUsernameProfileClassification(t *testing.T) {
	const username = "example_bot"
	generic := telegramUsernameProfileFixture(username, false)
	profile := telegramUsernameProfileFixture(username, true)
	for _, test := range []struct {
		name, body string
		exists     bool
		unverified bool
	}{
		{name: "existing bot", body: profile, exists: true},
		{name: "existing human", body: strings.Replace(profile, "Start Bot", "Send Message", 1), exists: true},
		{name: "existing channel", body: strings.Replace(profile, "Start Bot", "View in Telegram", 1), exists: true},
		{name: "unresolved public profile", body: generic},
		{name: "profile named like contact heading", body: strings.Replace(generic, "<body>", `<body><div class="tgme_page_title">Telegram: Contact @example_bot</div>`, 1), exists: true},
		{name: "empty profile marker", body: strings.Replace(generic, "<body>", `<body><div class="tgme_page_title"></div>`, 1), unverified: true},
		{name: "arbitrary html", body: "<html><title>Welcome</title></html>", unverified: true},
		{name: "malformed html", body: "<html><title", unverified: true},
		{name: "wrong candidate", body: telegramUsernameProfileFixture("another_bot", false), unverified: true},
		{name: "wrong resolve target", body: strings.ReplaceAll(generic, "domain="+username, "domain=other_bot"), unverified: true},
		{name: "profile heading identifies another candidate", body: strings.Replace(profile, "Launch @"+username, "Launch @other_bot", 1), unverified: true},
		{name: "missing action", body: strings.ReplaceAll(generic, "tgme_action_button_new", "unexpected_button"), unverified: true},
		{name: "missing social heading", body: strings.ReplaceAll(generic, "og:title", "unknown:title"), unverified: true},
		{name: "generic heading alone", body: "<title>Telegram: Contact @" + username + "</title>", unverified: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			exists, err := classifyTelegramUsernameProfile([]byte(test.body), username)
			if exists != test.exists || errors.Is(err, ErrTelegramBotUsernameUnverified) != test.unverified || (!test.unverified && err != nil) {
				t.Fatalf("classification = %v, %v; want exists=%v unverified=%v", exists, err, test.exists, test.unverified)
			}
		})
	}
}

type telegramUsernameRoundTripper func(*http.Request) (*http.Response, error)

func (f telegramUsernameRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestTelegramPublicUsernameLookupBoundaries(t *testing.T) {
	for _, test := range []struct {
		name, body, contentType string
		status                  int
		err                     error
		exists, unverified      bool
	}{
		{name: "existing", status: 200, contentType: "text/html; charset=utf-8", body: telegramUsernameProfileFixture("example_bot", true), exists: true},
		{name: "unresolved", status: 200, contentType: "text/html", body: telegramUsernameProfileFixture("example_bot", false)},
		{name: "rate limited", status: 429, contentType: "text/html", body: "rate limited", unverified: true},
		{name: "unavailable", status: 503, contentType: "text/html", unverified: true},
		{name: "redirect", status: 302, contentType: "text/html", unverified: true},
		{name: "wrong content type", status: 200, contentType: "application/json", body: telegramUsernameProfileFixture("example_bot", false), unverified: true},
		{name: "oversized", status: 200, contentType: "text/html", body: strings.Repeat(" ", telegramUsernameProfileBodyLimit) + telegramUsernameProfileFixture("example_bot", false), unverified: true},
		{name: "invalid UTF8", status: 200, contentType: "text/html", body: telegramUsernameProfileFixture("example_bot", false) + "\xff", unverified: true},
		{name: "network error", err: errors.New("secret remote response"), unverified: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			client := &http.Client{Transport: telegramUsernameRoundTripper(func(r *http.Request) (*http.Response, error) {
				requests++
				if r.URL.String() != "https://t.me/example_bot" || r.Method != http.MethodGet || r.Body != nil || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Fatalf("unsafe lookup request: %s %s", r.Method, r.URL)
				}
				if test.err != nil {
					return nil, test.err
				}
				return &http.Response{StatusCode: test.status, Header: http.Header{"Content-Type": {test.contentType}, "Location": {"https://untrusted.example/"}}, Body: io.NopCloser(strings.NewReader(test.body)), Request: r}, nil
			})}
			exists, err := telegramPublicUsernameExists(t.Context(), client, "example_bot")
			if exists != test.exists || errors.Is(err, ErrTelegramBotUsernameUnverified) != test.unverified || (!test.unverified && err != nil) || requests != 1 {
				t.Fatalf("lookup = %v, %v, requests=%d", exists, err, requests)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("lookup exposed remote error details")
			}
		})
	}
	client := &http.Client{Transport: telegramUsernameRoundTripper(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if _, err := telegramPublicUsernameExists(ctx, client, "example_bot"); !errors.Is(err, ErrTelegramBotUsernameUnverified) {
		t.Fatalf("timed out lookup: %v", err)
	}
	client.Transport = telegramUsernameRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("unsafe username reached network")
		return nil, nil
	})
	if _, err := telegramPublicUsernameExists(t.Context(), client, "example_bot/path"); !errors.Is(err, ErrTelegramBotInvalid) {
		t.Fatal("invalid username accepted")
	}
}

func TestTelegramManagedBotAutomaticUsernameChecksAndReserves(t *testing.T) {
	m, store, fake := newTelegramBotManagerTest(t)
	if _, err := m.ConnectTelegramBot(t.Context(), fake.add(222, "Friendly_Bot", false), false); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	pending := registry.TelegramManagedBotIntentRecord{ID: "other-manager", ManagerID: "elsewhere", OwnerID: 7, RequestedUsername: "FRIENDLY1_BOT", DisplayName: "Friendly", LaunchURL: "https://t.me/", Status: "pending", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := store.SaveTelegramManagedBotIntent(t.Context(), pending); err != nil {
		t.Fatal(err)
	}
	var checked []string
	m.usernameExists = func(_ context.Context, username string) (bool, error) {
		checked = append(checked, username)
		return username == "friendly2_bot", nil
	}
	intent, err := m.CreateManagedTelegramBot(t.Context(), "", "", " Friendly ")
	if err != nil {
		t.Fatal(err)
	}
	if intent.RequestedUsername != "friendly3_bot" || intent.DisplayName != "Friendly" || intent.UsernameAvailability != "telegram_confirmation_required" || !strings.Contains(intent.LaunchURL, "/friendly3_bot?name=Friendly") || !reflect.DeepEqual(checked, []string{"friendly2_bot", "friendly3_bot"}) {
		t.Fatalf("automatic creation: %+v, checked=%v", intent, checked)
	}
	intent, err = m.CreateManagedTelegramBot(t.Context(), "", "", "Friendly")
	if err != nil || intent.RequestedUsername != "friendly4_bot" {
		t.Fatalf("pending suggestion was reused: %+v %v", intent, err)
	}
}

func TestTelegramManagedBotAutomaticUsernameFailsWithoutCreatingIntent(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		exists bool
		want   error
		checks int
	}{
		{name: "unverified profile", err: errors.New("private network details"), want: ErrTelegramBotUsernameUnverified, checks: 1},
		{name: "too many collisions", exists: true, want: ErrTelegramBotUsernameUnavailable, checks: telegramManagedBotUsernameChecks},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, store, fake := newTelegramBotManagerTest(t)
			checks := 0
			m.usernameExists = func(context.Context, string) (bool, error) {
				checks++
				return test.exists, test.err
			}
			if _, err := m.CreateManagedTelegramBot(t.Context(), "", "", "Example"); !errors.Is(err, test.want) || strings.Contains(err.Error(), "private") {
				t.Fatalf("creation error: %v", err)
			}
			intents, err := store.TelegramManagedBotIntents(t.Context())
			if err != nil || len(intents) != 0 || fake.count("setWebhook") != 0 || checks != test.checks {
				t.Fatalf("failed lookup changed state: intents=%v err=%v checks=%d", intents, err, checks)
			}
		})
	}
}

func TestTelegramManagedBotAutomaticUsernameRetriesReservationRace(t *testing.T) {
	m, store, _ := newTelegramBotManagerTest(t)
	checks := 0
	m.usernameExists = func(ctx context.Context, username string) (bool, error) {
		checks++
		if checks == 1 {
			now := time.Now().UTC()
			if err := store.SaveTelegramManagedBotIntent(ctx, registry.TelegramManagedBotIntentRecord{ID: "competitor", ManagerID: "elsewhere", OwnerID: 7, RequestedUsername: username, DisplayName: "Example", LaunchURL: "https://t.me/", Status: "pending", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
		}
		return false, nil
	}
	intent, err := m.CreateManagedTelegramBot(t.Context(), "", "", "Example")
	if err != nil || intent.RequestedUsername != "example1_bot" || checks != 2 {
		t.Fatalf("reservation race was not retried: %+v %v checks=%d", intent, err, checks)
	}
}

func TestTelegramManagedBotDisplayNameValidationPrecedesLookup(t *testing.T) {
	m, _, fake := newTelegramBotManagerTest(t)
	m.usernameExists = func(context.Context, string) (bool, error) {
		t.Fatal("invalid display name reached lookup")
		return false, nil
	}
	for _, name := range []string{"", " \t ", strings.Repeat("a", 65), "invalid\xff"} {
		if _, err := m.CreateManagedTelegramBot(t.Context(), "", "", name); !errors.Is(err, ErrTelegramBotInvalid) {
			t.Errorf("invalid display name %q accepted: %v", name, err)
		}
	}
	if _, err := m.CreateManagedTelegramBot(t.Context(), "", "@", "Example"); !errors.Is(err, ErrTelegramBotInvalid) {
		t.Fatal("malformed explicit username was treated as automatic generation")
	}
	if fake.count("getMe") != 0 || fake.count("setWebhook") != 0 {
		t.Fatal("invalid display name reached Telegram")
	}
}
