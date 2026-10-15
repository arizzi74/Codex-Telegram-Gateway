package gateway

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/registry"
)

var (
	ErrTelegramBotInvalid            = errors.New("invalid Telegram bot input")
	ErrTelegramBotNotFound           = errors.New("Telegram bot not found")
	ErrTelegramBotDuplicate          = errors.New("Telegram bot is already connected or creation is pending")
	ErrTelegramBotLegacy             = errors.New("the primary bot is managed by the local configuration")
	ErrTelegramBotWebhookConflict    = errors.New("Telegram bot already has a different webhook; explicit takeover is required")
	ErrTelegramBotManagerUnavailable = errors.New("enable Bot Management Mode in BotFather for this manager bot")
	ErrTelegramBotOwnerChanged       = errors.New("managed Telegram bot ownership changed; verify the configured owner before reconnecting")
)

const TelegramManagedBotIntentTTL = 15 * time.Minute
const legacyTelegramWebhookPath = "/tgw/api/v1/telegram/webhook"
const telegramBotsWebhookPrefix = "/tgw/api/v1/telegram/bots/"

var telegramTokenPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}:[A-Za-z0-9_-]{20,256}$`)
var telegramUsernamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{3,30}[A-Za-z0-9_]$`)

type TelegramBotSummary struct {
	ID            string `json:"id"`
	TelegramID    int64  `json:"telegram_id"`
	Username      string `json:"username"`
	DisplayName   string `json:"display_name"`
	Enabled       bool   `json:"enabled"`
	Legacy        bool   `json:"legacy"`
	Managed       bool   `json:"managed"`
	CanManageBots bool   `json:"can_manage_bots"`
	WebhookURL    string `json:"webhook_url"`
	Status        string `json:"status"`
	Error         string `json:"error,omitempty"`
}

type TelegramManagedBotIntent struct {
	ID                string    `json:"id"`
	ManagerID         string    `json:"manager_id"`
	RequestedUsername string    `json:"requested_username"`
	DisplayName       string    `json:"display_name"`
	LaunchURL         string    `json:"launch_url"`
	ExpiresAt         time.Time `json:"expires_at"`
	Status            string    `json:"status"`
	BotID             string    `json:"bot_id,omitempty"`
}

type telegramBotCredentials struct{ Token, WebhookSecret string }
type telegramManagedRuntime struct {
	record    registry.TelegramBotRecord
	cfg       config.GatewayConfig
	client    *TelegramClient
	secret    string
	checkedAt time.Time
}

// TelegramBotManager owns bot identity, credentials and webhook routing. Every
// additional bot inherits the one configured owner and the same chat policy.
type TelegramBotManager struct {
	store         *registry.Store
	cfg           config.GatewayConfig
	log           *slog.Logger
	voiceSettings VoiceConfiguration
	aead          cipher.AEAD
	op            sync.Mutex
	mu            sync.RWMutex
	bots          map[string]*telegramManagedRuntime
	newClient     func(string) *TelegramClient
}

func NewTelegramBotManager(ctx context.Context, store *registry.Store, cfg config.GatewayConfig, legacySecret string, logger *slog.Logger, settings ...VoiceConfiguration) (*TelegramBotManager, error) {
	if store == nil || cfg.Secrets.WLID <= 0 || cfg.Secrets.BotName == "" || legacySecret == "" {
		return nil, ErrTelegramBotInvalid
	}
	if logger == nil {
		logger = slog.Default()
	}
	records, err := store.TelegramBotRecords(ctx)
	if err != nil {
		return nil, errors.New("Telegram bot state could not be loaded")
	}
	hasCredentials := false
	for _, record := range records {
		if len(record.EncryptedCredentials) > 0 {
			hasCredentials = true
		}
	}
	key, err := loadTelegramBotKey(cfg.DatabasePath+".telegram-bots.key", !hasCredentials)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("Telegram bot encryption is unavailable")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("Telegram bot encryption is unavailable")
	}
	m := &TelegramBotManager{store: store, cfg: cfg, log: logger, aead: aead, bots: map[string]*telegramManagedRuntime{}, newClient: NewTelegramClient}
	if len(settings) > 0 {
		m.voiceSettings = settings[0]
	}
	now := time.Now().UTC()
	primary := registry.TelegramBotRecord{ID: cfg.Secrets.BotName, TelegramID: telegramTokenID(cfg.Secrets.BotToken), OwnerID: cfg.Secrets.WLID, Username: telegramCommandUsername(cfg), DisplayName: telegramCommandUsername(cfg), Legacy: true, Enabled: true, EncryptedCredentials: []byte{}, CreatedAt: now, UpdatedAt: now}
	for _, record := range records {
		if record.OwnerID != cfg.Secrets.WLID {
			return nil, errors.New("Telegram bot state belongs to a different configured owner")
		}
		if record.Legacy {
			if record.ID != primary.ID {
				return nil, errors.New("configured primary Telegram routing ID differs from persisted bot state")
			}
			if record.TelegramID > 0 && record.TelegramID != primary.TelegramID {
				return nil, errors.New("configured primary Telegram bot identity differs from persisted state; existing bindings cannot be reassigned")
			}
			primary.Enabled, primary.CreatedAt, primary.CanManageBots = record.Enabled, record.CreatedAt, record.CanManageBots
			if record.TelegramID == primary.TelegramID && record.Username != "" {
				primary.Username, primary.DisplayName = record.Username, record.DisplayName
			}
			continue
		}
		credentials, err := m.decrypt(record.ID, record.EncryptedCredentials)
		if err != nil {
			return nil, err
		}
		if record.TelegramID <= 0 || telegramTokenID(credentials.Token) != record.TelegramID || record.ID != "telegram:"+strconv.FormatInt(record.TelegramID, 10) || credentials.WebhookSecret == "" {
			return nil, errors.New("Telegram bot state is invalid")
		}
		m.bots[record.ID] = m.runtime(record, credentials)
	}
	for _, bot := range m.bots {
		if primary.TelegramID > 0 && bot.record.TelegramID == primary.TelegramID {
			return nil, ErrTelegramBotDuplicate
		}
	}
	if err := store.SaveTelegramBotRecord(ctx, primary); err != nil {
		return nil, errors.New("primary Telegram bot state could not be saved")
	}
	m.bots[primary.ID] = m.runtime(primary, telegramBotCredentials{cfg.Secrets.BotToken, legacySecret})
	for _, bot := range m.bots {
		if err := store.ApplyTelegramChatAllowlist(ctx, bot.record.ID, cfg.AllowedChatIDs); err != nil {
			return nil, errors.New("Telegram chat policy could not be applied")
		}
		if !bot.record.Enabled {
			if err := store.RetireTelegramBotWork(ctx, bot.record.ID); err != nil {
				return nil, errors.New("disabled Telegram bot work could not be retired")
			}
		}
	}
	return m, nil
}

func loadTelegramBotKey(path string, create bool) ([]byte, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && create {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, errors.New("Telegram bot encryption key could not be generated")
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			return loadTelegramBotKey(path, false)
		}
		if err != nil {
			return nil, errors.New("Telegram bot encryption key could not be created")
		}
		_, writeErr := file.Write(key)
		if writeErr == nil {
			writeErr = file.Sync()
		}
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			return nil, errors.New("Telegram bot encryption key could not be saved")
		}
		if dir, err := os.OpenFile(filepath.Dir(path), os.O_RDONLY, 0); err == nil {
			syncErr := dir.Sync()
			_ = dir.Close()
			if syncErr != nil {
				return nil, errors.New("Telegram bot encryption key could not be saved")
			}
		} else {
			return nil, errors.New("Telegram bot encryption key directory is unavailable")
		}
		return key, nil
	}
	if err != nil {
		return nil, errors.New("Telegram bot encryption key is missing or unavailable")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("Telegram bot encryption key must be a private regular file (0600)")
	}
	key, err := os.ReadFile(path)
	if err != nil || len(key) != 32 {
		return nil, errors.New("Telegram bot encryption key is invalid")
	}
	return key, nil
}

func (m *TelegramBotManager) encrypt(id string, credentials telegramBotCredentials) ([]byte, error) {
	data, err := json.Marshal(credentials)
	if err != nil {
		return nil, errors.New("Telegram bot credentials could not be encoded")
	}
	nonce := make([]byte, m.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, errors.New("Telegram bot credentials could not be encrypted")
	}
	return m.aead.Seal(nonce, nonce, data, []byte(id)), nil
}
func (m *TelegramBotManager) decrypt(id string, data []byte) (telegramBotCredentials, error) {
	var credentials telegramBotCredentials
	if len(data) < m.aead.NonceSize() {
		return credentials, errors.New("Telegram bot credentials are invalid")
	}
	plain, err := m.aead.Open(nil, data[:m.aead.NonceSize()], data[m.aead.NonceSize():], []byte(id))
	if err != nil || json.Unmarshal(plain, &credentials) != nil {
		return credentials, errors.New("Telegram bot credentials could not be decrypted")
	}
	return credentials, nil
}
func telegramTokenID(token string) int64 {
	head, _, ok := strings.Cut(token, ":")
	if !ok {
		return 0
	}
	id, _ := strconv.ParseInt(head, 10, 64)
	if id < 0 {
		return 0
	}
	return id
}
func validTelegramBotUsername(username string) bool {
	return telegramUsernamePattern.MatchString(username) && strings.HasSuffix(strings.ToLower(username), "bot")
}
func (m *TelegramBotManager) runtime(record registry.TelegramBotRecord, credentials telegramBotCredentials) *telegramManagedRuntime {
	cfg := m.cfg
	cfg.Secrets.BotName = record.ID
	cfg.Secrets.BotUsername = record.Username
	cfg.Secrets.BotToken = credentials.Token
	cfg.AllowedUserIDs = []int64{m.cfg.Secrets.WLID}
	cfg.AllowedChatIDs = append([]int64(nil), m.cfg.AllowedChatIDs...)
	return &telegramManagedRuntime{record: record, cfg: cfg, client: m.newClient(credentials.Token), secret: credentials.WebhookSecret}
}
func (m *TelegramBotManager) get(id string) *telegramManagedRuntime {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.bots[id]
}
func (m *TelegramBotManager) put(bot *telegramManagedRuntime) {
	m.mu.Lock()
	m.bots[bot.record.ID] = bot
	m.mu.Unlock()
}
func (m *TelegramBotManager) ResolveTelegramBot(id string) (config.GatewayConfig, *TelegramClient, bool) {
	bot := m.get(id)
	if bot == nil || !bot.record.Enabled {
		return config.GatewayConfig{}, nil, false
	}
	cfg := bot.cfg
	cfg.AllowedUserIDs = append([]int64(nil), cfg.AllowedUserIDs...)
	cfg.AllowedChatIDs = append([]int64(nil), cfg.AllowedChatIDs...)
	return cfg, bot.client, true
}
func (m *TelegramBotManager) TelegramBotIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []string{}
	for id, bot := range m.bots {
		if bot.record.Enabled {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
func (m *TelegramBotManager) webhookURL(record registry.TelegramBotRecord) string {
	path := legacyTelegramWebhookPath
	if !record.Legacy {
		path = telegramBotsWebhookPrefix + url.PathEscape(record.ID) + "/webhook"
	}
	return strings.TrimRight(m.cfg.PublicBaseURL, "/") + path
}
func (m *TelegramBotManager) summary(bot *telegramManagedRuntime) TelegramBotSummary {
	b := bot.record
	status := "disabled"
	if b.Enabled {
		status = "connected"
	}
	if b.LastError != "" {
		status = "error"
	}
	return TelegramBotSummary{ID: b.ID, TelegramID: b.TelegramID, Username: b.Username, DisplayName: b.DisplayName, Enabled: b.Enabled, Legacy: b.Legacy, Managed: b.ManagedBy != "", CanManageBots: b.CanManageBots, WebhookURL: m.webhookURL(b), Status: status, Error: b.LastError}
}
func (m *TelegramBotManager) ListTelegramBots(ctx context.Context) ([]TelegramBotSummary, error) {
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.RLock()
	ids := make([]string, 0, len(m.bots))
	for id := range m.bots {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	sort.Strings(ids)
	out := []TelegramBotSummary{}
	probeBudget, finishProbes := context.WithTimeout(ctx, 3*time.Second)
	defer finishProbes()
	for _, id := range ids {
		bot := m.get(id)
		if bot.record.Enabled && time.Since(bot.checkedAt) > time.Minute && probeBudget.Err() == nil {
			copy := *bot
			copy.checkedAt = time.Now()
			identity, err := bot.client.Identity(probeBudget)
			if err == nil && identity.IsBot && identity.ID > 0 && (bot.record.TelegramID == 0 || bot.record.TelegramID == identity.ID) {
				copy.record.TelegramID, copy.record.Username, copy.record.DisplayName, copy.record.CanManageBots = identity.ID, identity.Username, identity.FirstName, identity.CanManageBots
				copy.cfg.Secrets.BotUsername = identity.Username
				copy.record.UpdatedAt = time.Now().UTC()
				if copy.record.LastError == "identity_unavailable" {
					copy.record.LastError = ""
				}
				if err := m.store.SaveTelegramBotRecord(ctx, copy.record); err != nil {
					return nil, errors.New("Telegram bot identity could not be saved")
				}
			} else {
				copy.record.LastError = "identity_unavailable"
			}
			m.put(&copy)
			bot = &copy
		}
		out = append(out, m.summary(bot))
	}
	return out, nil
}

func (m *TelegramBotManager) verifiedClient(ctx context.Context, token string) (*TelegramClient, TelegramUser, error) {
	if !telegramTokenPattern.MatchString(token) {
		return nil, TelegramUser{}, ErrTelegramBotInvalid
	}
	client := m.newClient(token)
	identity, err := client.Identity(ctx)
	if err != nil {
		return nil, TelegramUser{}, errors.New("Telegram bot identity could not be verified")
	}
	if !identity.IsBot || identity.ID <= 0 || identity.ID != telegramTokenID(token) || !validTelegramBotUsername(identity.Username) {
		return nil, TelegramUser{}, ErrTelegramBotInvalid
	}
	return client, identity, nil
}
func (m *TelegramBotManager) checkWebhook(ctx context.Context, client *TelegramClient, record registry.TelegramBotRecord, takeover bool) error {
	info, err := client.GetWebhook(ctx)
	if err != nil {
		return errors.New("Telegram webhook status could not be verified")
	}
	if info.URL != "" && info.URL != m.webhookURL(record) && !takeover {
		return ErrTelegramBotWebhookConflict
	}
	return nil
}
func (m *TelegramBotManager) ConnectTelegramBot(ctx context.Context, token string, takeover bool) (TelegramBotSummary, error) {
	m.op.Lock()
	defer m.op.Unlock()
	client, identity, err := m.verifiedClient(ctx, strings.TrimSpace(token))
	if err != nil {
		return TelegramBotSummary{}, err
	}
	m.mu.RLock()
	duplicate := false
	for _, bot := range m.bots {
		if bot.record.TelegramID == identity.ID {
			duplicate = true
		}
	}
	m.mu.RUnlock()
	if duplicate {
		return TelegramBotSummary{}, ErrTelegramBotDuplicate
	}
	now := time.Now().UTC()
	record := registry.TelegramBotRecord{ID: "telegram:" + strconv.FormatInt(identity.ID, 10), TelegramID: identity.ID, OwnerID: m.cfg.Secrets.WLID, Username: identity.Username, DisplayName: identity.FirstName, CanManageBots: identity.CanManageBots, CreatedAt: now, UpdatedAt: now}
	if err = m.checkWebhook(ctx, client, record, takeover); err != nil {
		return TelegramBotSummary{}, err
	}
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		return TelegramBotSummary{}, errors.New("Telegram webhook secret could not be generated")
	}
	credentials := telegramBotCredentials{strings.TrimSpace(token), base64.RawURLEncoding.EncodeToString(secret)}
	retired, err := m.store.TelegramBotRetired(ctx, record.ID)
	if err != nil {
		return TelegramBotSummary{}, errors.New("Telegram bot retirement state could not be read")
	}
	return m.connect(ctx, record, credentials, client, retired)
}
func (m *TelegramBotManager) connect(ctx context.Context, record registry.TelegramBotRecord, credentials telegramBotCredentials, client *TelegramClient, dropPending ...bool) (TelegramBotSummary, error) {
	var err error
	record.Enabled = false
	record.LastError = "webhook_setup_pending"
	record.EncryptedCredentials, err = m.encrypt(record.ID, credentials)
	if err != nil {
		return TelegramBotSummary{}, err
	}
	if err = m.store.SaveTelegramBotRecord(ctx, record); err != nil {
		return TelegramBotSummary{}, errors.New("Telegram bot connection could not be saved")
	}
	bot := m.runtime(record, credentials)
	bot.client = client
	bot.checkedAt = time.Now()
	m.put(bot)
	if err = m.store.ApplyTelegramChatAllowlist(ctx, record.ID, m.cfg.AllowedChatIDs); err != nil {
		return m.summary(bot), errors.New("Telegram chat policy could not be saved")
	}
	if err = client.setWebhook(ctx, m.webhookURL(record), credentials.WebhookSecret, len(dropPending) > 0 && dropPending[0]); err != nil {
		copy := *bot
		copy.record.LastError = "webhook_setup_failed"
		_ = m.store.SaveTelegramBotRecord(ctx, copy.record)
		m.put(&copy)
		return m.summary(&copy), errors.New("Telegram webhook could not be configured")
	}
	copy := *bot
	copy.record.Enabled = true
	copy.record.LastError = ""
	copy.record.UpdatedAt = time.Now().UTC()
	if err = m.store.SaveTelegramBotRecord(ctx, copy.record); err != nil {
		return m.summary(bot), errors.New("Telegram bot activation could not be saved")
	}
	m.put(&copy)
	return m.summary(&copy), nil
}

func (m *TelegramBotManager) ReplaceTelegramBotToken(ctx context.Context, id, token string, takeover bool) (TelegramBotSummary, error) {
	m.op.Lock()
	defer m.op.Unlock()
	bot := m.get(id)
	if bot == nil {
		return TelegramBotSummary{}, ErrTelegramBotNotFound
	}
	if bot.record.Legacy {
		return TelegramBotSummary{}, ErrTelegramBotLegacy
	}
	if bot.record.ManagedBy != "" && bot.record.LastError == "owner_changed" {
		return TelegramBotSummary{}, ErrTelegramBotOwnerChanged
	}
	client, identity, err := m.verifiedClient(ctx, strings.TrimSpace(token))
	if err != nil {
		return TelegramBotSummary{}, err
	}
	if identity.ID != bot.record.TelegramID {
		return TelegramBotSummary{}, ErrTelegramBotInvalid
	}
	if err = m.checkWebhook(ctx, client, bot.record, takeover); err != nil {
		return TelegramBotSummary{}, err
	}
	// Fence the old client and discard queued work before switching credentials.
	if err = m.disableLocal(ctx, bot); err != nil {
		return TelegramBotSummary{}, err
	}
	record := bot.record
	record.Username, record.DisplayName, record.CanManageBots, record.LastError = identity.Username, identity.FirstName, identity.CanManageBots, ""
	return m.connect(ctx, record, telegramBotCredentials{strings.TrimSpace(token), bot.secret}, client, !bot.record.Enabled)
}
func (m *TelegramBotManager) disableLocal(ctx context.Context, bot *telegramManagedRuntime, reason ...string) error {
	copy := *bot
	copy.record.Enabled = false
	if len(reason) > 0 {
		copy.record.LastError = reason[0]
	} else if copy.record.LastError != "owner_changed" {
		copy.record.LastError = ""
	}
	copy.record.UpdatedAt = time.Now().UTC()
	m.put(&copy)
	if err := m.store.SaveTelegramBotRecord(ctx, copy.record); err != nil {
		return errors.New("Telegram bot disable could not be saved")
	}
	if err := m.store.RetireTelegramBotWork(ctx, copy.record.ID); err != nil {
		return errors.New("Telegram bot queued work could not be retired")
	}
	return nil
}
func (m *TelegramBotManager) cleanupWebhook(ctx context.Context, bot *telegramManagedRuntime) {
	// Never delete a webhook that has since been moved to another gateway.
	info, err := bot.client.GetWebhook(ctx)
	if err == nil && info.URL == m.webhookURL(bot.record) {
		_ = bot.client.DeleteWebhook(ctx)
	}
}
func (m *TelegramBotManager) SetTelegramBotEnabled(ctx context.Context, id string, enabled bool) error {
	m.op.Lock()
	defer m.op.Unlock()
	bot := m.get(id)
	if bot == nil {
		return ErrTelegramBotNotFound
	}
	if !enabled {
		if err := m.disableLocal(ctx, bot); err != nil {
			return err
		}
		m.cleanupWebhook(ctx, bot)
		return nil
	}
	if bot.record.ManagedBy != "" && bot.record.LastError == "owner_changed" {
		return ErrTelegramBotOwnerChanged
	}
	if bot.record.Enabled {
		return nil
	}
	if err := m.checkWebhook(ctx, bot.client, bot.record, false); err != nil {
		return err
	}
	identity, err := bot.client.Identity(ctx)
	if err != nil {
		return errors.New("Telegram bot identity could not be verified")
	}
	if !identity.IsBot || identity.ID != bot.record.TelegramID {
		return ErrTelegramBotInvalid
	}
	if err := m.store.ApplyTelegramChatAllowlist(ctx, id, m.cfg.AllowedChatIDs); err != nil {
		return errors.New("Telegram chat policy could not be saved")
	}
	if err := bot.client.SetWebhookDiscardPending(ctx, m.webhookURL(bot.record), bot.secret); err != nil {
		return errors.New("Telegram webhook could not be configured")
	}
	copy := *bot
	copy.record.Enabled = true
	copy.record.LastError = ""
	copy.record.Username, copy.record.DisplayName, copy.record.CanManageBots = identity.Username, identity.FirstName, identity.CanManageBots
	copy.cfg.Secrets.BotUsername = identity.Username
	copy.record.UpdatedAt = time.Now().UTC()
	if err := m.store.SaveTelegramBotRecord(ctx, copy.record); err != nil {
		return errors.New("Telegram bot activation could not be saved")
	}
	m.put(&copy)
	return nil
}
func (m *TelegramBotManager) RemoveTelegramBot(ctx context.Context, id string) error {
	m.op.Lock()
	defer m.op.Unlock()
	bot := m.get(id)
	if bot == nil {
		return ErrTelegramBotNotFound
	}
	if bot.record.Legacy {
		return ErrTelegramBotLegacy
	}
	if err := m.disableLocal(ctx, bot); err != nil {
		return err
	}
	m.cleanupWebhook(ctx, bot)
	if err := m.store.DeleteTelegramBotRecord(ctx, id); err != nil {
		return errors.New("Telegram bot could not be removed")
	}
	m.mu.Lock()
	delete(m.bots, id)
	m.mu.Unlock()
	return nil
}

func (m *TelegramBotManager) CreateManagedTelegramBot(ctx context.Context, managerID, username, displayName string) (TelegramManagedBotIntent, error) {
	m.op.Lock()
	defer m.op.Unlock()
	if managerID == "" {
		managerID = m.cfg.Secrets.BotName
	}
	bot := m.get(managerID)
	if bot == nil || !bot.record.Enabled {
		return TelegramManagedBotIntent{}, ErrTelegramBotNotFound
	}
	username = strings.TrimPrefix(strings.TrimSpace(username), "@")
	displayName = strings.TrimSpace(displayName)
	if !validTelegramBotUsername(username) || !utf8.ValidString(displayName) || utf8.RuneCountInString(displayName) < 1 || utf8.RuneCountInString(displayName) > 64 {
		return TelegramManagedBotIntent{}, ErrTelegramBotInvalid
	}
	identity, err := bot.client.Identity(ctx)
	if err != nil {
		return TelegramManagedBotIntent{}, errors.New("Telegram manager identity could not be verified")
	}
	if !identity.CanManageBots {
		return TelegramManagedBotIntent{}, ErrTelegramBotManagerUnavailable
	}
	if !identity.IsBot || identity.ID != bot.record.TelegramID || !validTelegramBotUsername(identity.Username) {
		return TelegramManagedBotIntent{}, ErrTelegramBotInvalid
	}
	intents, err := m.store.TelegramManagedBotIntents(ctx)
	if err != nil {
		return TelegramManagedBotIntent{}, errors.New("Telegram creation requests could not be read")
	}
	for _, intent := range intents {
		if intent.ManagerID == managerID && (intent.Status == "pending" || intent.Status == "activating") && strings.EqualFold(intent.RequestedUsername, username) {
			return TelegramManagedBotIntent{}, ErrTelegramBotDuplicate
		}
	}
	if err = m.checkWebhook(ctx, bot.client, bot.record, false); err != nil {
		return TelegramManagedBotIntent{}, err
	}
	if err = bot.client.SetWebhook(ctx, m.webhookURL(bot.record), bot.secret); err != nil {
		return TelegramManagedBotIntent{}, errors.New("Telegram manager webhook could not be configured")
	}
	now := time.Now().UTC()
	intent := registry.TelegramManagedBotIntentRecord{ID: uuid.NewString(), ManagerID: managerID, OwnerID: m.cfg.Secrets.WLID, RequestedUsername: username, DisplayName: displayName, LaunchURL: "https://t.me/newbot/" + identity.Username + "/" + username + "?" + url.Values{"name": {displayName}}.Encode(), Status: "pending", CreatedAt: now, ExpiresAt: now.Add(TelegramManagedBotIntentTTL)}
	if err = m.store.SaveTelegramManagedBotIntent(ctx, intent); err != nil {
		return TelegramManagedBotIntent{}, errors.New("Telegram creation request could not be saved")
	}
	copy := *bot
	copy.record.CanManageBots = true
	copy.record.Username = identity.Username
	copy.cfg.Secrets.BotUsername = identity.Username
	copy.checkedAt = time.Now()
	_ = m.store.SaveTelegramBotRecord(ctx, copy.record)
	m.put(&copy)
	return telegramManagedIntentSummary(intent), nil
}
func telegramManagedIntentSummary(in registry.TelegramManagedBotIntentRecord) TelegramManagedBotIntent {
	return TelegramManagedBotIntent{ID: in.ID, ManagerID: in.ManagerID, RequestedUsername: in.RequestedUsername, DisplayName: in.DisplayName, LaunchURL: in.LaunchURL, ExpiresAt: in.ExpiresAt, Status: in.Status, BotID: in.BotID}
}
func (m *TelegramBotManager) ListTelegramManagedBotIntents(ctx context.Context) ([]TelegramManagedBotIntent, error) {
	records, err := m.store.TelegramManagedBotIntents(ctx)
	if err != nil {
		return nil, errors.New("Telegram creation requests could not be read")
	}
	out := []TelegramManagedBotIntent{}
	for _, in := range records {
		summary := telegramManagedIntentSummary(in)
		if in.Status == "activating" {
			summary.Status = "pending"
			if bot := m.get(in.BotID); bot != nil && bot.record.LastError == "webhook_setup_failed" {
				summary.Status = "failed"
			}
		}
		out = append(out, summary)
	}
	return out, nil
}

func (m *TelegramBotManager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := ""
	if r.URL.Path == legacyTelegramWebhookPath {
		id = m.cfg.Secrets.BotName
	} else if strings.HasPrefix(r.URL.Path, telegramBotsWebhookPrefix) && strings.HasSuffix(r.URL.Path, "/webhook") {
		id = strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, telegramBotsWebhookPrefix), "/webhook")
		if strings.Contains(id, "/") {
			id = ""
		}
	}
	bot := m.get(id)
	if bot == nil || !bot.record.Enabled || (!bot.record.Legacy && r.URL.Path != telegramBotsWebhookPrefix+bot.record.ID+"/webhook") || (bot.record.Legacy && r.URL.Path != legacyTelegramWebhookPath) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !auth.ValidateWebhookSecret(r.Header.Get("X-Telegram-Bot-Api-Secret-Token"), bot.secret) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "update too large", http.StatusRequestEntityTooLarge)
		return
	}
	var update TelegramUpdate
	if err = json.Unmarshal(raw, &update); err != nil || update.ID < 0 {
		http.Error(w, "invalid update", http.StatusBadRequest)
		return
	}
	if update.ManagedBot != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err = m.acceptManagedBotUpdate(ctx, id, update); err != nil {
			m.log.Warn("Telegram managed bot update could not be applied", "manager_id", id, "telegram_update_id", update.ID)
			http.Error(w, "managed bot update unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	bot = m.get(id)
	if bot == nil || !bot.record.Enabled {
		http.NotFound(w, r)
		return
	}
	NewWebhook(m.store, bot.cfg, bot.secret, bot.client, m.log, m.voiceSettings).ServeHTTP(w, r)
}

func (m *TelegramBotManager) acceptManagedBotUpdate(ctx context.Context, managerID string, update TelegramUpdate) error {
	m.op.Lock()
	defer m.op.Unlock()
	manager := m.get(managerID)
	if manager == nil || !manager.record.Enabled {
		return nil
	}
	seen, err := m.store.TelegramManagedBotUpdateSeen(ctx, managerID, update.ID)
	if err != nil {
		return errors.New("Telegram managed update state unavailable")
	}
	if seen {
		return nil
	}
	event := update.ManagedBot
	if event == nil {
		return nil
	}
	id := "telegram:" + strconv.FormatInt(event.Bot.ID, 10)
	existing := m.get(id)
	ignore := func() error { return m.store.CompleteTelegramManagedBotUpdate(ctx, managerID, update.ID, "") }
	if event.User.ID != m.cfg.Secrets.WLID || event.User.IsBot {
		if existing != nil && existing.record.ManagedBy == managerID {
			if err := m.disableLocal(ctx, existing, "owner_changed"); err != nil {
				return err
			}
		}
		return ignore()
	}
	if !event.Bot.IsBot || event.Bot.ID <= 0 || !validTelegramBotUsername(event.Bot.Username) {
		return ignore()
	}
	intentID := ""
	if existing != nil {
		if existing.record.ManagedBy != managerID {
			return ignore()
		}
	} else {
		intent, err := m.store.PendingTelegramManagedBotIntent(ctx, managerID, m.cfg.Secrets.WLID, event.Bot.Username)
		if errors.Is(err, registry.ErrTelegramBotRecordNotFound) {
			return ignore()
		}
		if err != nil {
			return errors.New("Telegram managed creation state unavailable")
		}
		intentID = intent.ID
		m.mu.RLock()
		duplicate := false
		for _, bot := range m.bots {
			if bot.record.TelegramID == event.Bot.ID {
				duplicate = true
			}
		}
		m.mu.RUnlock()
		if duplicate {
			return ignore()
		}
	}
	token, err := manager.client.GetManagedBotToken(ctx, event.Bot.ID)
	if err != nil {
		return errors.New("Telegram managed bot token could not be retrieved")
	}
	client, identity, err := m.verifiedClient(ctx, token)
	if err != nil {
		return err
	}
	if identity.ID != event.Bot.ID || !strings.EqualFold(identity.Username, event.Bot.Username) {
		return ignore()
	}
	now := time.Now().UTC()
	record := registry.TelegramBotRecord{ID: id, TelegramID: identity.ID, OwnerID: m.cfg.Secrets.WLID, Username: identity.Username, DisplayName: identity.FirstName, ManagedBy: managerID, CanManageBots: identity.CanManageBots, CreatedAt: now, UpdatedAt: now}
	secret := ""
	if existing != nil {
		record.CreatedAt = existing.record.CreatedAt
		secret = existing.secret
	} else {
		random := make([]byte, 32)
		if _, err := rand.Read(random); err != nil {
			return errors.New("Telegram managed webhook secret unavailable")
		}
		secret = base64.RawURLEncoding.EncodeToString(random)
	}
	if err = m.checkWebhook(ctx, client, record, false); err != nil {
		return err
	}
	if existing == nil {
		record.LastError = "webhook_setup_pending"
		record.EncryptedCredentials, err = m.encrypt(id, telegramBotCredentials{token, secret})
		if err != nil {
			return err
		}
		if err = m.store.ClaimTelegramManagedBotCreation(ctx, record, intentID); errors.Is(err, registry.ErrTelegramBotRecordNotFound) {
			return ignore()
		} else if err != nil {
			return errors.New("Telegram managed creation could not be claimed")
		}
		m.put(m.runtime(record, telegramBotCredentials{token, secret}))
	}
	// An intentional disable stays disabled. A failed or interrupted activation
	// remains retryable when Telegram redelivers the managed update.
	if existing != nil && !existing.record.Enabled && existing.record.LastError != "webhook_setup_pending" && existing.record.LastError != "webhook_setup_failed" {
		record.EncryptedCredentials, err = m.encrypt(id, telegramBotCredentials{token, secret})
		if err != nil {
			return err
		}
		if err = m.store.SaveTelegramBotRecord(ctx, record); err != nil {
			return errors.New("Telegram managed credentials could not be saved")
		}
		m.put(m.runtime(record, telegramBotCredentials{token, secret}))
	} else {
		if _, err = m.connect(ctx, record, telegramBotCredentials{token, secret}, client); err != nil {
			return err
		}
	}
	return m.store.CompleteTelegramManagedBotUpdate(ctx, managerID, update.ID, id)
}
