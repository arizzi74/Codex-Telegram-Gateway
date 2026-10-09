package worker

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter"
)

func statusCodexVersion(client *codexadapter.Client) string {
	// The handshake belongs to the process answering this status request. An
	// installed executable can change while that process keeps running.
	if info, ok := client.InitializeInfo(); ok {
		if version := codexUserAgentVersion(info.UserAgent); version != "" {
			return version
		}
	}
	return "unavailable"
}

var statusCodexVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

func codexUserAgentVersion(userAgent string) string {
	// App-server uses the integration's name as the originator in the first
	// product token, followed by its own build version. Later tokens can contain
	// terminal or integration versions and must not be mistaken for Codex.
	products := strings.Fields(userAgent)
	if len(products) == 0 {
		return ""
	}
	name, version, ok := strings.Cut(products[0], "/")
	if ok && name != "" && statusCodexVersionPattern.MatchString(version) {
		return version
	}
	return ""
}

// /usage retains its existing used-percentage display. Status describes the
// allowance left in each window, using durations rather than slot order to
// identify weekly limits.
func statusRateLimitLines(limits codexadapter.RateLimits) []string {
	var lines []string
	if limits.PlanType != "" {
		lines = append(lines, "Plan: "+limits.PlanType)
	}
	weekly := false
	for _, entry := range []struct {
		label  string
		window *codexadapter.RateLimitWindow
	}{{"Primary", limits.Primary}, {"Secondary", limits.Secondary}} {
		window := entry.window
		if window == nil {
			continue
		}
		label := entry.label
		if window.WindowDurationMins != nil {
			switch *window.WindowDurationMins {
			case 300:
				label = "5-hour"
			case 7 * 24 * 60:
				label, weekly = "Weekly", true
			}
		}
		remaining := 100 - min(100, max(0, window.UsedPercent))
		lines = append(lines, fmt.Sprintf("%s limit remaining: %d%%", label, remaining))
		reset := "unavailable"
		if window.ResetsAt != nil {
			seconds := *window.ResetsAt
			if seconds > 0 && seconds < time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC).Unix() {
				reset = usageTimestamp(seconds)
			}
		}
		lines = append(lines, label+" resets: "+reset)
	}
	if limits.Primary == nil && limits.Secondary == nil {
		lines = append(lines, "Rate limits: unavailable")
	}
	if !weekly {
		lines = append(lines, "Weekly limit: unavailable (not reported by Codex)")
	}
	return lines
}
