package protocol

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"strings"
)

const MaxWebUITextBytes = 256 << 10
const MaxWebUIImageDimension = 8192
const MaxWebUIImagePixels = 16_000_000

// ValidateWebUIImageURL accepts only immutable inline image content. No path or
// network fetch can enter the native image loader through the browser relay.
// Unlike the Telegram download boundary, the untrusted browser also needs a
// complete decode check after validating dimensions to prevent header spoofing.
func ValidateWebUIImageURL(value string) error {
	if len(value) > base64.StdEncoding.EncodedLen(MaxImageBytes)+64 {
		return errors.New("image exceeds the 10 MiB limit")
	}
	var mime, format, encoded string
	for _, candidate := range []struct{ mime, format string }{{"image/png", "png"}, {"image/jpeg", "jpeg"}, {"image/gif", "gif"}} {
		prefix := "data:" + candidate.mime + ";base64,"
		if strings.HasPrefix(value, prefix) {
			mime, format, encoded = candidate.mime, candidate.format, value[len(prefix):]
			break
		}
	}
	if mime == "" {
		return errors.New("upload a PNG, JPEG, or GIF image as an inline data URL; remote URLs and paths are not supported")
	}
	if encoded == "" || strings.ContainsAny(encoded, "\r\n\t ") || len(encoded) > base64.StdEncoding.EncodedLen(MaxImageBytes) {
		return errors.New("invalid or oversized base64 image")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > MaxImageBytes {
		return errors.New("invalid or oversized base64 image")
	}
	if err := ValidateImages([]Image{{MIMEType: mime, Data: data}}); err != nil {
		return err
	}
	config, actual, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || actual != format {
		return errors.New("the image file is invalid or does not match its MIME type")
	}
	if config.Width < 1 || config.Height < 1 || config.Width > MaxWebUIImageDimension || config.Height > MaxWebUIImageDimension || int64(config.Width)*int64(config.Height) > MaxWebUIImagePixels {
		return errors.New("image dimensions exceed 8192 pixels per side or 16 million pixels; resize the image before sending")
	}
	if _, actual, err = image.Decode(bytes.NewReader(data)); err != nil || actual != format {
		return errors.New("the image file is damaged or incomplete")
	}
	return nil
}
