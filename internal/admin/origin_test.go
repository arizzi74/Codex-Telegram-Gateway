package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func TestAdminUsesConfiguredHostnameAndExactOrigin(t *testing.T) {
	for _, tc := range []struct{ configured, origin, rpID string }{
		{"https://gateway.example.com", "https://gateway.example.com", "gateway.example.com"},
		{"https://codex.operations.example.org:8443/", "https://codex.operations.example.org:8443", "codex.operations.example.org"},
	} {
		t.Run(tc.configured, func(t *testing.T) {
			server, err := New(&registry.Store{}, Config{Origin: tc.configured})
			if err != nil {
				t.Fatal(err)
			}
			cfg := server.webauthn.Config
			if server.origin != tc.origin || cfg.RPID != tc.rpID {
				t.Fatalf("configured origin/RP ID = %q / %q, want %q / %q", server.origin, cfg.RPID, tc.origin, tc.rpID)
			}
			if len(cfg.RPOrigins) != 1 || cfg.RPOrigins[0] != tc.origin || len(cfg.RPTopOrigins) != 1 || cfg.RPTopOrigins[0] != tc.origin {
				t.Fatalf("WebAuthn origins are not restricted to the configured origin: %#v / %#v", cfg.RPOrigins, cfg.RPTopOrigins)
			}
			if cfg.AuthenticatorSelection.UserVerification != protocol.VerificationRequired {
				t.Fatal("configured hostname removed required user verification")
			}
			for _, origin := range []string{tc.origin, "http://" + tc.rpID, "https://" + tc.rpID + ":9443", "https://other.example.net", ""} {
				request := httptest.NewRequest(http.MethodPost, tc.origin+"/api/v1/admin/login/begin", nil)
				request.Header.Set("Origin", origin)
				response := httptest.NewRecorder()
				if accepted := server.sameOrigin(response, request); accepted != (origin == tc.origin) {
					t.Errorf("origin %q accepted=%t for configured origin %q", origin, accepted, tc.origin)
				}
				if origin != tc.origin && response.Code != http.StatusForbidden {
					t.Errorf("foreign origin returned %d, want 403", response.Code)
				}
			}
		})
	}
}

func TestAdminRejectsInvalidOrigins(t *testing.T) {
	for _, origin := range []string{
		"", "gateway.example.com", "http://gateway.example.com", "https://", "https://:8443",
		"https://user:password@gateway.example.com", "https://gateway.example.com/admin",
		"https://gateway.example.com//", "https://gateway.example.com?query=1", "https://gateway.example.com?",
		"https://gateway.example.com#fragment", "https://gateway.example.com#", "https://gateway.example.com:",
		"https://gateway.example.com:invalid", "https://gateway.example.com:0", "https://gateway.example.com:65536",
	} {
		t.Run(origin, func(t *testing.T) {
			if _, err := New(&registry.Store{}, Config{Origin: origin}); err == nil {
				t.Fatal("invalid admin origin accepted")
			}
		})
	}
}
