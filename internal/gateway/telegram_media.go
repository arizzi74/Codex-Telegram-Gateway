package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/iaia/telegramgw/internal/protocol"
)

// Image bytes travel in the durable command, never in a token-bearing Telegram
// download URL. Keeping media access separate also leaves text-only API clients
// usable by administration commands and tests.
type TelegramImageAPI interface {
	DownloadImage(context.Context, string) (protocol.Image, error)
}

type telegramMediaError string

func (e telegramMediaError) Error() string { return string(e) }

func (m *TelegramMessage) hasMedia() bool {
	return len(m.Photo) > 0 || m.Document != nil || len(m.Animation) > 0 ||
		len(m.Video) > 0 || len(m.Audio) > 0 || len(m.Voice) > 0 ||
		len(m.VideoNote) > 0 || len(m.Sticker) > 0
}

func (h *Webhook) downloadMessageImage(ctx context.Context, message *TelegramMessage) ([]protocol.Image, string) {
	var fileID string
	var fileSize int64
	if len(message.Photo) > 0 {
		// Telegram supplies several resolutions, not several attachments.
		// Prefer the largest resolution that fits the transport's byte limit.
		var bestArea int64 = -1
		for _, photo := range message.Photo {
			if photo.FileSize > protocol.MaxImageBytes || photo.Width < 1 || photo.Height < 1 || photo.Width > 100000 || photo.Height > 100000 {
				continue
			}
			if area := photo.Width * photo.Height; area > bestArea {
				fileID, fileSize, bestArea = photo.FileID, photo.FileSize, area
			}
		}
		if bestArea < 0 {
			return nil, "image_too_large"
		}
	} else if message.Document != nil && len(message.Animation) == 0 {
		// The content is inspected after download; filenames and declared MIME
		// types are not trusted to identify images.
		fileID, fileSize = message.Document.FileID, message.Document.FileSize
	} else {
		return nil, "image_unsupported"
	}
	if fileSize > protocol.MaxImageBytes {
		return nil, "image_too_large"
	}
	if strings.TrimSpace(fileID) == "" || len(fileID) > 4096 {
		return nil, "image_download_failed"
	}
	api, ok := h.api.(TelegramImageAPI)
	if !ok {
		return nil, "image_download_failed"
	}
	img, err := api.DownloadImage(ctx, fileID)
	if err != nil {
		var mediaErr telegramMediaError
		if errors.As(err, &mediaErr) {
			switch string(mediaErr) {
			case "image_too_large", "image_unsupported":
				return nil, string(mediaErr)
			}
		}
		return nil, "image_download_failed"
	}
	images := []protocol.Image{img}
	if err := protocol.ValidateImages(images); err != nil {
		return nil, "image_unsupported"
	}
	return images, ""
}

func (t *TelegramClient) DownloadImage(ctx context.Context, fileID string) (protocol.Image, error) {
	var file struct {
		Path string `json:"file_path"`
		Size int64  `json:"file_size"`
	}
	if err := t.call(ctx, "getFile", map[string]string{"file_id": fileID}, &file); err != nil {
		return protocol.Image{}, telegramMediaError("image_download_failed")
	}
	if file.Size > protocol.MaxImageBytes {
		return protocol.Image{}, telegramMediaError("image_too_large")
	}
	if !safeTelegramFilePath(file.Path) {
		return protocol.Image{}, telegramMediaError("image_download_failed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.endpoint+"/file/bot"+t.token+"/"+file.Path, nil)
	if err != nil {
		return protocol.Image{}, telegramMediaError("image_download_failed")
	}
	client := *t.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		// net/http errors can include the request URL and therefore the token.
		return protocol.Image{}, telegramMediaError("image_download_failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return protocol.Image{}, telegramMediaError("image_download_failed")
	}
	if response.ContentLength > protocol.MaxImageBytes {
		return protocol.Image{}, telegramMediaError("image_too_large")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, protocol.MaxImageBytes+1))
	if err != nil {
		return protocol.Image{}, telegramMediaError("image_download_failed")
	}
	if len(data) > protocol.MaxImageBytes {
		return protocol.Image{}, telegramMediaError("image_too_large")
	}
	img := protocol.Image{MIMEType: http.DetectContentType(data), Data: data}
	if err := protocol.ValidateImages([]protocol.Image{img}); err != nil {
		return protocol.Image{}, telegramMediaError("image_unsupported")
	}
	return img, nil
}

func safeTelegramFilePath(path string) bool {
	if path == "" || len(path) > 4096 {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for _, c := range part {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.') {
				return false
			}
		}
	}
	return true
}
