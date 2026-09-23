// Package webpush encrypts and delivers small Web Push notifications. It never
// sends conversation text and only connects directly to known push providers.
package webpush

import (
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	push "github.com/SherClockHolmes/webpush-go"
	"github.com/google/uuid"
)

const RequestTimeout = 10 * time.Second

var ErrInvalidSubscription = errors.New("invalid or unsupported push subscription")

type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256DH string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	ExpirationTime *float64 `json:"expirationTime,omitempty"`
}

type Keys struct{ PrivateKey, PublicKey string }

func GenerateKeys() (Keys, error) {
	private, public, err := push.GenerateVAPIDKeys()
	return Keys{PrivateKey: private, PublicKey: public}, err
}

// ValidateSubscription does not make network requests. DNS addresses are
// validated again at connection time, so DNS changes cannot bypass the guard.
func ValidateSubscription(s Subscription) error {
	if len(s.Endpoint) > 4096 {
		return ErrInvalidSubscription
	}
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Opaque != "" || u.Host == "" || (u.Port() != "" && u.Port() != "443") || !providerHost(u.Hostname()) {
		return ErrInvalidSubscription
	}
	public, err := decodeKey(s.Keys.P256DH)
	if err != nil || len(public) != 65 {
		return ErrInvalidSubscription
	}
	if _, err = ecdh.P256().NewPublicKey(public); err != nil {
		return ErrInvalidSubscription
	}
	auth, err := decodeKey(s.Keys.Auth)
	if err != nil || len(auth) != 16 {
		return ErrInvalidSubscription
	}
	return nil
}

func providerHost(host string) bool {
	host = strings.ToLower(host)
	if host == "fcm.googleapis.com" {
		return true
	}
	for _, domain := range []string{"push.apple.com", "push.services.mozilla.com", "notify.windows.com"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func decodeKey(value string) ([]byte, error) {
	if len(value) > 128 {
		return nil, ErrInvalidSubscription
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(value)
	}
	return decoded, err
}

type Sender struct {
	keys    Keys
	subject string
	client  push.HTTPClient
}

func NewSender(keys Keys, subject string) (*Sender, error) {
	u, err := url.Parse(subject)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, errors.New("invalid push application origin")
	}
	private, err := decodeKey(keys.PrivateKey)
	if err != nil {
		return nil, errors.New("invalid push application key")
	}
	key, err := ecdh.P256().NewPrivateKey(private)
	public, publicErr := decodeKey(keys.PublicKey)
	if err != nil || publicErr != nil || !equalBytes(key.PublicKey().Bytes(), public) {
		return nil, errors.New("invalid push application key")
	}
	transport := &http.Transport{
		Proxy: nil, DialContext: safeDialContext, ForceAttemptHTTP2: true,
		MaxIdleConns: 8, MaxIdleConnsPerHost: 2, IdleConnTimeout: time.Minute,
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 8 * time.Second,
		MaxResponseHeaderBytes: 16 << 10,
	}
	client := &http.Client{Transport: transport, Timeout: RequestTimeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	return &Sender{keys: keys, subject: subject, client: client}, nil
}

func equalBytes(a, b []byte) bool { return string(a) == string(b) }

func (s *Sender) Close() {
	if client, ok := s.client.(*http.Client); ok {
		client.CloseIdleConnections()
	}
}

// Send returns a bounded queue disposition. Neither provider error bodies nor
// sensitive endpoint URLs leave this layer, including through wrapped errors.
func (s *Sender) Send(ctx context.Context, subscription Subscription, eventID, sessionID uuid.UUID, expiresAt time.Time) string {
	if eventID == uuid.Nil || sessionID == uuid.Nil || ValidateSubscription(subscription) != nil {
		return "discard"
	}
	ttl := int(time.Until(expiresAt).Seconds())
	if ttl <= 0 {
		return "discard"
	}
	if ttl > 900 {
		ttl = 900
	}
	message, _ := json.Marshal(struct {
		Type  string `json:"type"`
		Title string `json:"title"`
		Body  string `json:"body"`
		URL   string `json:"url"`
		Tag   string `json:"tag"`
	}{"turn_finished", "Codex", "A Codex turn has finished.", "/tgw/webui/?session_id=" + sessionID.String(), eventID.String()})
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	response, err := push.SendNotificationWithContext(ctx, message, &push.Subscription{Endpoint: subscription.Endpoint, Keys: push.Keys{Auth: subscription.Keys.Auth, P256dh: subscription.Keys.P256DH}}, &push.Options{
		HTTPClient: s.client, Subscriber: s.subject, VAPIDPrivateKey: s.keys.PrivateKey, VAPIDPublicKey: s.keys.PublicKey,
		TTL: ttl, Urgency: push.UrgencyNormal, Topic: strings.ReplaceAll(eventID.String(), "-", ""),
	})
	if err != nil {
		return "retry"
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		return "delivered"
	case response.StatusCode == 404 || response.StatusCode == 410:
		return "gone"
	case response.StatusCode == 408 || response.StatusCode == 429 || response.StatusCode >= 500:
		return "retry"
	default:
		return "discard"
	}
}

var lookupNetIP = net.DefaultResolver.LookupNetIP

func safeDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port != "443" || !providerHost(host) {
		return nil, ErrInvalidSubscription
	}
	addresses, err := lookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return nil, errors.New("push provider lookup failed")
	}
	for _, address := range addresses {
		if !publicAddress(address) {
			return nil, errors.New("push provider address is not public")
		}
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	for _, address := range addresses {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(address.String(), port))
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, errors.New("push provider connection failed")
}

var excludedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"), netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
}

func publicAddress(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.Zone() != "" {
		return false
	}
	for _, prefix := range excludedPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}
