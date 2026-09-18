package releasemanager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const downloadAttempts = 4
const downloadTimeout = 5 * time.Minute

var downloadAssetRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
var githubRequestIDRE = regexp.MustCompile(`^[A-Za-z0-9:-]{1,128}$`)

// Only the original public asset name is useful in logs. Redirect paths and
// queries can contain signed credentials, and transport errors can include URLs.
func downloadDescription(source *url.URL) string {
	if source.Hostname() == "api.github.com" {
		return "release metadata"
	}
	name := path.Base(source.Path)
	if name == "SHA256SUMS" {
		return "release checksums (SHA256SUMS)"
	}
	if downloadAssetRE.MatchString(name) {
		return "release asset " + name
	}
	return "release asset"
}

func downloadDiagnostic(description, host string, attempt, status int, requestID, reason string) string {
	detail := fmt.Sprintf("GitHub %s download failed (host=%s; attempt %d/%d", description, host, attempt, downloadAttempts)
	if status != 0 {
		detail += fmt.Sprintf("; HTTP %d", status)
	}
	if githubRequestIDRE.MatchString(requestID) {
		detail += "; request_id=" + requestID
	}
	return detail + "): " + reason
}

func transientDownloadError(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	var network net.Error
	return errors.As(err, &network) && (network.Timeout() || network.Temporary())
}

// Clamp unreasonably large server delays above our entire budget, rather than
// overflowing or retrying earlier than the server permits.
func retryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds > uint64(downloadTimeout/time.Second) {
			return downloadTimeout + time.Second, true
		}
		return time.Duration(seconds) * time.Second, true
	}
	date, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return max(0, date.Sub(now)), true
}

func downloadRetry(response *http.Response, attempt int, now time.Time) (bool, time.Duration) {
	delay := time.Second << (attempt - 1)
	if response == nil {
		return true, delay
	}
	header := response.Header
	serverDelay, guided := retryAfter(header.Get("Retry-After"), now)
	exhausted := strings.TrimSpace(header.Get("X-RateLimit-Remaining")) == "0"
	rateLimited := response.StatusCode == http.StatusTooManyRequests ||
		(response.StatusCode == http.StatusForbidden && (exhausted || guided))
	switch response.StatusCode {
	case http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
	default:
		if !rateLimited {
			return false, 0
		}
	}
	if exhausted {
		reset, err := strconv.ParseInt(strings.TrimSpace(header.Get("X-RateLimit-Reset")), 10, 64)
		if (err == nil || errors.Is(err, strconv.ErrRange)) && reset >= 0 {
			// Avoid constructing a time outside time.Time's representable range.
			resetDelay := downloadTimeout + time.Second
			if reset <= now.Unix()+int64(downloadTimeout/time.Second) {
				resetDelay = max(0, time.Unix(reset, 0).Sub(now))
			}
			serverDelay, guided = max(serverDelay, resetDelay), true
		}
	}
	if rateLimited && !guided {
		// GitHub asks clients without rate-limit timing headers to wait at
		// least a minute, increasing the delay after repeated failures.
		delay = time.Minute << (attempt - 1)
	}
	return true, max(delay, serverDelay)
}

func (m *Manager) Download(ctx context.Context, source string, limit int64) ([]byte, error) {
	if err := githubURL(source, false); err != nil {
		return nil, err
	}
	if limit < 0 || limit == math.MaxInt64 {
		return nil, errors.New("invalid release download size limit")
	}
	address, _ := url.Parse(source) // Validated by githubURL above.
	description := downloadDescription(address)
	// The budget includes every request, body read, and backoff, so repeated
	// failures cannot multiply the previous five-minute request timeout.
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	for attempt := 1; ; attempt++ {
		host := address.Hostname()
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%s: %w", downloadDiagnostic(description, host, attempt, 0, "", "download cancelled or timed out"), err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			return nil, errors.New("could not construct the release download request")
		}
		req.Header.Set("User-Agent", "codex-telegramgw-release-manager")
		if host == "api.github.com" {
			req.Header.Set("Accept", "application/vnd.github+json")
		} else {
			req.Header.Set("Accept", "application/octet-stream")
		}
		// Start every retry from the public URL, obtaining a fresh signed URL.
		// Retain the host policy even with a caller-supplied HTTP transport.
		client := *m.HTTP
		var redirectErr error
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				redirectErr = errors.New("too many release download redirects")
			} else {
				redirectErr = githubURL(req.URL.String(), true)
			}
			if redirectErr == nil {
				host = req.URL.Hostname()
			}
			return redirectErr
		}
		response, requestErr := client.Do(req)
		status, requestID := 0, ""
		reason := "request failed"
		retry, delay := false, time.Duration(0)
		if response != nil {
			status, requestID = response.StatusCode, response.Header.Get("X-GitHub-Request-Id")
		}
		switch {
		case redirectErr != nil:
			reason = redirectErr.Error() // Fixed policy messages, never a URL.
		case requestErr != nil:
			reason = "network request failed"
			retry = transientDownloadError(requestErr)
			_, delay = downloadRetry(nil, attempt, time.Now())
		case response.StatusCode != http.StatusOK:
			reason = "server returned an unsuccessful response"
			retry, delay = downloadRetry(response, attempt, time.Now())
		default:
			data, readErr := io.ReadAll(io.LimitReader(response.Body, limit+1))
			if readErr == nil && response.ContentLength > int64(len(data)) {
				readErr = io.ErrUnexpectedEOF
			}
			if int64(len(data)) > limit {
				reason = "release download exceeded its size limit"
			} else if readErr != nil {
				reason = "could not read the release download"
				retry = transientDownloadError(readErr)
				_, delay = downloadRetry(nil, attempt, time.Now())
			} else {
				response.Body.Close()
				return data, nil
			}
		}
		// Client.Do already closes response bodies when rejecting redirects.
		if requestErr == nil && response != nil && response.Body != nil {
			response.Body.Close()
		}
		diagnostic := downloadDiagnostic(description, host, attempt, status, requestID, reason)
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%s: %w", diagnostic, err)
		}
		if !retry || attempt == downloadAttempts {
			return nil, errors.New(diagnostic)
		}
		if deadline, ok := ctx.Deadline(); ok && delay >= time.Until(deadline) {
			return nil, fmt.Errorf("%s; retry delay exceeds the remaining download time limit", diagnostic)
		}
		if m.Out != nil {
			fmt.Fprintf(m.Out, "%s; retrying in %s\n", diagnostic, delay)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("%s: %w", diagnostic, ctx.Err())
		case <-timer.C:
		}
	}
}
