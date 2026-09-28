package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/codexadapter/codextest"
	"github.com/iaia/telegramgw/internal/protocol"
)

func usageFixture(t *testing.T) (*sessionActor, *codexadapter.Client, *codextest.Server) {
	t.Helper()
	a, runtime, server, cleanup := testAgent(t)
	t.Cleanup(cleanup)
	client, _, _ := a.manager.Client(runtime.ID)
	actor := &sessionActor{agent: a, runtime: runtime, session: protocol.Session{RuntimeID: runtime.ID, ThreadID: "usage-thread", CWD: runtime.DefaultCWD, Loaded: true, State: "running", ActiveTurnID: "existing-turn"}}
	setUsageLimits(t, server, "account-original", 2, []map[string]any{usageCredit("late", "Later reset", 48*time.Hour), usageCredit("early", "Sooner reset", 24*time.Hour)})
	return actor, client, server
}

func usageCredit(id, title string, expires time.Duration) map[string]any {
	return map[string]any{"id": id, "resetType": "codexRateLimits", "status": "available", "title": title, "description": "Resets account usage", "grantedAt": time.Now().Add(-time.Hour).Unix(), "expiresAt": time.Now().Add(expires).Unix()}
}

func setUsageLimits(t *testing.T, server *codextest.Server, account string, count int, credits any) {
	t.Helper()
	if err := server.SetMethodResult("account/rateLimits/read", map[string]any{"accountId": account, "rateLimits": map[string]any{"planType": "pro", "primary": map[string]any{"usedPercent": 25, "windowDurationMins": 300, "resetsAt": time.Now().Add(time.Hour).Unix()}}, "rateLimitResetCredits": map[string]any{"availableCount": count, "credits": credits}}); err != nil {
		t.Fatal(err)
	}
}

func usageOption(t *testing.T, result protocol.Result, prefix string) protocol.UsageOption {
	t.Helper()
	if result.UsageMenu != nil {
		if err := result.UsageMenu.Validate(); err != nil {
			t.Fatal(err)
		}
		for _, option := range result.UsageMenu.Options {
			if strings.HasPrefix(option.Args, prefix) {
				return option
			}
		}
	}
	t.Fatalf("missing %q in %#v", prefix, result)
	return protocol.UsageOption{}
}

func usageCommand(t *testing.T, actor *sessionActor, client *codexadapter.Client, args string) protocol.Result {
	t.Helper()
	result, err := actor.executeCodexCommand(t.Context(), client, "usage", args)
	if err != nil {
		t.Fatalf("usage %q: %v", args, err)
	}
	return result
}

func usageConfirmation(t *testing.T, actor *sessionActor, client *codexadapter.Client) string {
	t.Helper()
	resets := usageCommand(t, actor, client, "resets")
	selected := usageOption(t, resets, "redeem ")
	confirmation := usageCommand(t, actor, client, selected.Args)
	if !strings.Contains(confirmation.Text, "all sessions") || !strings.Contains(confirmation.Text, "cannot be undone") {
		t.Fatal(confirmation.Text)
	}
	return usageOption(t, confirmation, "confirm ").Args
}

func TestUsageMenusNeverRedeemUntilConfirmedAndDoNotDisturbActiveTurn(t *testing.T) {
	actor, client, server := usageFixture(t)
	before := actor.session
	main := usageCommand(t, actor, client, "")
	if !strings.Contains(main.Text, "Banked resets available: 2") || !strings.Contains(main.Text, "UTC") {
		t.Fatal(main.Text)
	}
	usageOption(t, main, "daily")
	resets := usageCommand(t, actor, client, "resets")
	if !strings.Contains(resets.Text, "1. Sooner reset") || !strings.Contains(resets.Text, "2. Later reset") || !strings.Contains(resets.Text, "Granted") {
		t.Fatal(resets.Text)
	}
	selected := usageOption(t, resets, "redeem ")
	// Knowing a list capability does not bypass the separate review step.
	if _, err := actor.executeCodexCommand(t.Context(), client, "usage", "confirm "+strings.TrimPrefix(selected.Args, "redeem ")); err == nil {
		t.Fatal("unreviewed reset redeemed")
	}
	confirmation := usageCommand(t, actor, client, selected.Args)
	confirm := usageOption(t, confirmation, "confirm ").Args
	usageCommand(t, actor, client, "cancel")
	if hasCall(server.Calls(), "account/rateLimitResetCredit/consume") {
		t.Fatal("browsing consumed reset")
	}
	if _, err := actor.executeCodexCommand(t.Context(), client, "usage", confirm); err == nil {
		t.Fatal("cancelled confirmation remained redeemable")
	}
	confirm = usageConfirmation(t, actor, client)
	if err := server.SetMethodResult("account/rateLimitResetCredit/consume", map[string]string{"outcome": "reset"}); err != nil {
		t.Fatal(err)
	}
	result := usageCommand(t, actor, client, confirm)
	if !strings.Contains(result.Text, "One banked reset was redeemed.") || result.UsageMenu != nil {
		t.Fatal(result)
	}
	usageCommand(t, actor, client, confirm)
	if countCall(server.Calls(), "account/rateLimitResetCredit/consume") != 1 {
		t.Fatal("duplicate confirmation consumed more than one reset")
	}
	for _, call := range server.Calls() {
		if call.Method == "account/rateLimitResetCredit/consume" {
			var params map[string]string
			if json.Unmarshal(call.Params, &params) != nil || params["creditId"] != "early" || uuid.Validate(params["idempotencyKey"]) != nil {
				t.Fatalf("wrong consume: %s", call.Params)
			}
		}
	}
	if actor.session != before || hasCall(server.Calls(), "turn/start") || hasCall(server.Calls(), "thread/resume") || codexCommandNeedsIdle("usage", confirm) {
		t.Fatal("account usage disturbed active turn")
	}
	encoded, _ := json.Marshal(resets)
	if strings.Contains(string(encoded), "account-original") || strings.Contains(string(encoded), `"creditId"`) {
		t.Fatalf("private reset data exposed: %s", encoded)
	}
}

func TestUsageResetConfirmationRevalidatesBeforeMutation(t *testing.T) {
	for _, test := range []string{"expired", "generation", "account", "removed", "count-zero", "invented", "restart"} {
		t.Run(test, func(t *testing.T) {
			actor, client, server := usageFixture(t)
			confirm := usageConfirmation(t, actor, client)
			token := strings.TrimPrefix(confirm, "confirm ")
			switch test {
			case "expired":
				actor.usageResets[token].expires = time.Now().Add(-time.Minute)
			case "generation":
				actor.runtime.Generation++
			case "account":
				setUsageLimits(t, server, "different-account", 1, []map[string]any{usageCredit("early", "Sooner reset", time.Hour)})
			case "removed":
				setUsageLimits(t, server, "account-original", 1, []map[string]any{usageCredit("late", "Later reset", time.Hour)})
			case "count-zero":
				setUsageLimits(t, server, "account-original", 0, []map[string]any{usageCredit("early", "Sooner reset", time.Hour)})
			case "invented":
				confirm = "confirm " + uuid.NewString()
			case "restart":
				actor.usageResets = nil
			}
			result, err := actor.executeCodexCommand(t.Context(), client, "usage", confirm)
			if err == nil && !strings.Contains(result.Text, "no longer available") {
				t.Fatalf("bad confirmation accepted: %#v", result)
			}
			if hasCall(server.Calls(), "account/rateLimitResetCredit/consume") {
				t.Fatal("stale reset consumed")
			}
		})
	}
}

func TestUsageResetOutcomesAndUnsupportedRuntime(t *testing.T) {
	for _, outcome := range []string{"reset", "alreadyRedeemed", "nothingToReset", "noCredit", "unknown", "unsupported", "rpc-error", "lost-response"} {
		t.Run(outcome, func(t *testing.T) {
			actor, client, server := usageFixture(t)
			confirm := usageConfirmation(t, actor, client)
			if err := server.SetMethodResult("account/rateLimitResetCredit/consume", map[string]string{"outcome": outcome}); err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			switch outcome {
			case "unsupported":
				server.SetMethodUnavailable("account/rateLimitResetCredit/consume", true)
			case "rpc-error":
				server.SetRPCError("account/rateLimitResetCredit/consume", -32000, "Request rejected")
			case "lost-response":
				server.SetResponseDelay("account/rateLimitResetCredit/consume", 120*time.Millisecond)
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
			}
			result, err := actor.executeCodexCommand(ctx, client, "usage", confirm)
			expectError := outcome == "unknown" || outcome == "unsupported" || outcome == "rpc-error" || outcome == "lost-response"
			if (err != nil) != expectError {
				t.Fatalf("outcome=%s result=%#v err=%v", outcome, result, err)
			}
			if outcome == "unsupported" && !errors.Is(err, codexadapter.ErrMethodUnavailable) {
				t.Fatal(err)
			}
			if !expectError && (result.Text == "" || !strings.Contains(result.Text, "Banked resets available:")) {
				t.Fatal(result)
			}
			before := countCall(server.Calls(), "account/rateLimitResetCredit/consume")
			_, _ = actor.executeCodexCommand(t.Context(), client, "usage", confirm)
			if before != 1 || countCall(server.Calls(), "account/rateLimitResetCredit/consume") != before {
				t.Fatal("confirmation replay retried a mutation")
			}
		})
	}
}

func TestUsageCountOnlyUnavailableAndPagedResets(t *testing.T) {
	actor, client, server := usageFixture(t)
	setUsageLimits(t, server, "", 3, nil)
	if _, err := actor.executeCodexCommand(t.Context(), client, "usage", "redeem"); err == nil || hasCall(server.Calls(), "account/rateLimitResetCredit/consume") {
		t.Fatal("unidentified account could confirm a generic reset")
	}
	setUsageLimits(t, server, "account-original", 3, nil)
	resets := usageCommand(t, actor, client, "resets")
	usageOption(t, resets, "redeem")
	if !strings.Contains(resets.Text, "Banked resets available: 3") || !strings.Contains(resets.Text, "details are unavailable") {
		t.Fatal(resets.Text)
	}
	confirm := usageOption(t, usageCommand(t, actor, client, "redeem"), "confirm ").Args
	if err := server.SetMethodResult("account/rateLimitResetCredit/consume", map[string]string{"outcome": "reset"}); err != nil {
		t.Fatal(err)
	}
	usageCommand(t, actor, client, confirm)
	for _, call := range server.Calls() {
		if call.Method == "account/rateLimitResetCredit/consume" && strings.Contains(string(call.Params), "creditId") {
			t.Fatal("count-only choice invented credit ID")
		}
	}
	if err := server.SetMethodResult("account/rateLimits/read", map[string]any{"rateLimits": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	unknown := usageCommand(t, actor, client, "resets")
	if !strings.Contains(unknown.Text, "unavailable") || len(unknown.UsageMenu.Options) != 2 {
		t.Fatal(unknown)
	}
	credits := make([]map[string]any, 19)
	for i := range credits {
		credits[i] = usageCredit(fmt.Sprint(i), "Reset", time.Duration(i+1)*time.Hour)
	}
	setUsageLimits(t, server, "account-original", 19, credits)
	seen := map[string]bool{}
	for page := 0; page < 3; page++ {
		result := usageCommand(t, actor, client, fmt.Sprintf("resets %d", page))
		if len(result.UsageMenu.Options) > 13 {
			t.Fatal("Telegram keyboard too large")
		}
		for _, option := range result.UsageMenu.Options {
			if strings.HasPrefix(option.Args, "redeem ") {
				id := actor.usageResets[strings.TrimPrefix(option.Args, "redeem ")].creditID
				if seen[id] {
					t.Fatal("duplicate reset across pages")
				}
				seen[id] = true
			}
		}
	}
	if len(seen) != 19 {
		t.Fatalf("only %d resets displayed", len(seen))
	}
}

func TestUsageActivityMatchesSundayWeeksAndCumulativeTotals(t *testing.T) {
	if got := usageActivity(codexadapter.TokenUsage{DailyUsageBuckets: []codexadapter.UsageBucket{}}, "daily"); !strings.Contains(got, "No token activity recorded") {
		t.Fatal(got)
	}
	if got := usageActivity(codexadapter.TokenUsage{}, "daily"); !strings.Contains(got, "not reported") {
		t.Fatal(got)
	}
	today, _ := time.Parse("2006-01-02", time.Now().UTC().Format("2006-01-02"))
	sunday := today.AddDate(0, 0, -int(today.Weekday())-7)
	usage := codexadapter.TokenUsage{DailyUsageBuckets: []codexadapter.UsageBucket{
		{StartDate: sunday.AddDate(0, 0, 1).Format("2006-01-02"), Tokens: 20},
		{StartDate: sunday.Format("2006-01-02"), Tokens: 10},
		{StartDate: sunday.Format("2006-01-02"), Tokens: 5},
		{StartDate: sunday.AddDate(0, 0, 7).Format("2006-01-02"), Tokens: 7},
		{StartDate: "invalid", Tokens: 99999}, {StartDate: "1999-01-01", Tokens: 99999},
	}}
	weekly := usageActivity(usage, "weekly")
	if !strings.Contains(weekly, "Week of "+sunday.Format("2006-01-02")+": 35 tokens") || strings.Contains(weekly, "99,999") {
		t.Fatal(weekly)
	}
	cumulative := usageActivity(usage, "cumulative")
	if !strings.Contains(cumulative, ": 42 tokens") {
		t.Fatal(cumulative)
	}
}
