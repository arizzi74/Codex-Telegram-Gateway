package admin

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/iaia/telegramgw/internal/registry"
)

const passkeyTestOrigin = "https://gateway.example.com"

// TestPasskeyRegistrationAndAuthenticationHTTP drives the same JSON ceremony
// used by app.js. The fake authenticator emits standards-shaped P-256
// registration and assertion responses; no browser or real credential is used.
func TestPasskeyRegistrationAndAuthenticationHTTP(t *testing.T) {
	for _, origin := range []string{passkeyTestOrigin, "https://codex.operations.example.org:8443"} {
		t.Run(origin, func(t *testing.T) { testPasskeyRegistrationAndAuthenticationHTTP(t, origin) })
	}
}

func testPasskeyRegistrationAndAuthenticationHTTP(t *testing.T, origin string) {
	publicURL, err := url.Parse(origin)
	if err != nil {
		t.Fatal(err)
	}
	rpID := publicURL.Hostname()
	store := adminIntegrationStore(t)
	bootstrap, err := store.BootstrapAdmin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	admin, err := New(store, Config{Origin: origin})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(admin)
	t.Cleanup(server.Close)
	client := passkeyHTTPClient(t, server)

	response := adminRequest(t, client, origin, http.MethodGet, "/admin/", nil, "", "")
	requireHTTPStatus(t, response, http.StatusOK)
	csrf := cookieValue(t, client, origin, csrfCookie)

	// HTTP origin checks run before any ceremony state is created.
	response = adminRequest(t, client, origin, http.MethodPost, "/api/v1/admin/passkeys/register/begin", map[string]string{"bootstrap_token": bootstrap}, "https://evil.example", csrf)
	requireHTTPStatus(t, response, http.StatusForbidden)

	response = adminRequest(t, client, origin, http.MethodPost, "/api/v1/admin/passkeys/register/begin", map[string]string{"bootstrap_token": bootstrap}, origin, csrf)
	var registration struct {
		CeremonyID string `json:"ceremony_id"`
		PublicKey  struct {
			Challenge string `json:"challenge"`
			User      struct {
				ID string `json:"id"`
			} `json:"user"`
			AuthenticatorSelection struct {
				ResidentKey      string `json:"residentKey"`
				UserVerification string `json:"userVerification"`
			} `json:"authenticatorSelection"`
		} `json:"publicKey"`
	}
	decodeHTTPJSON(t, response, http.StatusOK, &registration)
	if registration.PublicKey.AuthenticatorSelection.ResidentKey != "required" || registration.PublicKey.AuthenticatorSelection.UserVerification != "required" {
		t.Fatalf("registration authenticatorSelection = %#v", registration.PublicKey.AuthenticatorSelection)
	}
	userHandle, err := base64.RawURLEncoding.DecodeString(registration.PublicKey.User.ID)
	if err != nil || len(userHandle) < 16 {
		t.Fatalf("invalid discoverable user handle %q: %v", registration.PublicKey.User.ID, err)
	}
	ceremony := cookieValue(t, client, origin, ceremonyCookie)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credentialID := []byte("http-resident-passkey")

	// A client-data origin mismatch and a response without UV both fail while
	// leaving the valid ceremony available for a subsequent correct response.
	wrongOrigin := registrationCredential(t, key, credentialID, registration.PublicKey.Challenge, "https://evil.example", rpID, 0x45)
	response = registrationFinishRequest(t, client, origin, csrf, registration.CeremonyID, bootstrap, wrongOrigin)
	requireHTTPStatus(t, response, http.StatusBadRequest)
	wrongPort := registrationCredential(t, key, credentialID, registration.PublicKey.Challenge, "https://"+rpID+":9443", rpID, 0x45)
	response = registrationFinishRequest(t, client, origin, csrf, registration.CeremonyID, bootstrap, wrongPort)
	requireHTTPStatus(t, response, http.StatusBadRequest)
	withoutUV := registrationCredential(t, key, credentialID, registration.PublicKey.Challenge, origin, rpID, 0x41)
	response = registrationFinishRequest(t, client, origin, csrf, registration.CeremonyID, bootstrap, withoutUV)
	requireHTTPStatus(t, response, http.StatusBadRequest)
	validRegistration := registrationCredential(t, key, credentialID, registration.PublicKey.Challenge, origin, rpID, 0x45)
	response = registrationFinishRequest(t, client, origin, csrf, registration.CeremonyID, bootstrap, validRegistration)
	requireHTTPStatus(t, response, http.StatusCreated)

	// Restore the ceremony cookie to prove the durable challenge, rather than
	// cookie clearing alone, prevents replay.
	setCookie(client, origin, ceremonyCookie, ceremony)
	response = registrationFinishRequest(t, client, origin, csrf, registration.CeremonyID, bootstrap, validRegistration)
	requireHTTPStatus(t, response, http.StatusForbidden)

	response = adminRequest(t, passkeyHTTPClient(t, server), origin, http.MethodGet, "/api/v1/admin/dashboard", nil, "", "")
	requireHTTPStatus(t, response, http.StatusUnauthorized)

	response = adminRequest(t, client, origin, http.MethodPost, "/api/v1/admin/login/begin", struct{}{}, origin, csrf)
	var login struct {
		CeremonyID string `json:"ceremony_id"`
		PublicKey  struct {
			Challenge        string `json:"challenge"`
			RPID             string `json:"rpId"`
			UserVerification string `json:"userVerification"`
		} `json:"publicKey"`
	}
	decodeHTTPJSON(t, response, http.StatusOK, &login)
	if login.PublicKey.RPID != rpID || login.PublicKey.UserVerification != "required" {
		t.Fatalf("login options = %#v", login.PublicKey)
	}
	loginCeremony := cookieValue(t, client, origin, ceremonyCookie)
	wrongLoginOrigin := browserAssertion(t, key, credentialID, userHandle, login.PublicKey.Challenge, "https://evil.example", rpID, 0x05)
	response = loginFinishRequest(t, client, origin, csrf, login.CeremonyID, wrongLoginOrigin)
	requireHTTPStatus(t, response, http.StatusBadRequest)
	loginWithoutUV := browserAssertion(t, key, credentialID, userHandle, login.PublicKey.Challenge, origin, rpID, 0x01)
	response = loginFinishRequest(t, client, origin, csrf, login.CeremonyID, loginWithoutUV)
	requireHTTPStatus(t, response, http.StatusBadRequest)
	validAssertion := browserAssertion(t, key, credentialID, userHandle, login.PublicKey.Challenge, origin, rpID, 0x05)
	response = loginFinishRequest(t, client, origin, csrf, login.CeremonyID, validAssertion)
	requireHTTPStatus(t, response, http.StatusOK)
	if cookieValue(t, client, origin, adminCookie) == "" {
		t.Fatal("login did not set the admin session cookie")
	}

	response = adminRequest(t, client, origin, http.MethodGet, "/api/v1/admin/dashboard", nil, "", "")
	var dashboard struct {
		registry.AdminDashboard
		Bot       BotInfo `json:"bot"`
		UpdatedAt string  `json:"updated_at"`
	}
	decodeHTTPJSON(t, response, http.StatusOK, &dashboard)
	if dashboard.Bot.Status != "not_configured" || dashboard.UpdatedAt == "" || dashboard.Sessions == nil || dashboard.Workers == nil {
		t.Fatalf("incomplete dashboard response: %+v", dashboard)
	}

	setCookie(client, origin, ceremonyCookie, loginCeremony)
	csrf = cookieValue(t, client, origin, csrfCookie)
	response = loginFinishRequest(t, client, origin, csrf, login.CeremonyID, validAssertion)
	requireHTTPStatus(t, response, http.StatusForbidden)
}

func adminIntegrationStore(t *testing.T) *registry.Store {
	t.Helper()
	ctx := context.Background()
	store, err := registry.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatalf("open isolated SQLite store: %v", err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate isolated SQLite store: %v", err)
	}
	return store
}

func passkeyHTTPClient(t *testing.T, server *httptest.Server) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	dialer := &net.Dialer{}
	// Keep the test's transport explicit: requests go directly to the TLS test
	// server even though their virtual Host and WebAuthn Origin use the configured domain.
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // httptest certificate is not issued for the virtual host.
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, target.Host)
		},
	}
	return &http.Client{Transport: transport, Jar: jar}
}

func adminRequest(t *testing.T, client *http.Client, serverURL, method, path string, body any, origin, csrf string) *http.Response {
	t.Helper()
	var input io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		input = bytes.NewReader(raw)
	}
	request, err := http.NewRequest(method, serverURL+path, input)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func registrationFinishRequest(t *testing.T, client *http.Client, serverURL, csrf, ceremonyID, bootstrap string, credential json.RawMessage) *http.Response {
	t.Helper()
	return adminRequest(t, client, serverURL, http.MethodPost, "/api/v1/admin/passkeys/register/finish", map[string]any{"ceremony_id": ceremonyID, "bootstrap_token": bootstrap, "credential": credential}, serverURL, csrf)
}

func loginFinishRequest(t *testing.T, client *http.Client, serverURL, csrf, ceremonyID string, credential json.RawMessage) *http.Response {
	t.Helper()
	return adminRequest(t, client, serverURL, http.MethodPost, "/api/v1/admin/login/finish", map[string]any{"ceremony_id": ceremonyID, "credential": credential}, serverURL, csrf)
}

func decodeHTTPJSON(t *testing.T, response *http.Response, expected int, output any) {
	t.Helper()
	defer response.Body.Close()
	if response.StatusCode != expected {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("HTTP status = %d, want %d: %s", response.StatusCode, expected, body)
	}
	if err := json.NewDecoder(response.Body).Decode(output); err != nil {
		t.Fatal(err)
	}
}

func requireHTTPStatus(t *testing.T, response *http.Response, expected int) {
	t.Helper()
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != expected {
		t.Fatalf("HTTP status = %d, want %d: %s", response.StatusCode, expected, body)
	}
}

func cookieValue(t *testing.T, client *http.Client, serverURL, name string) string {
	t.Helper()
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range client.Jar.Cookies(u) {
		if cookie.Name == name {
			return cookie.Value
		}
	}
	t.Fatalf("cookie %s was not set", name)
	return ""
}

func setCookie(client *http.Client, serverURL, name, value string) {
	u, _ := url.Parse(serverURL)
	client.Jar.SetCookies(u, []*http.Cookie{{Name: name, Value: value, Path: "/", Secure: true}})
}

func registrationCredential(t *testing.T, key *ecdsa.PrivateKey, credentialID []byte, challenge, origin, rpID string, flags byte) json.RawMessage {
	t.Helper()
	clientData, err := json.Marshal(map[string]string{"type": "webauthn.create", "challenge": challenge, "origin": origin})
	if err != nil {
		t.Fatal(err)
	}
	rpIDHash := sha256.Sum256([]byte(rpID))
	authenticatorData := make([]byte, 37)
	copy(authenticatorData, rpIDHash[:])
	authenticatorData[32] = flags
	// Attested credential data: zero AAGUID, credential ID, then ES256 COSE key.
	authenticatorData = append(authenticatorData, make([]byte, 16)...)
	length := make([]byte, 2)
	binary.BigEndian.PutUint16(length, uint16(len(credentialID)))
	authenticatorData = append(authenticatorData, length...)
	authenticatorData = append(authenticatorData, credentialID...)
	authenticatorData = append(authenticatorData, coseP256(&key.PublicKey)...)
	attestation, err := cbor.Marshal(map[string]any{"fmt": "none", "authData": authenticatorData, "attStmt": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	encodedID := base64.RawURLEncoding.EncodeToString(credentialID)
	return marshalCredential(t, map[string]any{
		"id": encodedID, "rawId": encodedID, "type": "public-key",
		"response": map[string]string{"clientDataJSON": base64.RawURLEncoding.EncodeToString(clientData), "attestationObject": base64.RawURLEncoding.EncodeToString(attestation)},
	})
}

func browserAssertion(t *testing.T, key *ecdsa.PrivateKey, credentialID, userHandle []byte, challenge, origin, rpID string, flags byte) json.RawMessage {
	t.Helper()
	clientData, err := json.Marshal(map[string]string{"type": "webauthn.get", "challenge": challenge, "origin": origin})
	if err != nil {
		t.Fatal(err)
	}
	rpIDHash := sha256.Sum256([]byte(rpID))
	authenticatorData := make([]byte, 37)
	copy(authenticatorData, rpIDHash[:])
	authenticatorData[32] = flags
	binary.BigEndian.PutUint32(authenticatorData[33:], 1)
	clientHash := sha256.Sum256(clientData)
	signed := append(append([]byte{}, authenticatorData...), clientHash[:]...)
	digest := sha256.Sum256(signed)
	signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	encodedID := base64.RawURLEncoding.EncodeToString(credentialID)
	return marshalCredential(t, map[string]any{
		"id": encodedID, "rawId": encodedID, "type": "public-key",
		"response": map[string]string{
			"authenticatorData": base64.RawURLEncoding.EncodeToString(authenticatorData),
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientData),
			"signature":         base64.RawURLEncoding.EncodeToString(signature),
			"userHandle":        base64.RawURLEncoding.EncodeToString(userHandle),
		},
	})
}

func marshalCredential(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(fmt.Errorf("marshal fake credential: %w", err))
	}
	return raw
}
