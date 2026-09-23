package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/hkdf"
)

type clientFunc func(*http.Request) (*http.Response, error)

func (f clientFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func testSubscription(t *testing.T) (Subscription, *ecdh.PrivateKey, []byte) {
	t.Helper()
	receiver, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	s := Subscription{Endpoint: "https://web.push.apple.com/device/private-endpoint"}
	s.Keys.P256DH = base64.RawURLEncoding.EncodeToString(receiver.PublicKey().Bytes())
	s.Keys.Auth = base64.RawURLEncoding.EncodeToString(auth)
	return s, receiver, auth
}

func TestSendEncryptsPayloadAndAuthenticatesApplication(t *testing.T) {
	subscription, receiver, auth := testSubscription(t)
	keys, err := GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	sender, err := NewSender(keys, "https://gateway.example")
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	eventID, sessionID := uuid.New(), uuid.New()
	var ciphertext []byte
	sender.client = clientFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != "POST" || r.URL.String() != subscription.Endpoint || r.Header.Get("Content-Encoding") != "aes128gcm" || r.Header.Get("Content-Type") != "application/octet-stream" {
			t.Fatalf("invalid encrypted request: %s %s", r.Method, r.Header.Get("Content-Encoding"))
		}
		if topic := r.Header.Get("Topic"); len(topic) != 32 || strings.Contains(topic, "-") {
			t.Fatalf("invalid push topic %q", topic)
		}
		if _, ok := r.Context().Deadline(); !ok {
			t.Fatal("network request has no timeout")
		}
		authorization := strings.TrimPrefix(r.Header.Get("Authorization"), "vapid t=")
		parts := strings.Split(authorization, ", k=")
		if len(parts) != 2 || parts[1] != keys.PublicKey {
			t.Fatal("invalid VAPID header")
		}
		public, _ := base64.RawURLEncoding.DecodeString(keys.PublicKey)
		x, y := elliptic.Unmarshal(elliptic.P256(), public)
		token, err := jwt.Parse(parts[0], func(*jwt.Token) (any, error) { return &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, nil }, jwt.WithValidMethods([]string{"ES256"}), jwt.WithAudience("https://web.push.apple.com"))
		if err != nil || !token.Valid {
			t.Fatalf("VAPID signature: %v", err)
		}
		if token.Claims.(jwt.MapClaims)["sub"] != "https://gateway.example" {
			t.Fatal("wrong application identity")
		}
		ciphertext, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(ciphertext, []byte("A Codex turn")) || bytes.Contains(ciphertext, []byte(sessionID.String())) {
			t.Fatal("notification leaked plaintext")
		}
		return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	if result := sender.Send(t.Context(), subscription, eventID, sessionID, time.Now().Add(15*time.Minute)); result != "delivered" {
		t.Fatalf("result=%s", result)
	}
	// Decrypt independently as the receiving browser, following RFC 8291.
	if len(ciphertext) != 4096 || binary.BigEndian.Uint32(ciphertext[16:20]) != 4096 || ciphertext[20] != 65 {
		t.Fatal("invalid aes128gcm record")
	}
	serverPublic, err := ecdh.P256().NewPublicKey(ciphertext[21:86])
	if err != nil {
		t.Fatal(err)
	}
	shared, err := receiver.ECDH(serverPublic)
	if err != nil {
		t.Fatal(err)
	}
	derive := func(secret, salt, info []byte, size int) []byte {
		out := make([]byte, size)
		if _, err := io.ReadFull(hkdf.New(sha256.New, secret, salt, info), out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	info := append([]byte("WebPush: info\x00"), receiver.PublicKey().Bytes()...)
	info = append(info, serverPublic.Bytes()...)
	material := derive(shared, auth, info, 32)
	key := derive(material, ciphertext[:16], []byte("Content-Encoding: aes128gcm\x00"), 16)
	nonce := derive(material, ciphertext[:16], []byte("Content-Encoding: nonce\x00"), 12)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext[86:], nil)
	if err != nil {
		t.Fatal(err)
	}
	plaintext = bytes.TrimRight(plaintext, "\x00")
	if len(plaintext) == 0 || plaintext[len(plaintext)-1] != 2 {
		t.Fatal("missing final record delimiter")
	}
	var payload map[string]string
	if err := json.Unmarshal(plaintext[:len(plaintext)-1], &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) != 5 || payload["type"] != "turn_finished" || payload["title"] != "Codex" || payload["body"] != "A Codex turn has finished." || payload["url"] != "/tgw/webui/?session_id="+sessionID.String() || payload["tag"] != eventID.String() {
		t.Fatalf("unexpected notification: %v", payload)
	}
}

func TestValidateSubscriptionRejectsSSRFAndMalformedKeys(t *testing.T) {
	s, _, _ := testSubscription(t)
	for _, endpoint := range []string{"http://fcm.googleapis.com/x", "https://127.0.0.1/x", "https://[::1]/x", "https://metadata.google.internal/x", "https://fcm.googleapis.com.evil.example/x", "https://evilpush.apple.com/x", "https://user@fcm.googleapis.com/x", "https://fcm.googleapis.com:444/x", "https://fcm.googleapis.com/x#fragment", "https://fcm.googleapis.com./x", "https://example.org/", "https://fcm.googleapis.com/" + strings.Repeat("a", 4096)} {
		t.Run(endpoint[:min(len(endpoint), 80)], func(t *testing.T) {
			s.Endpoint = endpoint
			if ValidateSubscription(s) == nil {
				t.Fatal("unsafe endpoint accepted")
			}
		})
	}
	for _, endpoint := range []string{"https://fcm.googleapis.com/fcm/send/token", "https://web.push.apple.com/token", "https://updates.push.services.mozilla.com/wpush/v2/token", "https://wns2.notify.windows.com/token"} {
		s.Endpoint = endpoint
		if err := ValidateSubscription(s); err != nil {
			t.Fatalf("valid provider rejected: %v", err)
		}
	}
	s.Keys.Auth = "short"
	if ValidateSubscription(s) == nil {
		t.Fatal("invalid auth accepted")
	}
	s, _, _ = testSubscription(t)
	s.Keys.P256DH = base64.RawURLEncoding.EncodeToString(make([]byte, 65))
	if ValidateSubscription(s) == nil {
		t.Fatal("invalid curve point accepted")
	}
}

func TestSafeDialRejectsPrivateOrMixedDNSAnswers(t *testing.T) {
	original := lookupNetIP
	t.Cleanup(func() { lookupNetIP = original })
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.0.1", "169.254.169.254", "100.100.100.200", "::1", "::ffff:127.0.0.1", "fc00::1", "fe80::1", "2001:db8::1", "64:ff9b::7f00:1", "2002:7f00:1::1"} {
		if publicAddress(netip.MustParseAddr(ip)) {
			t.Errorf("nonpublic address accepted: %s", ip)
		}
		lookupNetIP = func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr(ip)}, nil
		}
		conn, err := safeDialContext(t.Context(), "tcp", "fcm.googleapis.com:443")
		if err == nil || conn != nil {
			t.Fatal("private DNS answer reached dial")
		}
	}
	for _, ip := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !publicAddress(netip.MustParseAddr(ip)) {
			t.Errorf("public address rejected: %s", ip)
		}
	}
}

func TestProviderResponsesAndInvalidInputAreBounded(t *testing.T) {
	subscription, _, _ := testSubscription(t)
	keys, _ := GenerateKeys()
	sender, err := NewSender(keys, "https://gateway.example")
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	client := sender.client.(*http.Client)
	if client.Timeout != RequestTimeout || client.CheckRedirect(nil, nil) != http.ErrUseLastResponse || client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("unsafe HTTP client")
	}
	for code, want := range map[int]string{200: "delivered", 201: "delivered", 302: "discard", 400: "discard", 401: "discard", 403: "discard", 404: "gone", 410: "gone", 408: "retry", 429: "retry", 500: "retry", 503: "retry"} {
		body := &countedBody{remaining: 1 << 20}
		sender.client = clientFunc(func(*http.Request) (*http.Response, error) { return &http.Response{StatusCode: code, Body: body}, nil })
		if got := sender.Send(t.Context(), subscription, uuid.New(), uuid.New(), time.Now().Add(time.Minute)); got != want {
			t.Fatalf("status%d got%s want%s", code, got, want)
		}
		if !body.closed || body.read > 4096 {
			t.Fatalf("unbounded response read: %+v", body)
		}
	}
	sender.client = clientFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("expired or invalid input made network request")
		return nil, nil
	})
	if got := sender.Send(t.Context(), subscription, uuid.New(), uuid.New(), time.Now().Add(-time.Second)); got != "discard" {
		t.Fatal(got)
	}
	subscription.Endpoint = "https://localhost/private"
	if got := sender.Send(t.Context(), subscription, uuid.New(), uuid.New(), time.Now().Add(time.Minute)); got != "discard" {
		t.Fatal(got)
	}
}

type countedBody struct {
	remaining, read int
	closed          bool
}

func (b *countedBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), b.remaining)
	clear(p[:n])
	b.remaining -= n
	b.read += n
	return n, nil
}
func (b *countedBody) Close() error { b.closed = true; return nil }
