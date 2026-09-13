package admin

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/go-webauthn/webauthn/protocol"
	wa "github.com/go-webauthn/webauthn/webauthn"
)

// This is a full P-256 assertion assembled as an authenticator would emit it:
// client data, rpIdHash, UP+UV flags, a COSE public key, and an ECDSA
// signature. It protects the critical library integration without requiring a
// browser or a human-owned passkey in CI.
func TestP256PasskeyAssertionRequiresUserVerification(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credentialID := []byte("test-resident-passkey")
	handle := make([]byte, 32)
	if _, err = rand.Read(handle); err != nil {
		t.Fatal(err)
	}
	credential := wa.Credential{ID: credentialID, PublicKey: coseP256(&key.PublicKey), Authenticator: wa.Authenticator{SignCount: 0}}
	user := adminUser{handle: handle, name: "test", creds: []wa.Credential{credential}}
	w, err := wa.New(&wa.Config{RPID: "gateway.example.com", RPDisplayName: "test", RPOrigins: []string{"https://gateway.example.com"}, RPTopOrigins: []string{"https://gateway.example.com"}, AuthenticatorSelection: protocol.AuthenticatorSelection{UserVerification: protocol.VerificationRequired}})
	if err != nil {
		t.Fatal(err)
	}
	_, session, err := w.BeginLogin(user, wa.WithLoginOrigin("https://gateway.example.com"), wa.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		t.Fatal(err)
	}
	body := signedAssertion(t, key, credentialID, session.Challenge, 0x05)
	parsed, err := protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.ValidateLogin(user, *session, parsed); err != nil {
		t.Fatalf("valid P-256 user-verified assertion rejected: %v", err)
	}
	// The same otherwise-valid assertion without the UV bit must be rejected.
	body = signedAssertion(t, key, credentialID, session.Challenge, 0x01)
	parsed, err = protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.ValidateLogin(user, *session, parsed); err == nil {
		t.Fatal("assertion without user verification was accepted")
	}
}

func signedAssertion(t *testing.T, key *ecdsa.PrivateKey, id []byte, challenge string, flags byte) []byte {
	t.Helper()
	client := []byte(`{"type":"webauthn.get","challenge":"` + challenge + `","origin":"https://gateway.example.com"}`)
	rp := sha256.Sum256([]byte("gateway.example.com"))
	auth := make([]byte, 37)
	copy(auth, rp[:])
	auth[32] = flags
	binary.BigEndian.PutUint32(auth[33:], 1)
	clientHash := sha256.Sum256(client)
	signed := append(append([]byte{}, auth...), clientHash[:]...)
	digest := sha256.Sum256(signed)
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	v := map[string]any{"id": base64.RawURLEncoding.EncodeToString(id), "rawId": base64.RawURLEncoding.EncodeToString(id), "type": "public-key", "response": map[string]string{"authenticatorData": base64.RawURLEncoding.EncodeToString(auth), "clientDataJSON": base64.RawURLEncoding.EncodeToString(client), "signature": base64.RawURLEncoding.EncodeToString(sig)}}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Minimal canonical COSE_Key for ES256: {1:2, 3:-7, -1:1, -2:x, -3:y}.
func coseP256(key *ecdsa.PublicKey) []byte {
	x := key.X.FillBytes(make([]byte, 32))
	y := key.Y.FillBytes(make([]byte, 32))
	out := []byte{0xa5, 0x01, 0x02, 0x03, 0x26, 0x20, 0x01, 0x21, 0x58, 0x20}
	out = append(out, x...)
	out = append(out, 0x22, 0x58, 0x20)
	return append(out, y...)
}
