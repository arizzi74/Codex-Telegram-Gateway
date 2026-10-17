package gateway

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/registry"
)

const telegramBotNamesInterval = 5 * time.Second

type telegramBotNameState struct {
	preference registry.TelegramBotNamePreference
	// The creation intent can recover a clean base from a historical capture
	// that already included a session. Keep that source separate from the
	// immutable capture, which remains the fallback for externally created bots.
	baseName  *string
	pending   bool
	lastError string
	retryAt   time.Time
	failures  int
}

func (s telegramBotNameState) originalDisplayName() *string {
	if s.baseName != nil {
		return s.baseName
	}
	return s.preference.OriginalDisplayName
}

func (s telegramBotNameState) needsNameRestore() bool {
	original := s.originalDisplayName()
	applied := s.preference.AppliedDisplayName
	return s.preference.OriginalDisplayName != nil && (applied == nil || *applied != *original)
}

func (m *TelegramBotManager) loadBotNameBase(ctx context.Context, id string, state *telegramBotNameState) error {
	base, err := m.store.TelegramBotCreationName(ctx, id, m.cfg.Secrets.WLID)
	if err != nil {
		return err
	}
	if base == nil {
		base = state.preference.OriginalDisplayName
	}
	state.baseName = base
	return nil
}

func (m *TelegramBotManager) botNameState(id string) telegramBotNameState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.botNames[id]
}

func (m *TelegramBotManager) putBotNameState(id string, state telegramBotNameState) {
	m.mu.Lock()
	m.botNames[id] = state
	m.mu.Unlock()
}

// SetBotNameRedactor applies the gateway's outbound redaction policy to composed
// names before truncation. The resolved base is restored exactly.
func (m *TelegramBotManager) SetBotNameRedactor(redactor *auth.Redactor) {
	m.op.Lock()
	m.botNameRedactor = redactor
	m.op.Unlock()
}

// SetTelegramBotSessionName changes only the durable preference. Telegram API
// calls run in RunBotNames, so a transient failure never loses the admin's choice
// and selecting a session never waits for a remote profile update.
func (m *TelegramBotManager) SetTelegramBotSessionName(ctx context.Context, id string, enabled bool) error {
	m.op.Lock()
	defer m.op.Unlock()
	if m.get(id) == nil {
		return ErrTelegramBotNotFound
	}
	if err := m.store.SetTelegramBotSessionName(ctx, id, enabled); err != nil {
		return errors.New("Telegram bot name preference could not be saved")
	}
	state := m.botNameState(id)
	state.preference.BotID = id
	state.preference.InheritSessionName = enabled
	state.pending = enabled || state.needsNameRestore()
	state.lastError = ""
	m.putBotNameState(id, state)
	select {
	case m.botNamesWake <- struct{}{}:
	default:
	}
	return nil
}

// RunBotNames follows the owner's private-chat selection and session renames.
// Manager mutations are serialized by op; ordinary webhook acceptance uses only
// short read locks and remains independent of Telegram profile API latency.
func (m *TelegramBotManager) RunBotNames(ctx context.Context) {
	ticker := time.NewTicker(telegramBotNamesInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		m.syncBotNames(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-m.botNamesWake:
		}
	}
}

func (m *TelegramBotManager) syncBotNames(ctx context.Context) {
	m.mu.RLock()
	ids := make([]string, 0, len(m.botNames))
	for id := range m.botNames {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	sort.Strings(ids)
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		m.op.Lock()
		if ctx.Err() == nil {
			m.syncBotName(ctx, id, time.Now())
		}
		m.op.Unlock()
	}
}

// syncBotName is called with op held, including during API calls, so a profile
// result cannot overwrite a concurrent disable, token replacement or removal.
func (m *TelegramBotManager) syncBotName(ctx context.Context, id string, now time.Time) {
	bot := m.get(id)
	state := m.botNameState(id)
	p := &state.preference
	if bot == nil || !bot.record.Enabled || bot.record.LastError == "owner_changed" || now.Before(state.retryAt) {
		return
	}
	if !p.InheritSessionName && p.OriginalDisplayName == nil {
		state.pending, state.lastError = false, ""
		m.putBotNameState(id, state)
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	fail := func(code string, err error) {
		if ctx.Err() != nil {
			return
		}
		state.pending, state.lastError = true, code
		state.failures++
		delay := telegramBotNamesInterval * time.Duration(1<<min(state.failures-1, 6))
		var apiError *TelegramError
		if errors.As(err, &apiError) && apiError.RetryAfter > delay {
			delay = apiError.RetryAfter
		}
		state.retryAt = time.Now().Add(delay)
		m.putBotNameState(id, state)
	}
	// Resolve the current trusted source even if the previous applied name is
	// cached or the flag is off: a newly recovered creation intent can correct
	// the base without replacing the historical capture.
	if err := m.loadBotNameBase(requestCtx, id, &state); err != nil {
		fail("name_state_unavailable", err)
		return
	}
	sessionName := ""
	if p.InheritSessionName {
		var err error
		sessionName, err = m.store.TelegramBotOwnerSessionName(requestCtx, id, m.cfg.Secrets.WLID)
		if err != nil {
			fail("name_selection_unavailable", err)
			return
		}
	}
	desired := ""
	if p.OriginalDisplayName != nil {
		desired = telegramBotSessionDisplayName(*state.originalDisplayName(), sessionName, m.botNameRedactor)
		if p.AppliedDisplayName != nil && desired == *p.AppliedDisplayName {
			state.pending, state.lastError, state.failures, state.retryAt = false, "", 0, time.Time{}
			m.putBotNameState(id, state)
			return
		}
	}
	state.pending = true
	m.putBotNameState(id, state)
	// Never mutate a global profile after the bot has moved to another gateway.
	webhook, err := bot.client.GetWebhook(requestCtx)
	if err != nil {
		fail("name_update_unavailable", err)
		return
	}
	if webhook.URL != m.webhookURL(bot.record) {
		fail("name_webhook_moved", nil)
		return
	}
	if p.OriginalDisplayName == nil {
		original, err := bot.client.GetMyName(requestCtx)
		if err != nil {
			fail("name_update_unavailable", err)
			return
		}
		if err := m.store.CaptureTelegramBotOriginalName(requestCtx, id, original); err != nil {
			fail("name_state_unavailable", err)
			return
		}
		p.OriginalDisplayName, p.AppliedDisplayName = &original, &original
		if state.baseName == nil {
			state.baseName = &original
		}
		// Persist and publish the captured baseline before the first remote change.
		m.putBotNameState(id, state)
		desired = telegramBotSessionDisplayName(*state.originalDisplayName(), sessionName, m.botNameRedactor)
	}
	if p.AppliedDisplayName == nil || desired != *p.AppliedDisplayName {
		if err := m.store.MarkTelegramBotNamePending(requestCtx, id); err != nil {
			fail("name_state_unavailable", err)
			return
		}
		p.AppliedDisplayName = nil
		m.putBotNameState(id, state)
		if err := bot.client.SetMyName(requestCtx, desired); err != nil {
			fail("name_update_unavailable", err)
			return
		}
	}
	copy := *bot
	copy.record.DisplayName = desired
	copy.record.UpdatedAt = time.Now().UTC()
	copy.checkedAt = time.Now()
	if err := m.store.SaveTelegramBotRecord(requestCtx, copy.record); err != nil {
		fail("name_state_unavailable", err)
		return
	}
	if err := m.store.SetTelegramBotAppliedName(requestCtx, id, desired); err != nil {
		fail("name_state_unavailable", err)
		return
	}
	m.put(&copy)
	p.AppliedDisplayName = &desired
	state.pending, state.lastError, state.failures, state.retryAt = false, "", 0, time.Time{}
	m.putBotNameState(id, state)
}

func telegramBotSessionDisplayName(original, session string, redactor *auth.Redactor) string {
	session = normalizeTelegramSessionName(session)
	if session == "" {
		return original
	}
	base := normalizeTelegramSessionName(original)
	if redactor != nil {
		base, session = redactor.Redact(base), redactor.Redact(session)
	}
	if base == "" {
		return truncateTelegramBotName(session, 64)
	}
	// A full-length original must still leave a useful part of the session visible.
	if utf8.RuneCountInString(base)+3+utf8.RuneCountInString(session) > 64 && utf8.RuneCountInString(base) > 32 {
		base = truncateTelegramBotName(base, 32)
	}
	name := base + " · " + truncateTelegramBotName(session, 64-3-utf8.RuneCountInString(base))
	if redactor != nil {
		name = redactor.Redact(name)
	}
	return truncateTelegramBotName(name, 64)
}

func normalizeTelegramSessionName(name string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(name, ""))), " ")
}

func truncateTelegramBotName(name string, limit int) string {
	runes := []rune(name)
	if len(runes) <= limit {
		return name
	}
	if limit <= 0 {
		return ""
	}
	return string(runes[:limit-1]) + "…"
}
