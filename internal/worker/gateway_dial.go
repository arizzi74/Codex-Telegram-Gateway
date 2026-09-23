package worker

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"
	"github.com/iaia/telegramgw/internal/config"
)

// dialGateway allows a new worker to be installed before its gateway during the
// /tgw route migration. Every connection first tries the current endpoint; only
// an absent endpoint on the same TLS origin permits trying the old path. A new
// gateway never needs to expose the legacy endpoint.
func (c *Connection) dialGateway(ctx context.Context, options *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
	conn, response, err := c.dial(ctx, c.cfg.GatewayURL, options)
	if err == nil || ctx.Err() != nil || response == nil || (response.StatusCode != http.StatusNotFound && response.StatusCode != http.StatusGone) {
		return conn, response, err
	}
	endpoint, parseErr := url.Parse(c.cfg.GatewayURL)
	if parseErr != nil || endpoint.Scheme != "wss" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.EscapedPath() != config.WorkerConnectPath || endpoint.RawQuery != "" || endpoint.ForceQuery || strings.Contains(c.cfg.GatewayURL, "#") {
		return conn, response, err
	}
	if response.Body != nil {
		response.Body.Close()
	}
	endpoint.Path, endpoint.RawPath = "/tgapi/v1/workers/connect", ""
	return c.dial(ctx, endpoint.String(), options)
}
