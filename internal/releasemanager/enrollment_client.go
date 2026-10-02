package releasemanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
)

const (
	workerEnrollmentPagePath   = protocol.WorkerEnrollmentPath
	workerEnrollmentRedeemPath = protocol.WorkerEnrollmentRedeemPath
	workerEnrollmentBodyLimit  = 16 * 1024
)

type workerEnrollmentURL struct {
	origin string
	code   string
}

type workerEnrollmentResponse = protocol.RedeemWorkerEnrollmentResponse

// A definite rejection may be retried with a newly generated URL. An ambiguous
// result must never automatically repeat the request, because the first request
// may already have created the worker and returned its only copy of the token.
type workerEnrollmentRejected struct{}

func (*workerEnrollmentRejected) Error() string {
	return "the enrollment URL is invalid, expired, or already used; create a fresh URL in the gateway admin console (URLs last 10 minutes and can be used once)"
}

const workerEnrollmentUncertain = "enrollment did not return usable credentials; check the gateway admin worker list for a worker created by this attempt, then create a fresh enrollment URL and rerun the installer"

func parseWorkerEnrollmentURL(value string) (workerEnrollmentURL, error) {
	invalid := errors.New("enrollment URL must be an HTTPS URL ending in /tgw/enroll/# followed by its 12-character code, without credentials or query parameters")
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery ||
		u.Path != workerEnrollmentPagePath || u.RawPath != "" || u.RawFragment != "" || len(u.Fragment) != 12 {
		return workerEnrollmentURL{}, invalid
	}
	origin, err := config.ParseHTTPSOrigin((&url.URL{Scheme: "https", Host: u.Host}).String())
	if err != nil {
		return workerEnrollmentURL{}, invalid
	}
	code, err := protocol.NormalizeWorkerEnrollmentCode(u.Fragment)
	if err != nil {
		return workerEnrollmentURL{}, invalid
	}
	return workerEnrollmentURL{origin: origin.String(), code: code}, nil
}

func normalizeWorkerEnrollmentURL(value string) (string, error) {
	if _, err := parseWorkerEnrollmentURL(value); err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func validateWorkerEnrollmentResponse(response workerEnrollmentResponse, origin string) error {
	id, err := uuid.Parse(response.WorkerID)
	if err != nil || id == uuid.Nil || id.String() != response.WorkerID {
		return errors.New(workerEnrollmentUncertain)
	}
	if len(response.Token) < 32 || len(response.Token) > 4096 || strings.ContainsFunc(response.Token, func(r rune) bool { return r < '!' || r > '~' }) {
		return errors.New(workerEnrollmentUncertain)
	}
	u, err := url.Parse(origin)
	if err != nil {
		return errors.New(workerEnrollmentUncertain)
	}
	gateway, err := url.Parse(response.GatewayURL)
	if err != nil || gateway.Scheme != "wss" || gateway.User != nil || gateway.RawQuery != "" || gateway.ForceQuery ||
		strings.Contains(response.GatewayURL, "#") || gateway.Path != config.WorkerConnectPath || gateway.RawPath != "" {
		return errors.New(workerEnrollmentUncertain)
	}
	if _, err := config.ParseHTTPSOrigin((&url.URL{Scheme: "https", Host: gateway.Host}).String()); err != nil {
		return errors.New(workerEnrollmentUncertain)
	}
	if enrollmentOriginKey(gateway) != enrollmentOriginKey(u) || (response.ServiceAccess != workerServiceRestricted && response.ServiceAccess != workerServiceFull) {
		return errors.New(workerEnrollmentUncertain)
	}
	return nil
}

// Browser URL parsing normalizes DNS casing, IP notation, and the default HTTPS
// port. Compare those representations of the same origin without allowing a
// different hostname or nondefault port in the returned connection endpoint.
func enrollmentOriginKey(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	port := 443
	if u.Port() != "" {
		port, _ = strconv.Atoi(u.Port())
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func (m *Manager) redeemWorkerEnrollment(ctx context.Context, enrollment workerEnrollmentURL, name string, l *Layout) (workerEnrollmentResponse, error) {
	if err := ctx.Err(); err != nil {
		return workerEnrollmentResponse{}, err
	}
	body, err := json.Marshal(protocol.RedeemWorkerEnrollmentRequest{Code: enrollment.code, Name: name, OS: l.System, Arch: l.Architecture})
	if err != nil {
		return workerEnrollmentResponse{}, errors.New(workerEnrollmentUncertain)
	}
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, enrollment.origin+workerEnrollmentRedeemPath, bytes.NewReader(body))
	if err != nil {
		return workerEnrollmentResponse{}, errors.New(workerEnrollmentUncertain)
	}
	request.GetBody = nil // No transport or redirect can replay this one-use request.
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	client := *m.HTTP
	client.Timeout = 30 * time.Second
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return workerEnrollmentResponse{}, errors.New("enrollment was interrupted and may have created a worker; check the gateway admin worker list before creating a fresh URL and rerunning the installer")
		}
		return workerEnrollmentResponse{}, errors.New(workerEnrollmentUncertain)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusConflict || response.StatusCode == http.StatusGone {
		return workerEnrollmentResponse{}, &workerEnrollmentRejected{}
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return workerEnrollmentResponse{}, errors.New(workerEnrollmentUncertain)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, workerEnrollmentBodyLimit+1))
	if err != nil || len(data) > workerEnrollmentBodyLimit || response.ContentLength > int64(len(data)) || !utf8.Valid(data) {
		return workerEnrollmentResponse{}, errors.New(workerEnrollmentUncertain)
	}
	var result workerEnrollmentResponse
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return workerEnrollmentResponse{}, errors.New(workerEnrollmentUncertain)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return workerEnrollmentResponse{}, errors.New(workerEnrollmentUncertain)
	}
	if err := validateWorkerEnrollmentResponse(result, enrollment.origin); err != nil {
		return workerEnrollmentResponse{}, err
	}
	return result, nil
}
