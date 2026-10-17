package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/iaia/telegramgw/internal/protocol"
)

// Image bytes travel in the durable command, never in a token-bearing Telegram
// download URL. Keeping media access separate also leaves text-only API clients
// usable by administration commands and tests.
type TelegramImageAPI interface {
	DownloadImage(context.Context, string) (protocol.Image, error)
}

// TelegramFileAPI transfers document bytes without exposing a download URL.
// Text-only and legacy image-only API implementations remain usable.
type TelegramFileAPI interface {
	DownloadFile(context.Context, string) (protocol.File, error)
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

func (h *Webhook) downloadMessageFile(ctx context.Context, message *TelegramMessage) ([]protocol.Image, []protocol.File, string) {
	document := message.Document
	if document.FileSize > protocol.MaxFileBytes {
		return nil, nil, "file_too_large"
	}
	if strings.TrimSpace(document.FileID) == "" || len(document.FileID) > 4096 {
		return nil, nil, "file_download_failed"
	}
	api, ok := h.api.(TelegramFileAPI)
	if !ok {
		return nil, nil, "file_download_failed"
	}
	file, err := api.DownloadFile(ctx, document.FileID)
	if err != nil {
		var mediaErr telegramMediaError
		if errors.As(err, &mediaErr) && string(mediaErr) == "file_too_large" {
			return nil, nil, "file_too_large"
		}
		return nil, nil, "file_download_failed"
	}
	if len(file.Data) > protocol.MaxFileBytes {
		return nil, nil, "file_too_large"
	}
	// Telegram metadata and filename extensions do not identify an image.
	// Preserve the existing vision path for supported actual image bytes.
	img := protocol.Image{MIMEType: http.DetectContentType(file.Data), Data: file.Data}
	if protocol.ValidateImages([]protocol.Image{img}) == nil {
		return []protocol.Image{img}, nil, ""
	}
	file.Name = telegramDocumentName(document.FileName)
	if err := protocol.ValidateFiles([]protocol.File{file}); err != nil {
		return nil, nil, "file_invalid"
	}
	return nil, []protocol.File{file}, ""
}

func telegramDocumentName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || unicode.IsControl(r) {
			return '_'
		}
		return r
	}, strings.ToValidUTF8(name, "_"))
	name = strings.TrimSpace(name)
	for len(name) > 1024 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	if name == "" || name == "." || name == ".." {
		return "file"
	}
	return name
}

func (t *TelegramClient) DownloadImage(ctx context.Context, fileID string) (protocol.Image, error) {
	data, err := t.downloadMediaBytes(ctx, fileID, protocol.MaxImageBytes, "image")
	if err != nil {
		return protocol.Image{}, err
	}
	img := protocol.Image{MIMEType: http.DetectContentType(data), Data: data}
	if err := protocol.ValidateImages([]protocol.Image{img}); err != nil {
		return protocol.Image{}, telegramMediaError("image_unsupported")
	}
	return img, nil
}

func (t *TelegramClient) DownloadFile(ctx context.Context, fileID string) (protocol.File, error) {
	data, err := t.downloadMediaBytes(ctx, fileID, protocol.MaxFileBytes, "file")
	if err != nil {
		return protocol.File{}, err
	}
	return protocol.File{Name: "file", Data: data}, nil
}

func (t *TelegramClient) downloadMediaBytes(ctx context.Context, fileID string, limit int, kind string) ([]byte, error) {
	var file struct {
		Path string `json:"file_path"`
		Size int64  `json:"file_size"`
	}
	failed := telegramMediaError(kind + "_download_failed")
	large := telegramMediaError(kind + "_too_large")
	if err := t.call(ctx, "getFile", map[string]string{"file_id": fileID}, &file); err != nil {
		return nil, failed
	}
	if file.Size > int64(limit) {
		return nil, large
	}
	if !safeTelegramFilePath(file.Path) {
		return nil, failed
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.endpoint+"/file/bot"+t.token+"/"+file.Path, nil)
	if err != nil {
		return nil, failed
	}
	client := *t.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		// net/http errors can include the request URL and therefore the token.
		return nil, failed
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, failed
	}
	if response.ContentLength > int64(limit) {
		return nil, large
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if err != nil {
		return nil, failed
	}
	if len(data) > limit {
		return nil, large
	}
	return data, nil
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
