package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// SendVoice uploads a generated voice note directly from memory. The fixed
// filename contains no session information, and redirects never receive tokens.
func (t *TelegramClient) SendVoice(ctx context.Context, chatID, topicID int64, audio []byte, duration int) (int64, error) {
	if chatID == 0 || topicID < 0 || duration < 0 || len(audio) < 4 || len(audio) > 20<<20 || string(audio[:4]) != "OggS" {
		return 0, errors.New("invalid generated voice message")
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	fields := map[string]string{"chat_id": strconv.FormatInt(chatID, 10), "disable_notification": "true"}
	if topicID != 0 {
		fields["message_thread_id"] = strconv.FormatInt(topicID, 10)
	}
	if duration != 0 {
		fields["duration"] = strconv.Itoa(duration)
	}
	for key, value := range fields {
		if err := form.WriteField(key, value); err != nil {
			return 0, errors.New("could not encode voice message")
		}
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="voice"; filename="reply.ogg"`)
	header.Set("Content-Type", "audio/ogg")
	part, err := form.CreatePart(header)
	if err != nil {
		return 0, errors.New("could not encode voice message")
	}
	if _, err = part.Write(audio); err != nil {
		return 0, errors.New("could not encode voice message")
	}
	if err = form.Close(); err != nil {
		return 0, errors.New("could not encode voice message")
	}
	defer clear(body.Bytes())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint+"/bot"+t.token+"/sendVoice", &body)
	if err != nil {
		return 0, errors.New("could not prepare voice upload")
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	client := *t.http
	client.Timeout = 60 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, errors.New("Telegram voice upload failed")
	}
	defer response.Body.Close()
	var envelope struct {
		OK     bool `json:"ok"`
		Result struct {
			ID int64 `json:"message_id"`
		} `json:"result"`
		Code        int    `json:"error_code"`
		Description string `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&envelope); err != nil {
		return 0, errors.New("invalid Telegram voice response")
	}
	if !envelope.OK {
		return 0, &TelegramError{Code: envelope.Code, Description: strings.ReplaceAll(envelope.Description, t.token, "[REDACTED]"), RetryAfter: time.Duration(envelope.Parameters.RetryAfter) * time.Second}
	}
	if envelope.Result.ID <= 0 {
		return 0, errors.New("Telegram returned no voice message identity")
	}
	return envelope.Result.ID, nil
}
