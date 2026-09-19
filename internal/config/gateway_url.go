package config

import (
	"net/url"
	"strings"
)

// NormalizeGatewayURL migrates the original gateway connection endpoint when
// reading an existing worker configuration. Custom endpoints, other protocols,
// and invalid URLs are left unchanged; validation remains the caller's job.
func NormalizeGatewayURL(value string) string {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "wss" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(value, "#") {
		return value
	}
	if u.EscapedPath() != "/api/v1/workers/connect" && u.EscapedPath() != "/api/v1/workers/connect/" {
		return value
	}
	u.Path = WorkerConnectPath
	u.RawPath = ""
	return u.String()
}
