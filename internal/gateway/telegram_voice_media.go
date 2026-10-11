package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/iaia/telegramgw/internal/registry"
	"github.com/iaia/telegramgw/internal/voice"
)

// TelegramVoiceAPI keeps audio downloads out of the webhook and text-only clients.
type TelegramVoiceAPI interface {
	DownloadVoice(context.Context, string) ([]byte, error)
}

type telegramVoiceAcceptor interface {
	AcceptTelegramVoice(context.Context, registry.IncomingUpdate, registry.TelegramVoice) (registry.AcceptResult, error)
}

func (h *Webhook) acceptVoiceMessage(w http.ResponseWriter, r *http.Request, in registry.IncomingUpdate, raw json.RawMessage) {
	var recording registry.TelegramVoice
	code := ""
	switch {
	case !h.cfg.Voice.Enabled:
		code = "voice_disabled"
	case json.Unmarshal(raw, &recording) != nil:
		code = "voice_invalid"
	case strings.TrimSpace(recording.FileID) == "" || len(recording.FileID) > 4096 || recording.Duration < 0 || recording.FileSize < 0:
		code = "voice_invalid"
	case recording.FileSize > voice.MaxInputBytes:
		code = "voice_too_large"
	case recording.Duration > int(voice.DefaultMaxDuration/time.Second):
		code = "voice_too_long"
	}
	acceptor, ok := h.store.(telegramVoiceAcceptor)
	if code == "" && !ok {
		code = "voice_unavailable"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var err error
	if code != "" {
		in.Action, in.MediaError = "media_error", code
		_, err = h.acceptTelegramReply(ctx, in)
	} else {
		// Routing is frozen in the same transaction as durable acceptance. No
		// Telegram downloads, audio decoding, or inference happen in the webhook.
		for {
			_, err = acceptor.AcceptTelegramVoice(ctx, in, recording)
			if !errors.Is(err, registry.ErrTelegramReplyPending) {
				break
			}
			select {
			case <-ctx.Done():
				err = ctx.Err()
			case <-time.After(25 * time.Millisecond):
				continue
			}
			break
		}
	}
	if err != nil {
		h.log.Warn("Telegram voice note could not be persisted", "telegram_update_id", in.UpdateID)
		http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// DownloadVoice uses only Telegram's validated file path. Redirects are refused
// so the bot token cannot leave the configured Telegram origin. Untrusted size
// metadata is checked again against the actual response bytes.
func (t *TelegramClient) DownloadVoice(ctx context.Context, fileID string) ([]byte, error) {
	if strings.TrimSpace(fileID) == "" || len(fileID) > 4096 {
		return nil, telegramMediaError("voice_download_failed")
	}
	var file struct {
		Path string `json:"file_path"`
		Size int64  `json:"file_size"`
	}
	if err := t.call(ctx, "getFile", map[string]string{"file_id": fileID}, &file); err != nil {
		return nil, telegramMediaError("voice_download_failed")
	}
	if file.Size > voice.MaxInputBytes {
		return nil, telegramMediaError("voice_too_large")
	}
	if file.Size < 0 {
		return nil, telegramMediaError("voice_download_failed")
	}
	if !safeTelegramFilePath(file.Path) {
		return nil, telegramMediaError("voice_download_failed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.endpoint+"/file/bot"+t.token+"/"+file.Path, nil)
	if err != nil {
		return nil, telegramMediaError("voice_download_failed")
	}
	client := *t.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return nil, telegramMediaError("voice_download_failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, telegramMediaError("voice_download_failed")
	}
	if response.ContentLength > voice.MaxInputBytes {
		return nil, telegramMediaError("voice_too_large")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, voice.MaxInputBytes+1))
	if err != nil {
		return nil, telegramMediaError("voice_download_failed")
	}
	if len(data) > voice.MaxInputBytes {
		return nil, telegramMediaError("voice_too_large")
	}
	// iOS recordings may end without an Ogg EOS marker. Check Telegram's
	// declared file size too, so a download cut at a complete page boundary
	// cannot silently become a shorter but otherwise valid recording.
	if file.Size > 0 && int64(len(data)) != file.Size {
		clear(data)
		return nil, telegramMediaError("voice_download_failed")
	}
	return data, nil
}
