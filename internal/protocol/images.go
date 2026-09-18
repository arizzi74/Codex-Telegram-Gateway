package protocol

import (
	"errors"
	"net/http"
)

const MaxImageBytes = 10 << 20
const MaxImageCount = 1

// Image carries immutable content, rather than an expiring download URL or a
// path that may name a different file on the receiving worker.
type Image struct {
	MIMEType string `json:"mime_type"`
	Data     []byte `json:"data"`
}

// ValidateImages bounds decoded content as well as the base64 wire payload.
// Content sniffing prevents mislabeled documents from becoming image inputs.
func ValidateImages(images []Image) error {
	if len(images) > MaxImageCount {
		return errors.New("too many input images")
	}
	for _, image := range images {
		if len(image.Data) == 0 || len(image.Data) > MaxImageBytes {
			return errors.New("input image size is invalid")
		}
		switch image.MIMEType {
		case "image/jpeg", "image/png", "image/webp", "image/gif":
		default:
			return errors.New("input image type is unsupported")
		}
		if http.DetectContentType(image.Data) != image.MIMEType {
			return errors.New("input image content does not match its type")
		}
	}
	return nil
}
