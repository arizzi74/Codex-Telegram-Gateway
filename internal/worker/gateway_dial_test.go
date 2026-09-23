package worker

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/coder/websocket"
	"github.com/iaia/telegramgw/internal/config"
)

func TestGatewayRouteFallbackOnlyForMissingEndpoint(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone, http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			const current = "wss://gateway.example.test:8443/tgw/api/v1/workers/connect"
			var urls []string
			options := &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer test-token"}}}
			c := &Connection{cfg: config.WorkerConfig{GatewayURL: current}}
			c.dial = func(_ context.Context, endpoint string, got *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
				urls = append(urls, endpoint)
				if got != options {
					t.Fatal("fallback lost authentication or TLS options")
				}
				return nil, &http.Response{StatusCode: status}, errors.New("upgrade rejected")
			}
			_, _, _ = c.dialGateway(context.Background(), options)
			want := []string{current}
			if status == http.StatusNotFound || status == http.StatusGone {
				want = append(want, "wss://gateway.example.test:8443/tgapi/v1/workers/connect")
			}
			if !reflect.DeepEqual(urls, want) {
				t.Fatalf("dialed %v, want %v", urls, want)
			}
		})
	}
}

func TestGatewayRouteFallbackDoesNotRewriteCustomOrInvalidEndpoints(t *testing.T) {
	for _, endpoint := range []string{
		"wss://gateway.example.test/custom/workers/connect",
		"wss://gateway.example.test/%74gw/api/v1/workers/connect",
		"wss://gateway.example.test/tgw/api/v1/workers/connect?secret=value",
		"wss://gateway.example.test/tgw/api/v1/workers/connect?",
		"wss://gateway.example.test/tgw/api/v1/workers/connect#",
		"wss://user:password@gateway.example.test/tgw/api/v1/workers/connect",
		"ws://gateway.example.test/tgw/api/v1/workers/connect",
	} {
		t.Run(endpoint, func(t *testing.T) {
			calls := 0
			c := &Connection{cfg: config.WorkerConfig{GatewayURL: endpoint}}
			c.dial = func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
				calls++
				return nil, &http.Response{StatusCode: http.StatusNotFound}, errors.New("missing")
			}
			_, _, _ = c.dialGateway(context.Background(), nil)
			if calls != 1 {
				t.Fatalf("custom or invalid endpoint fell back: %d calls", calls)
			}
		})
	}
}

func TestGatewayRouteFallbackPreservesCancellationAndConnectionErrors(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		ctx, stop := context.WithCancel(context.Background())
		calls := 0
		c := &Connection{cfg: config.WorkerConfig{GatewayURL: "wss://gateway.example.test/tgw/api/v1/workers/connect"}}
		c.dial = func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
			calls++
			if cancel {
				stop()
				return nil, &http.Response{StatusCode: http.StatusNotFound}, errors.New("missing")
			}
			return nil, nil, errors.New("TLS connection failed")
		}
		_, _, _ = c.dialGateway(ctx, nil)
		stop()
		if calls != 1 {
			t.Fatalf("failed connection retried: %d calls", calls)
		}
	}
}
