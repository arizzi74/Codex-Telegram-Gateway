package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func webUIImageFixture(t *testing.T, mime string) []byte {
	t.Helper()
	var data bytes.Buffer
	var err error
	switch mime {
	case "image/png":
		err = png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 1, 1)))
	case "image/jpeg":
		err = jpeg.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 1, 1)), nil)
	case "image/gif":
		err = gif.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 1, 1)), nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}
func webUIImageURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func TestWebUIImageURLValidatesRealFilesAndBoundsDecode(t *testing.T) {
	for _, mime := range []string{"image/png", "image/jpeg", "image/gif"} {
		if err := ValidateWebUIImageURL(webUIImageURL(mime, webUIImageFixture(t, mime))); err != nil {
			t.Fatalf("%s: %v", mime, err)
		}
	}
	pngData := webUIImageFixture(t, "image/png")
	for _, input := range []string{
		"https://example.invalid/image.png", "file:///private/image.png", "/private/image.png", "data:image/svg+xml;base64,PHN2Zy8+", "data:image/webp;base64,UklGRg==",
		webUIImageURL("image/png", []byte("\x89PNG\r\n\x1a\n")), webUIImageURL("image/jpeg", pngData), webUIImageURL("image/png", pngData[:len(pngData)-10]),
		"data:image/png;foo=bar;base64," + base64.StdEncoding.EncodeToString(pngData), "data:image/png;base64,!!!!", "data:image/png;base64,",
		webUIImageURL("image/png", pngData) + "=", webUIImageURL("image/png", pngData) + "\n", "data:image/png;base64," + strings.Repeat("A", base64.StdEncoding.EncodedLen(MaxImageBytes)+4),
	} {
		if err := ValidateWebUIImageURL(input); err == nil {
			t.Fatalf("unsafe image accepted: %.120s", input)
		}
	}
	for _, dimensions := range [][2]uint32{{8193, 1}, {4096, 4096}} {
		data := append([]byte(nil), pngData...)
		binary.BigEndian.PutUint32(data[16:20], dimensions[0])
		binary.BigEndian.PutUint32(data[20:24], dimensions[1])
		binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
		if err := ValidateWebUIImageURL(webUIImageURL("image/png", data)); err == nil || !strings.Contains(err.Error(), "dimensions") {
			t.Fatalf("unbounded decoded image %v: %v", dimensions, err)
		}
	}
}

func TestWebUIMaximumImageFitsNestedRelayEnvelopeUnchanged(t *testing.T) {
	data := make([]byte, MaxImageBytes)
	copy(data, webUIImageFixture(t, "image/png"))
	url := webUIImageURL("image/png", data)
	if err := ValidateWebUIImageURL(url); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"id": 1, "method": "turn/start", "params": map[string]any{"input": []map[string]string{{"type": "text", "text": strings.Repeat("x", MaxWebUITextBytes)}, {"type": "image", "url": url}}}})
	if err != nil {
		t.Fatal(err)
	}
	frame := WebUIFrame{ID: uuid.NewString(), Action: "input", Data: raw}
	if err := frame.Validate(); err != nil {
		t.Fatal(err)
	}
	envelope, err := NewEnvelope("webui", frame)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(envelope)
	if len(encoded) >= MaxFrameBytes {
		t.Fatal("maximum image exceeds outer envelope")
	}
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	received, err := Payload[WebUIFrame](decoded)
	if err != nil || !bytes.Equal(received.Data, raw) {
		t.Fatal("inline image changed in transport")
	}
}
