package releasemanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
)

const testWorkerEnrollmentCode = "ABCDEFGH2345"
const testWorkerEnrollmentToken = "cwk_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func setupEnrollmentServer(t *testing.T, m *Manager, handler func(http.ResponseWriter, *http.Request, protocol.RedeemWorkerEnrollmentRequest)) string {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != workerEnrollmentRedeemPath || r.URL.RawQuery != "" || r.URL.Fragment != "" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected enrollment request: method=%s path=%s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body protocol.RedeemWorkerEnrollmentRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		handler(w, r, body)
	}))
	t.Cleanup(server.Close)
	m.HTTP = server.Client()
	return server.URL + workerEnrollmentPagePath + "#" + testWorkerEnrollmentCode
}

func writeSetupEnrollmentResponse(w http.ResponseWriter, r *http.Request, profile string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(workerEnrollmentResponse{
		WorkerID: "00000000-0000-4000-8000-000000000001", Token: testWorkerEnrollmentToken,
		GatewayURL: "wss://" + r.Host + config.WorkerConnectPath, ServiceAccess: profile,
	})
}

func TestParseWorkerEnrollmentURLRequiresExactCapabilityFormat(t *testing.T) {
	valid := "https://gateway.example:8443/tgw/enroll/#abcdefgh2345"
	enrollment, err := parseWorkerEnrollmentURL(valid)
	if err != nil || enrollment.origin != "https://gateway.example:8443" || enrollment.code != testWorkerEnrollmentCode {
		t.Fatal("valid URL was not normalized", err)
	}
	for _, value := range []string{
		"http://gateway.example/tgw/enroll/#" + testWorkerEnrollmentCode,
		"https://user:secret@gateway.example/tgw/enroll/#" + testWorkerEnrollmentCode,
		"https://gateway.example/tgw/enroll/?#" + testWorkerEnrollmentCode,
		"https://gateway.example/tgw/enroll/?token=private#" + testWorkerEnrollmentCode,
		"https://gateway.example/tgw/enroll#" + testWorkerEnrollmentCode,
		"https://gateway.example/tgw/%65nroll/#" + testWorkerEnrollmentCode,
		"https://gateway.example/tgw/enroll/#%41BCDEFGH2345",
		"https://gateway.example/tgw/enroll/#ABCDEFGH2340",
		"https://gateway.example/tgw/enroll/#ABCDEFGH234I",
		"https://gateway.example/tgw/enroll/#short",
		"https://gateway.example:0/tgw/enroll/#" + testWorkerEnrollmentCode,
		"https:///tgw/enroll/#" + testWorkerEnrollmentCode,
	} {
		if _, err := parseWorkerEnrollmentURL(value); err == nil || strings.Contains(err.Error(), value) || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid URL accepted or echoed")
		}
	}
}

func TestRedeemWorkerEnrollmentUsesSameOriginTLSAndPlatform(t *testing.T) {
	m, l, _ := setupFixture(t)
	var calls atomic.Int32
	value := setupEnrollmentServer(t, m, func(w http.ResponseWriter, r *http.Request, body protocol.RedeemWorkerEnrollmentRequest) {
		calls.Add(1)
		if body != (protocol.RedeemWorkerEnrollmentRequest{Code: testWorkerEnrollmentCode, Name: "Worker one", OS: "linux", Arch: "arm64"}) {
			t.Error("incorrect enrollment payload")
		}
		writeSetupEnrollmentResponse(w, r, workerServiceFull)
	})
	enrollment, _ := parseWorkerEnrollmentURL(strings.ToLower(value))
	response, err := m.redeemWorkerEnrollment(t.Context(), enrollment, "Worker one", l)
	if err != nil || calls.Load() != 1 || response.Token != testWorkerEnrollmentToken || response.ServiceAccess != workerServiceFull {
		t.Fatal("redemption failed", err)
	}
}

func TestRedeemWorkerEnrollmentTLSOriginNormalizationPreservesPortBoundary(t *testing.T) {
	for _, tc := range []struct {
		origin, gateway string
		valid           bool
	}{
		{"https://example.com", "wss://EXAMPLE.COM:443", true},
		{"https://EXAMPLE.COM:443", "wss://example.com", true},
		{"https://example.com:8443", "wss://EXAMPLE.COM:8443", true},
		{"https://example.com", "wss://example.com:8443", false},
		{"https://example.com:8443", "wss://example.com", false},
	} {
		t.Run(tc.gateway+"from"+tc.origin, func(t *testing.T) {
			m, l, _ := setupFixture(t)
			value := setupEnrollmentServer(t, m, func(w http.ResponseWriter, r *http.Request, _ protocol.RedeemWorkerEnrollmentRequest) {
				_ = json.NewEncoder(w).Encode(workerEnrollmentResponse{WorkerID: "00000000-0000-4000-8000-000000000001", Token: testWorkerEnrollmentToken, GatewayURL: tc.gateway + config.WorkerConnectPath, ServiceAccess: workerServiceRestricted})
			})
			serverURL, _ := url.Parse(value)
			transport := m.HTTP.Transport.(*http.Transport).Clone()
			transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, serverURL.Host)
			}
			t.Cleanup(transport.CloseIdleConnections)
			m.HTTP.Transport = transport
			enrollment, _ := parseWorkerEnrollmentURL(tc.origin + workerEnrollmentPagePath + "#" + testWorkerEnrollmentCode)
			_, err := m.redeemWorkerEnrollment(t.Context(), enrollment, "Worker one", l)
			if (err == nil) != tc.valid {
				t.Fatal("incorrect origin normalization", err)
			}
		})
	}
}

func TestRedeemWorkerEnrollmentRejectsResponsesWithoutRetryOrSecretOutput(t *testing.T) {
	for _, kind := range []string{"expired", "used", "redirect", "invalid-json", "unknown-field", "extra-json", "invalid-utf8", "oversize", "short-token", "control-token", "unicode-token", "bad-id", "empty-id", "other-origin", "gateway-query", "gateway-path", "bad-profile", "server-error", "lost-response", "truncated"} {
		t.Run(kind, func(t *testing.T) {
			m, l, _ := setupFixture(t)
			var calls atomic.Int32
			value := setupEnrollmentServer(t, m, func(w http.ResponseWriter, r *http.Request, _ protocol.RedeemWorkerEnrollmentRequest) {
				calls.Add(1)
				response := workerEnrollmentResponse{
					WorkerID: "00000000-0000-4000-8000-000000000001", Token: testWorkerEnrollmentToken,
					GatewayURL: "wss://" + r.Host + config.WorkerConnectPath, ServiceAccess: workerServiceRestricted,
				}
				switch kind {
				case "expired", "used":
					w.WriteHeader(http.StatusGone)
					fmt.Fprint(w, testWorkerEnrollmentToken)
					return
				case "redirect":
					http.Redirect(w, r, "/private/"+testWorkerEnrollmentToken, http.StatusTemporaryRedirect)
					return
				case "invalid-json":
					fmt.Fprint(w, testWorkerEnrollmentToken)
					return
				case "unknown-field":
					fmt.Fprintf(w, `{"worker_id":"00000000-0000-4000-8000-000000000001","token":"%s","gateway_url":"wss://%s%s","service_access":"restricted","private":"secret"}`, testWorkerEnrollmentToken, r.Host, config.WorkerConnectPath)
					return
				case "invalid-utf8":
					w.Write([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'})
					return
				case "oversize":
					fmt.Fprint(w, strings.Repeat("x", workerEnrollmentBodyLimit+1))
					return
				case "short-token":
					response.Token = "private"
				case "control-token":
					response.Token += "\nprivate"
				case "unicode-token":
					response.Token += "è"
				case "bad-id":
					response.WorkerID = "private-invalid-uuid"
				case "empty-id":
					response.WorkerID = "00000000-0000-0000-0000-000000000000"
				case "other-origin":
					response.GatewayURL = "wss://attacker.example" + config.WorkerConnectPath
				case "gateway-query":
					response.GatewayURL += "?token=private"
				case "gateway-path":
					response.GatewayURL += "/"
				case "bad-profile":
					response.ServiceAccess = "private-invalid"
				case "server-error":
					w.WriteHeader(http.StatusServiceUnavailable)
					fmt.Fprint(w, testWorkerEnrollmentToken)
					return
				case "lost-response":
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					connection.Close()
					return
				case "truncated":
					w.Header().Set("Content-Length", "9999")
				}
				_ = json.NewEncoder(w).Encode(response)
				if kind == "extra-json" {
					fmt.Fprint(w, "{}")
				}
			})
			enrollment, _ := parseWorkerEnrollmentURL(value)
			_, err := m.redeemWorkerEnrollment(t.Context(), enrollment, "Worker one", l)
			if err == nil || calls.Load() != 1 || strings.Contains(err.Error(), testWorkerEnrollmentToken) || strings.Contains(err.Error(), testWorkerEnrollmentCode) || strings.Contains(err.Error(), value) {
				t.Fatal("response was accepted, retried, or leaked", err, calls.Load())
			}
			var rejected *workerEnrollmentRejected
			if errors.As(err, &rejected) != (kind == "expired" || kind == "used") {
				t.Fatal("incorrect retry safety classification", err)
			}
		})
	}
}

func TestRedeemWorkerEnrollmentCancellationAndNetworkErrorPrivacy(t *testing.T) {
	m, l, _ := setupFixture(t)
	enrollment, _ := parseWorkerEnrollmentURL("https://gateway.example/tgw/enroll/#" + testWorkerEnrollmentCode)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
		t.Fatal("cancelled enrollment performed network IO")
		return nil, nil
	})
	if _, err := m.redeemWorkerEnrollment(ctx, enrollment, "Worker one", l); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var calls int
	m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, fmt.Errorf("private network URL: %s#%s", r.URL, testWorkerEnrollmentCode)
	})
	_, err := m.redeemWorkerEnrollment(t.Context(), enrollment, "Worker one", l)
	if err == nil || calls != 1 || strings.Contains(err.Error(), "gateway.example") || strings.Contains(err.Error(), testWorkerEnrollmentCode) {
		t.Fatal("network error leaked or was retried", err)
	}
}

func TestRedeemWorkerEnrollmentCanceledAfterRequestWarnsAboutLostResponse(t *testing.T) {
	m, l, _ := setupFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	value := setupEnrollmentServer(t, m, func(w http.ResponseWriter, r *http.Request, _ protocol.RedeemWorkerEnrollmentRequest) {
		cancel()
		<-r.Context().Done()
	})
	enrollment, _ := parseWorkerEnrollmentURL(value)
	_, err := m.redeemWorkerEnrollment(ctx, enrollment, "Worker one", l)
	if err == nil || !strings.Contains(err.Error(), "may have created a worker") || strings.Contains(err.Error(), value) {
		t.Fatal("interrupted enrollment lacked recovery guidance", err)
	}
}
