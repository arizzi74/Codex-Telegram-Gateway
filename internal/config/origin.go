package config

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// ParseHTTPSOrigin validates a configured browser origin and removes an
// optional root slash. Keep the hostname and explicit port together for exact
// browser-origin checks; callers deriving a WebAuthn RP ID use Hostname().
func ParseHTTPSOrigin(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || strings.Contains(value, "#") ||
		(u.Path != "" && u.Path != "/") || strings.HasSuffix(u.Host, ":") {
		return nil, errors.New("must be an HTTPS origin without credentials, path, query, or fragment")
	}
	if port := u.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return nil, errors.New("HTTPS origin port must be 1..65535")
		}
	}
	u.Path, u.RawPath = "", ""
	return u, nil
}
