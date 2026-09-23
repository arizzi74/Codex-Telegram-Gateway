package config

import (
	"net/url"
	"strings"
)

// NormalizeGatewayURL migrates previous gateway connection endpoints when
// reading an existing worker configuration. Custom endpoints, other protocols,
// and invalid URLs are left unchanged; validation remains the caller's job.
func NormalizeGatewayURL(value string) string {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "wss" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(value, "#") {
		return value
	}
	switch u.EscapedPath() {
	case "/api/v1/workers/connect", "/api/v1/workers/connect/", "/tgapi/v1/workers/connect", "/tgapi/v1/workers/connect/":
	default:
		return value
	}
	u.Path = WorkerConnectPath
	u.RawPath = ""
	return u.String()
}
