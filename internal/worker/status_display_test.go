package worker

import (
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter"
)

func TestCodexUserAgentVersion(t *testing.T) {
	for _, test := range []struct {
		name, userAgent, want string
	}{
		{"production originator", "telegramgw/0.162.0 (Linux 6.8; x86_64) terminal/1.2 (telegramgw; dev)", "0.162.0"},
		{"custom originator", "custom-worker/0.163.0 (Mac OS 15; arm64)", "0.163.0"},
		{"CLI originator", "codex-cli/0.154.0", "0.154.0"},
		{"prerelease build", "codex_cli_rs/0.163.0-alpha.2+build.42", "0.163.0-alpha.2+build.42"},
		{"missing", "", ""},
		{"missing product name", "/0.162.0", ""},
		{"missing version", "telegramgw/ (telegramgw; 0.162.0)", ""},
		{"unreported build", "telegramgw/unknown codex-cli/0.162.0", ""},
		{"terminal version only", "telegramgw terminal/1.2.3", ""},
		{"incomplete version", "telegramgw/0.162", ""},
		{"invalid version", "telegramgw/0.162.x", ""},
		{"extra slash", "telegramgw/0.162.0/client", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := codexUserAgentVersion(test.userAgent); got != test.want {
				t.Fatalf("version = %q, want %q", got, test.want)
			}
		})
	}
}

func TestStatusRateLimitLinesClampRemainingAllowance(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, test := range []struct {
		name      string
		used      int
		remaining string
	}{
		{"unused", 0, "100%"},
		{"partly used", 17, "83%"},
		{"exhausted", 100, "0%"},
		{"negative", -10, "100%"},
		{"over limit", 110, "0%"},
		{"minimum integer", -maxInt - 1, "100%"},
		{"maximum integer", maxInt, "0%"},
	} {
		t.Run(test.name, func(t *testing.T) {
			text := strings.Join(statusRateLimitLines(codexadapter.RateLimits{
				Primary: &codexadapter.RateLimitWindow{UsedPercent: test.used},
			}), "\n")
			if !strings.Contains(text, "Primary limit remaining: "+test.remaining) {
				t.Fatalf("remaining allowance is wrong:\n%s", text)
			}
			if strings.Contains(text, "Weekly limit remaining:") || !strings.Contains(text, "Weekly limit: unavailable (not reported by Codex)") {
				t.Fatalf("unknown window was presented as weekly:\n%s", text)
			}
		})
	}
}

func TestStatusRateLimitLinesResetsUseUTC(t *testing.T) {
	weekly, fiveHours := int64(7*24*60), int64(300)
	reset := time.Date(2026, time.October, 12, 18, 45, 0, 0, time.FixedZone("CEST", 2*60*60)).Unix()
	text := strings.Join(statusRateLimitLines(codexadapter.RateLimits{
		PlanType:  "pro",
		Primary:   &codexadapter.RateLimitWindow{UsedPercent: 63, WindowDurationMins: &weekly, ResetsAt: &reset},
		Secondary: &codexadapter.RateLimitWindow{UsedPercent: 8, WindowDurationMins: &fiveHours},
	}), "\n")
	for _, want := range []string{"Plan: pro", "Weekly limit remaining: 37%", "Weekly resets: 2026-10-12 16:45 UTC", "5-hour limit remaining: 92%", "5-hour resets: unavailable"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Weekly limit: unavailable") {
		t.Fatalf("reported weekly window marked unavailable:\n%s", text)
	}
}

func TestStatusRateLimitLinesUnknownDurationKeepsSlotName(t *testing.T) {
	for _, duration := range []int64{-1, 0, 120, 24 * 60} {
		text := strings.Join(statusRateLimitLines(codexadapter.RateLimits{
			Secondary: &codexadapter.RateLimitWindow{UsedPercent: 20, WindowDurationMins: &duration},
		}), "\n")
		if !strings.Contains(text, "Secondary limit remaining: 80%") || !strings.Contains(text, "Secondary resets: unavailable") {
			t.Fatalf("duration %d lost its honest fallback label:\n%s", duration, text)
		}
		if strings.Contains(text, "Weekly limit remaining:") || strings.Contains(text, "5-hour limit remaining:") {
			t.Fatalf("duration %d was misidentified:\n%s", duration, text)
		}
	}
}

func TestStatusRateLimitLinesInvalidResetUnavailable(t *testing.T) {
	weekly := int64(7 * 24 * 60)
	for _, reset := range []int64{-1, 0, time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC).Unix(), int64(^uint64(0) >> 1)} {
		text := strings.Join(statusRateLimitLines(codexadapter.RateLimits{
			Primary: &codexadapter.RateLimitWindow{WindowDurationMins: &weekly, ResetsAt: &reset},
		}), "\n")
		if !strings.Contains(text, "Weekly resets: unavailable") {
			t.Fatalf("invalid reset %d shown as a date:\n%s", reset, text)
		}
	}
}
