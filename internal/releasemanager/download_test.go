package releasemanager

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

const downloadTestSource = "https://github.com/example/project/releases/download/v1.2.3/codex-worker-linux-amd64.tar.gz"

func downloadTestStatus(r *http.Request, code int) *http.Response {
	response := coreResponse(r, []byte("private response body"))
	response.StatusCode = code
	return response
}

func TestDownloadRetriesTransientHTTPFailures(t *testing.T) {
	for _, code := range []int{408, 500, 502, 503, 504} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := New(nil)
				var attempts []time.Time
				m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
					attempts = append(attempts, time.Now())
					if len(attempts) < 4 {
						return downloadTestStatus(r, code), nil
					}
					return coreResponse(r, []byte("verified payload")), nil
				})
				data, err := m.Download(t.Context(), downloadTestSource, 100)
				if err != nil || string(data) != "verified payload" || len(attempts) != 4 {
					t.Fatalf("download = %q, %v; attempts = %d", data, err, len(attempts))
				}
				for i, delay := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
					if got := attempts[i+1].Sub(attempts[i]); got != delay {
						t.Errorf("retry %d waited %s, want %s", i+1, got, delay)
					}
				}
			})
		})
	}
}

func TestDownloadStopsAfterBoundedRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var output bytes.Buffer
		m := New(&output)
		calls := 0
		m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
			calls++
			response := downloadTestStatus(r, http.StatusInternalServerError)
			response.Header.Set("X-GitHub-Request-Id", "A12B:34CD:56EF:7890:ABCD1234")
			return response, nil
		})
		started := time.Now()
		_, err := m.Download(t.Context(), downloadTestSource, 100)
		if err == nil || calls != 4 || time.Since(started) != 7*time.Second {
			t.Fatalf("download = %v; attempts = %d; elapsed = %s", err, calls, time.Since(started))
		}
		for _, fragment := range []string{"500", "github.com", "codex-worker-linux-amd64.tar.gz", "A12B:34CD:56EF:7890:ABCD1234"} {
			if !strings.Contains(err.Error(), fragment) {
				t.Errorf("final error is missing %q: %v", fragment, err)
			}
		}
		if output.Len() == 0 {
			t.Error("transient failures and retries were not logged")
		}
		if strings.Contains(output.String()+err.Error(), "private response body") {
			t.Error("response body appeared in download diagnostics")
		}
	})
}

func TestDownloadDoesNotRetryPermanentFailures(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 410, 422, 501} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := New(nil)
				calls := 0
				m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
					calls++
					return downloadTestStatus(r, code), nil
				})
				started := time.Now()
				_, err := m.Download(t.Context(), downloadTestSource, 100)
				if err == nil || calls != 1 || time.Since(started) != 0 {
					t.Fatalf("download = %v; attempts = %d; elapsed = %s", err, calls, time.Since(started))
				}
			})
		})
	}
}

func TestDownloadHonorsRateLimitGuidance(t *testing.T) {
	tests := []struct {
		name   string
		code   int
		header func(time.Time) http.Header
		delay  time.Duration
	}{
		{"retry-after-seconds", 429, func(time.Time) http.Header {
			return http.Header{"Retry-After": {"12"}}
		}, 12 * time.Second},
		{"retry-after-date", 503, func(now time.Time) http.Header {
			return http.Header{"Retry-After": {now.Add(17 * time.Second).UTC().Format(http.TimeFormat)}}
		}, 17 * time.Second},
		{"rate-limit-reset", 403, func(now time.Time) http.Header {
			return http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(now.Add(19*time.Second).Unix(), 10)}}
		}, 19 * time.Second},
		{"secondary-limit", 403, func(time.Time) http.Header {
			return http.Header{"Retry-After": {"11"}}
		}, 11 * time.Second},
		{"unguided-429", 429, func(time.Time) http.Header {
			return make(http.Header)
		}, time.Minute},
		{"unguided-primary-limit", 403, func(time.Time) http.Header {
			return http.Header{"X-Ratelimit-Remaining": {"0"}}
		}, time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := New(nil)
				calls := 0
				started := time.Now()
				m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
					calls++
					if calls == 1 {
						response := downloadTestStatus(r, tc.code)
						response.Header = tc.header(started)
						return response, nil
					}
					return coreResponse(r, []byte("complete")), nil
				})
				data, err := m.Download(t.Context(), downloadTestSource, 100)
				if err != nil || string(data) != "complete" || calls != 2 {
					t.Fatalf("download = %q, %v; attempts = %d", data, err, calls)
				}
				if elapsed := time.Since(started); elapsed < tc.delay || elapsed >= 5*time.Minute {
					t.Errorf("waited %s; server requires at least %s within the download budget", elapsed, tc.delay)
				}
			})
		})
	}
}

func TestDownloadDoesNotRetryBeforeAnUnwaitableServerDeadline(t *testing.T) {
	for _, guidance := range []string{"seconds", "overflow-seconds", "date", "reset", "overflow-reset"} {
		t.Run(guidance, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := New(nil)
				calls := 0
				started := time.Now()
				m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
					calls++
					response := downloadTestStatus(r, 429)
					switch guidance {
					case "seconds":
						response.Header.Set("Retry-After", "3600")
					case "overflow-seconds":
						response.Header.Set("Retry-After", "18446744073709551616000")
					case "date":
						response.Header.Set("Retry-After", started.Add(time.Hour).UTC().Format(http.TimeFormat))
					case "reset":
						response.StatusCode = 403
						response.Header.Set("X-RateLimit-Remaining", "0")
						response.Header.Set("X-RateLimit-Reset", strconv.FormatInt(started.Add(time.Hour).Unix(), 10))
					case "overflow-reset":
						response.StatusCode = 403
						response.Header.Set("X-RateLimit-Remaining", "0")
						response.Header.Set("X-RateLimit-Reset", "18446744073709551616000")
					}
					return response, nil
				})
				_, err := m.Download(t.Context(), downloadTestSource, 100)
				if err == nil || calls != 1 || time.Since(started) > 5*time.Minute {
					t.Fatalf("download = %v; attempts = %d; elapsed = %s", err, calls, time.Since(started))
				}
			})
		})
	}
}

func TestDownloadCancellationInterruptsRetryWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := New(nil)
		calls := 0
		m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
			calls++
			return downloadTestStatus(r, 503), nil
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() {
			time.Sleep(500 * time.Millisecond)
			cancel()
		}()
		started := time.Now()
		_, err := m.Download(ctx, downloadTestSource, 100)
		if !errors.Is(err, context.Canceled) || calls != 1 || time.Since(started) != 500*time.Millisecond {
			t.Fatalf("download = %v; attempts = %d; elapsed = %s", err, calls, time.Since(started))
		}
	})
}

func TestDownloadAlreadyCanceledDoesNotContactGitHub(t *testing.T) {
	m := New(nil)
	called := false
	m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
		called = true
		return coreResponse(r, nil), nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := m.Download(ctx, downloadTestSource, 100)
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("download = %v; transport called = %v", err, called)
	}
}

func TestDownloadBoundsTotalRequestTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := New(nil)
		m.HTTP.Timeout = 0 // The operation budget must not rely on a caller's client timeout.
		calls := 0
		m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
			calls++
			<-r.Context().Done()
			return nil, r.Context().Err()
		})
		started := time.Now()
		_, err := m.Download(t.Context(), downloadTestSource, 100)
		if err == nil || calls != 1 || time.Since(started) > 5*time.Minute {
			t.Fatalf("download = %v; attempts = %d; elapsed = %s", err, calls, time.Since(started))
		}
	})
}

func TestDownloadRetriesTransientTransportErrorsWithoutLeakingThem(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"timeout", &net.DNSError{Err: "PRIVATE_TRANSPORT_DETAIL", Name: "private.example", IsTimeout: true}},
		{"connection-reset", fmt.Errorf("PRIVATE_TRANSPORT_DETAIL: %w", syscall.ECONNRESET)},
		{"connection-eof", io.EOF},
		{"connection-truncated", io.ErrUnexpectedEOF},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var output bytes.Buffer
				m := New(&output)
				calls := 0
				m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
					calls++
					if calls == 1 {
						return nil, tc.err
					}
					return coreResponse(r, []byte("complete")), nil
				})
				data, err := m.Download(t.Context(), downloadTestSource, 100)
				if err != nil || calls != 2 || string(data) != "complete" {
					t.Fatalf("download = %q, %v; attempts = %d", data, err, calls)
				}
				if strings.Contains(output.String(), "PRIVATE_TRANSPORT_DETAIL") || strings.Contains(output.String(), "private.example") {
					t.Fatalf("raw transport error was logged: %q", output.String())
				}
			})
		})
	}
}

type downloadTestBrokenBody struct {
	closed bool
	read   bool
}

func (b *downloadTestBrokenBody) Read(p []byte) (int, error) {
	if b.read {
		return 0, io.ErrUnexpectedEOF
	}
	b.read = true
	return copy(p, "partial"), nil
}

func (b *downloadTestBrokenBody) Close() error { b.closed = true; return nil }

func TestDownloadRetriesDiscardPartialBodyAndCloseIt(t *testing.T) {
	for _, truncation := range []string{"read-error", "content-length"} {
		t.Run(truncation, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := New(nil)
				calls := 0
				body := &downloadTestBrokenBody{}
				m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
					calls++
					if calls == 1 {
						response := coreResponse(r, []byte("partial"))
						response.ContentLength = 100
						if truncation == "read-error" {
							response.Body = body
						}
						return response, nil
					}
					if truncation == "read-error" && !body.closed {
						t.Error("failed response body was not closed before retry")
					}
					return coreResponse(r, []byte("complete")), nil
				})
				data, err := m.Download(t.Context(), downloadTestSource, 100)
				if err != nil || calls != 2 || string(data) != "complete" {
					t.Fatalf("download = %q, %v; attempts = %d", data, err, calls)
				}
			})
		})
	}
}

func TestDownloadDoesNotRetryTLSOrUnknownTransportErrors(t *testing.T) {
	for _, transportErr := range []error{
		x509.HostnameError{Certificate: &x509.Certificate{}, Host: "PRIVATE_TRANSPORT_DETAIL"},
		x509.UnknownAuthorityError{},
		errors.New("PRIVATE_TRANSPORT_DETAIL"),
	} {
		synctest.Test(t, func(t *testing.T) {
			var output bytes.Buffer
			m := New(&output)
			calls := 0
			m.HTTP.Transport = coreRoundTripper(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, transportErr
			})
			_, err := m.Download(t.Context(), downloadTestSource, 100)
			if err == nil || calls != 1 {
				t.Fatalf("download = %v; attempts = %d", err, calls)
			}
			if strings.Contains(err.Error()+output.String(), "PRIVATE_TRANSPORT_DETAIL") {
				t.Error("raw transport error leaked")
			}
		})
	}
}

func TestDownloadRedirectPolicyFailuresAreNotRetried(t *testing.T) {
	for _, target := range []string{"https://private.example/SIGNED_PRIVATE_PATH?token=SECRET_QUERY", "http://github.com/SIGNED_PRIVATE_PATH?token=SECRET_QUERY"} {
		synctest.Test(t, func(t *testing.T) {
			var output bytes.Buffer
			m := New(&output)
			calls := 0
			m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				response := downloadTestStatus(r, 302)
				response.Header.Set("Location", target)
				return response, nil
			})
			_, err := m.Download(t.Context(), downloadTestSource, 100)
			if err == nil || calls != 1 {
				t.Fatalf("download = %v; attempts = %d", err, calls)
			}
			for _, private := range []string{"private.example", "SIGNED_PRIVATE_PATH", "SECRET_QUERY"} {
				if strings.Contains(err.Error()+output.String(), private) {
					t.Errorf("rejected redirect leaked %q", private)
				}
			}
		})
	}
}

func TestDownloadDiagnosticsKeepPublicAssetAndCDNHost(t *testing.T) {
	for _, hostileID := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			var output bytes.Buffer
			m := New(&output)
			calls := 0
			m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Hostname() == "github.com" {
					response := downloadTestStatus(r, 302)
					response.Header.Set("Location", "https://release-assets.githubusercontent.com/SIGNED_PRIVATE_PATH?token=SECRET_QUERY")
					return response, nil
				}
				response := downloadTestStatus(r, 404)
				requestID := "A12B:34CD:56EF:7890:ABCD1234"
				if hostileID {
					requestID = "safe\r\nhttps://private.example/HEADER_PRIVATE_PATH?token=HEADER_PRIVATE_QUERY\x1b[31m"
				}
				response.Header.Set("X-GitHub-Request-Id", requestID)
				return response, nil
			})
			_, err := m.Download(t.Context(), downloadTestSource+"?token=SOURCE_PRIVATE_QUERY", 100)
			if err == nil || calls != 2 {
				t.Fatalf("download = %v; requests = %d", err, calls)
			}
			for _, public := range []string{"codex-worker-linux-amd64.tar.gz", "release-assets.githubusercontent.com", "404"} {
				if !strings.Contains(err.Error(), public) {
					t.Errorf("error is missing diagnostic %q: %v", public, err)
				}
			}
			if !hostileID && !strings.Contains(err.Error(), "A12B:34CD:56EF:7890:ABCD1234") {
				t.Errorf("valid GitHub request ID was lost: %v", err)
			}
			for _, private := range []string{"SIGNED_PRIVATE_PATH", "SECRET_QUERY", "SOURCE_PRIVATE_QUERY", "HEADER_PRIVATE_PATH", "HEADER_PRIVATE_QUERY", "private.example", "private response body", "\r", "\x1b"} {
				if strings.Contains(err.Error()+output.String(), private) {
					t.Errorf("download diagnostics leaked %q", private)
				}
			}
		})
	}
}

func TestDownloadRetriesFromPublicURLToRefreshSignedRedirect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := New(nil)
		publicCalls, cdnCalls := 0, 0
		m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
			if r.URL.Hostname() == "github.com" {
				publicCalls++
				response := downloadTestStatus(r, 302)
				response.Header.Set("Location", "https://release-assets.githubusercontent.com/asset?signature="+strconv.Itoa(publicCalls))
				return response, nil
			}
			cdnCalls++
			if cdnCalls == 1 {
				return downloadTestStatus(r, 500), nil
			}
			if got := r.URL.Query().Get("signature"); got != "2" {
				t.Errorf("retried an old signed redirect: signature %q", got)
			}
			return coreResponse(r, []byte("complete")), nil
		})
		data, err := m.Download(t.Context(), downloadTestSource, 100)
		if err != nil || string(data) != "complete" || publicCalls != 2 || cdnCalls != 2 {
			t.Fatalf("download = %q, %v; public requests = %d; CDN requests = %d", data, err, publicCalls, cdnCalls)
		}
	})
}

func TestReleaseDownloadDiagnosticsIdentifyMetadataAndChecksums(t *testing.T) {
	for _, stage := range []string{"metadata", "SHA256SUMS"} {
		t.Run(stage, func(t *testing.T) {
			m := New(nil)
			calls := 0
			m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if stage == "SHA256SUMS" && r.URL.Hostname() == "api.github.com" {
					data, err := json.Marshal(map[string]any{
						"tag_name": "v1.2.3", "draft": false, "prerelease": false,
						"assets": []map[string]string{{"name": "SHA256SUMS", "browser_download_url": "https://github.com/example/project/releases/download/v1.2.3/SHA256SUMS"}},
					})
					if err != nil {
						t.Fatal(err)
					}
					return coreResponse(r, data), nil
				}
				return downloadTestStatus(r, 404), nil
			})
			_, err := m.FetchRelease(t.Context(), "example/project", "v1.2.3")
			if err == nil || !strings.Contains(err.Error(), stage) {
				t.Fatalf("error does not identify failed %s request: %v", stage, err)
			}
			wantCalls := 1
			if stage == "SHA256SUMS" {
				wantCalls = 2
			}
			if calls != wantCalls {
				t.Errorf("made %d requests, want %d", calls, wantCalls)
			}
		})
	}
}

func TestDownloadOversizedPayloadAndPackageChecksumFailWithoutRetry(t *testing.T) {
	m := New(nil)
	calls := 0
	m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		return coreResponse(r, []byte("payload")), nil
	})
	if _, err := m.Download(t.Context(), downloadTestSource, 3); err == nil || calls != 1 {
		t.Fatalf("oversized download = %v; attempts = %d", err, calls)
	}
	calls = 0
	commands := 0
	m.Run = func(context.Context, ...string) (CommandResult, error) {
		commands++
		return CommandResult{}, nil
	}
	name := "codex-worker-linux-amd64.tar.gz"
	release := &Release{
		Tag:    "v1.2.3",
		Assets: map[string]string{name: downloadTestSource},
		Hashes: map[string]string{name: coreHash([]byte("expected payload"))},
	}
	_, err := m.Package(t.Context(), release, &Layout{System: "linux", Architecture: "amd64"}, "codex-worker", t.TempDir())
	if err == nil || calls != 1 || commands != 0 {
		t.Fatalf("unverified package = %v; attempts = %d; program executions = %d", err, calls, commands)
	}
}
