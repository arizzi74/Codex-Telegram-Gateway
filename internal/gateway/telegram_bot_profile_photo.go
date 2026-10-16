package gateway

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"
)

//go:embed assets/default-bot-profile.jpg
var defaultTelegramBotProfilePhoto []byte

var (
	ErrTelegramBotDisabled               = errors.New("Telegram bot is disabled")
	ErrTelegramBotProfileWebhookConflict = errors.New("Telegram bot profile webhook does not belong to this gateway")
)

// ResetTelegramBotProfilePhoto explicitly replaces an enabled bot's current
// photo. Verify its present credentials and webhook before changing the global
// profile, including when the target is the primary configured bot.
func (m *TelegramBotManager) ResetTelegramBotProfilePhoto(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	m.op.Lock()
	defer m.op.Unlock()
	if ctx.Err() != nil {
		return errors.New("Telegram profile photo reset timed out")
	}
	bot := m.get(id)
	if bot == nil {
		return ErrTelegramBotNotFound
	}
	if bot.record.OwnerID != m.cfg.Secrets.WLID || bot.record.LastError == "owner_changed" {
		return ErrTelegramBotOwnerChanged
	}
	if !bot.record.Enabled {
		return ErrTelegramBotDisabled
	}
	client, identity, err := m.verifiedClient(ctx, bot.cfg.Secrets.BotToken)
	if err != nil {
		return errors.New("Telegram profile photo identity could not be verified")
	}
	if identity.ID != bot.record.TelegramID {
		return errors.New("Telegram profile photo identity did not match")
	}
	webhook, err := client.GetWebhook(ctx)
	if err != nil {
		return errors.New("Telegram profile photo webhook could not be verified")
	}
	if webhook.URL != m.webhookURL(bot.record) {
		return ErrTelegramBotProfileWebhookConflict
	}
	if err = client.SetMyProfilePhoto(ctx, defaultTelegramBotProfilePhoto); err != nil {
		return errors.New("Telegram profile photo could not be reset")
	}
	return nil
}

// HasUserProfilePhoto checks the verified bot's own profile without downloading
// images. Missing or malformed metadata must never be interpreted as no photo.
func (t *TelegramClient) HasUserProfilePhoto(ctx context.Context, botID int64) (bool, error) {
	if botID <= 0 {
		return false, errors.New("invalid Telegram bot identity")
	}
	data, _ := json.Marshal(struct {
		UserID int64 `json:"user_id"`
		Limit  int   `json:"limit"`
	}{UserID: botID, Limit: 1})
	var result struct {
		TotalCount *int               `json:"total_count"`
		Photos     *[][]TelegramPhoto `json:"photos"`
	}
	if err := t.profilePhotoRequest(ctx, "getUserProfilePhotos", bytes.NewReader(data), "application/json", &result); err != nil {
		return false, err
	}
	if result.TotalCount == nil || *result.TotalCount < 0 || result.Photos == nil || len(*result.Photos) > 1 {
		return false, errors.New("invalid Telegram profile photo response")
	}
	// Either positive signal preserves the existing photo, even if Telegram's
	// count and returned list disagree during a concurrent profile change.
	return *result.TotalCount > 0 || len(*result.Photos) > 0, nil
}

// SetMyProfilePhoto uploads a static JPEG using this bot's own token. Telegram
// requires a fresh multipart upload for InputProfilePhotoStatic.
func (t *TelegramClient) SetMyProfilePhoto(ctx context.Context, photo []byte) error {
	if len(photo) == 0 || len(photo) > 10<<20 {
		return errors.New("invalid Telegram profile photo")
	}
	image, err := jpeg.DecodeConfig(bytes.NewReader(photo))
	if err != nil || image.Width <= 0 || image.Height <= 0 {
		return errors.New("invalid Telegram profile photo")
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err = form.WriteField("photo", `{"type":"static","photo":"attach://profile_photo"}`); err != nil {
		return errors.New("could not encode Telegram profile photo")
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="profile_photo"; filename="codex-gateway.jpg"`)
	header.Set("Content-Type", "image/jpeg")
	part, err := form.CreatePart(header)
	if err != nil {
		return errors.New("could not encode Telegram profile photo")
	}
	if _, err = part.Write(photo); err != nil {
		return errors.New("could not encode Telegram profile photo")
	}
	if err = form.Close(); err != nil {
		return errors.New("could not encode Telegram profile photo")
	}
	var applied bool
	if err = t.profilePhotoRequest(ctx, "setMyProfilePhoto", &body, form.FormDataContentType(), &applied); err != nil {
		return err
	}
	if !applied {
		return errors.New("Telegram profile photo was not applied")
	}
	return nil
}

func (t *TelegramClient) profilePhotoRequest(ctx context.Context, method string, body io.Reader, contentType string, result any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint+"/bot"+t.token+"/"+method, body)
	if err != nil {
		return errors.New("invalid Telegram profile photo endpoint")
	}
	request.Header.Set("Content-Type", contentType)
	client := *t.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return errors.New("Telegram profile photo request could not be completed")
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return errors.New("Telegram profile photo request was redirected")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil || len(data) > 2<<20 {
		return errors.New("invalid Telegram profile photo response")
	}
	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Code        int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err = json.Unmarshal(data, &envelope); err != nil {
		return errors.New("invalid Telegram profile photo response")
	}
	if !envelope.OK {
		return &TelegramError{Code: envelope.Code, Description: strings.ReplaceAll(envelope.Description, t.token, "[REDACTED]"), RetryAfter: time.Duration(envelope.Parameters.RetryAfter) * time.Second}
	}
	if response.StatusCode != http.StatusOK || json.Unmarshal(envelope.Result, result) != nil {
		return errors.New("invalid Telegram profile photo response")
	}
	return nil
}

func (m *TelegramBotManager) applyManagedBotDefaultProfilePhoto(ctx context.Context, managerID string, bot *telegramManagedRuntime) error {
	if bot == nil || !bot.record.Enabled || bot.record.Legacy || bot.record.ManagedBy != managerID || bot.record.OwnerID != m.cfg.Secrets.WLID {
		return nil
	}
	pending, err := m.store.TelegramManagedBotActivationPending(ctx, managerID, m.cfg.Secrets.WLID, bot.record.TelegramID)
	if err != nil {
		return errors.New("Telegram managed profile photo state unavailable")
	}
	if !pending {
		return nil
	}
	exists, err := bot.client.HasUserProfilePhoto(ctx, bot.record.TelegramID)
	if err != nil {
		return errors.New("Telegram managed profile photo could not be checked")
	}
	if exists {
		return nil
	}
	if err = bot.client.SetMyProfilePhoto(ctx, defaultTelegramBotProfilePhoto); err != nil {
		return errors.New("Telegram managed profile photo could not be applied")
	}
	return nil
}
