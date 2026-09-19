package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/registry"
)

type presenceStore struct {
	targets []registry.TelegramTypingTarget
	err     error
}

func (s *presenceStore) ListTelegramTypingTargets(context.Context, int) ([]registry.TelegramTypingTarget, error) {
	return append([]registry.TelegramTypingTarget(nil), s.targets...), s.err
}

type presenceAPI struct {
	mu      sync.Mutex
	actions []ChatAction
	err     error
}

func (a *presenceAPI) SendChatAction(_ context.Context, action ChatAction) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.actions = append(a.actions, action)
	return a.err
}

func (a *presenceAPI) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.actions)
}

func TestPresenceRefreshesActiveTargetsWithoutAPIRetrySpam(t *testing.T) {
	target := registry.TelegramTypingTarget{BotID: "bot", ChatID: 42, TopicID: 9}
	store := &presenceStore{targets: []registry.TelegramTypingTarget{target}}
	api := &presenceAPI{err: errors.New("temporary Telegram failure")}
	presence := NewPresence(store, api, nil)
	now := time.Unix(100, 0)

	if err := presence.flush(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if api.count() != 1 || api.actions[0] != (ChatAction{ChatID: 42, TopicID: 9, Action: "typing"}) {
		t.Fatalf("first typing action = %#v", api.actions)
	}
	if err := presence.flush(context.Background(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if api.count() != 1 {
		t.Fatalf("API failure was retried at poll rate: %d calls", api.count())
	}
	if err := presence.flush(context.Background(), now.Add(typingRefreshInterval)); err != nil {
		t.Fatal(err)
	}
	if api.count() != 2 {
		t.Fatalf("typing action was not refreshed: %d calls", api.count())
	}

	store.targets = nil
	if err := presence.flush(context.Background(), now.Add(typingRefreshInterval+time.Second)); err != nil {
		t.Fatal(err)
	}
	store.targets = []registry.TelegramTypingTarget{target}
	if err := presence.flush(context.Background(), now.Add(typingRefreshInterval+2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if api.count() != 3 {
		t.Fatalf("re-activated target did not send immediately: %d calls", api.count())
	}
}

func TestPresenceRegistryErrorsAreReturnedWithoutSending(t *testing.T) {
	want := errors.New("registry unavailable")
	store := &presenceStore{err: want}
	api := &presenceAPI{}
	presence := NewPresence(store, api, nil)
	if err := presence.flush(context.Background(), time.Now()); !errors.Is(err, want) {
		t.Fatalf("flush error = %v, want %v", err, want)
	}
	if api.count() != 0 {
		t.Fatal("typing action sent without registry state")
	}
}

type changingPresenceStore struct {
	*presenceStore
	active bool
	errNow error
}

func (s *changingPresenceStore) IsTelegramTypingTargetActive(context.Context, registry.TelegramTypingTarget) (bool, error) {
	return s.active, s.errNow
}

func TestPresenceRechecksSelectionImmediatelyBeforeTyping(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{{name: "switched to idle session"}, {name: "current selection unavailable", err: errors.New("registry unavailable")}} {
		t.Run(test.name, func(t *testing.T) {
			store := &changingPresenceStore{presenceStore: &presenceStore{targets: []registry.TelegramTypingTarget{{BotID: "bot", ChatID: 42}}}, errNow: test.err}
			api := &presenceAPI{}
			presence := NewPresence(store, api, nil)
			if err := presence.flush(context.Background(), time.Now()); err != nil {
				t.Fatal(err)
			}
			if api.count() != 0 {
				t.Fatal("typing continued after the active session changed")
			}
		})
	}
}
