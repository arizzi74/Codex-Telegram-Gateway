package codexadapter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const usageResetTestKey = "8ae96ff3-3425-4f4c-8772-b6fd61502868"

func TestUsageRateLimitsPreserveResetAvailability(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		wantSummary   bool
		wantCount     int
		wantDetails   bool
		wantRows      int
	}{
		{"unsupported", `{}`, false, 0, false, 0},
		{"unavailable", `{"rateLimitResetCredits":null}`, false, 0, false, 0},
		{"count_only", `{"rateLimitResetCredits":{"availableCount":3,"credits":null}}`, true, 3, false, 0},
		{"omitted_details", `{"rateLimitResetCredits":{"availableCount":3}}`, true, 3, false, 0},
		{"empty_details", `{"rateLimitResetCredits":{"availableCount":0,"credits":[]}}`, true, 0, true, 0},
		{"capped_details", `{"rateLimitResetCredits":{"availableCount":3,"credits":[{"id":"credit-1","resetType":"codexRateLimits","status":"available","grantedAt":1781654400,"expiresAt":1784246400,"title":"Full reset","description":"Reset usage"}]}}`, true, 3, true, 1},
		{"nullable_fields", `{"rateLimitResetCredits":{"availableCount":1,"credits":[{"id":"credit-1","resetType":"unknown","status":"unknown","grantedAt":1781654400,"expiresAt":null,"title":null,"description":null}]}}`, true, 1, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, fake := newFake(t)
			initialize(t, client, fake)
			type response struct {
				limits RateLimits
				err    error
			}
			done := make(chan response, 1)
			go func() {
				limits, err := client.ReadUsageRateLimits(context.Background())
				done <- response{limits, err}
			}()
			request := fake.next(t)
			if method(t, request) != "account/rateLimits/read" || string(request["params"]) != `{"excludeResetCreditDetails":false}` {
				t.Fatalf("usage read = %v", request)
			}
			fake.respond(t, request, json.RawMessage(tc.payload))
			got := <-done
			if got.err != nil {
				t.Fatal(got.err)
			}
			credits := got.limits.ResetCredits
			if (credits != nil) != tc.wantSummary {
				t.Fatalf("summary = %+v", credits)
			}
			if credits != nil && (credits.AvailableCount != tc.wantCount || (credits.Credits != nil) != tc.wantDetails || len(credits.Credits) != tc.wantRows) {
				t.Fatalf("credits = %+v", credits)
			}
			if tc.name == "capped_details" {
				credit := credits.Credits[0]
				if credit.ID != "credit-1" || credit.ResetType != "codexRateLimits" || credit.Status != "available" || credit.GrantedAt != 1781654400 || credit.ExpiresAt == nil || *credit.ExpiresAt != 1784246400 || credit.Title != "Full reset" || credit.Description != "Reset usage" {
					t.Fatalf("credit = %+v", credit)
				}
			}
		})
	}
}

func TestRateLimitsBackgroundReadStillExcludesResetDetails(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	type response struct {
		limits RateLimits
		err    error
	}
	done := make(chan response, 1)
	go func() {
		limits, err := client.ReadRateLimits(context.Background())
		done <- response{limits, err}
	}()
	request := fake.next(t)
	if string(request["params"]) != `{"excludeResetCreditDetails":true}` {
		t.Fatalf("background read = %s", request["params"])
	}
	fake.respond(t, request, json.RawMessage(`{"ordinaryUsageAllowed":false,"rateLimits":{"planType":"pro","primary":{"usedPercent":100,"windowDurationMins":300,"resetsAt":1784246400}},"rateLimitResetCredits":{"availableCount":1,"credits":[]}}`))
	got := <-done
	if got.err != nil || got.limits.PlanType != "pro" || got.limits.Primary == nil || got.limits.Primary.UsedPercent != 100 || got.limits.ResetCredits != nil || got.limits.OrdinaryUsageAllowed == nil || *got.limits.OrdinaryUsageAllowed {
		t.Fatalf("background limits = %+v, %v", got.limits, got.err)
	}
}

func TestUsageRateLimitsAccountPinIsNotSerialized(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	type response struct {
		limits RateLimits
		err    error
	}
	done := make(chan response, 1)
	go func() {
		limits, err := client.ReadUsageRateLimits(context.Background())
		done <- response{limits, err}
	}()
	request := fake.next(t)
	fake.respond(t, request, json.RawMessage(`{"accountId":"private-account-pin","rateLimits":{},"accessToken":"secret","rateLimitResetCredits":{"availableCount":1}}`))
	got := <-done
	if got.err != nil || got.limits.AccountID != "private-account-pin" {
		t.Fatalf("account pin = %+v, %v", got.limits, got.err)
	}
	raw, err := json.Marshal(got.limits)
	if err != nil || strings.Contains(string(raw), "private-account-pin") || strings.Contains(string(raw), "accessToken") {
		t.Fatalf("private account metadata leaked: %s, %v", raw, err)
	}
}

func TestUsageDailyBucketsPreserveAvailability(t *testing.T) {
	for _, raw := range []string{`{}`, `{"dailyUsageBuckets":null}`, `{"dailyUsageBuckets":[]}`, `{"dailyUsageBuckets":[{"startDate":"2026-09-27","tokens":12345}]}`} {
		t.Run(raw, func(t *testing.T) {
			client, fake := newFake(t)
			initialize(t, client, fake)
			type response struct {
				usage TokenUsage
				err   error
			}
			done := make(chan response, 1)
			go func() {
				usage, err := client.ReadTokenUsage(context.Background(), "")
				done <- response{usage, err}
			}()
			request := fake.next(t)
			if method(t, request) != "account/usage/read" || string(request["params"]) != `{}` {
				t.Fatalf("usage read = %v", request)
			}
			fake.respond(t, request, json.RawMessage(raw))
			got := <-done
			if got.err != nil {
				t.Fatal(got.err)
			}
			buckets := got.usage.DailyUsageBuckets
			if strings.Contains(raw, "[") != (buckets != nil) {
				t.Fatalf("bucket availability = %+v", buckets)
			}
			if strings.Contains(raw, "12345") && (len(buckets) != 1 || buckets[0].StartDate != "2026-09-27" || buckets[0].Tokens != 12345) {
				t.Fatalf("bucket data = %+v", buckets)
			}
		})
	}
}

func TestResetConsumeUsesExactKeyAndOptionalCredit(t *testing.T) {
	for _, outcome := range []string{"reset", "alreadyRedeemed", "nothingToReset", "noCredit"} {
		for _, creditID := range []string{"", "opaque-credit-1"} {
			t.Run(outcome+"/"+creditID, func(t *testing.T) {
				client, fake := newFake(t)
				initialize(t, client, fake)
				type response struct {
					outcome string
					err     error
				}
				done := make(chan response, 1)
				go func() {
					outcome, err := client.ConsumeRateLimitResetCredit(context.Background(), usageResetTestKey, creditID)
					done <- response{outcome, err}
				}()
				request := fake.next(t)
				p := params(t, request)
				if method(t, request) != "account/rateLimitResetCredit/consume" || string(p["idempotencyKey"]) != `"`+usageResetTestKey+`"` {
					t.Fatalf("consume request = %v", request)
				}
				if creditID == "" && len(p) != 1 || creditID != "" && (len(p) != 2 || string(p["creditId"]) != `"opaque-credit-1"`) {
					t.Fatalf("consume params = %s", request["params"])
				}
				status := client.UpdateSafety()
				if len(status.Blockers) != 1 || status.Blockers[0].Method != "account/rateLimitResetCredit/consume" || updateReadOnlyMethod(status.Blockers[0].Method) {
					t.Fatalf("consume safety = %+v", status)
				}
				fake.respond(t, request, map[string]any{"outcome": outcome})
				got := <-done
				if got.err != nil || got.outcome != outcome || !client.UpdateQuiescent() {
					t.Fatalf("consume = %+v; safety = %+v", got, client.UpdateSafety())
				}
			})
		}
	}
}

func TestResetConsumeRejectsInvalidInputBeforeRPC(t *testing.T) {
	client, _ := newFake(t)
	for _, tc := range []struct{ key, credit string }{
		{"", ""}, {"not-uuid", ""}, {"00000000-0000-0000-0000-000000000000", ""},
		{usageResetTestKey, " "}, {usageResetTestKey, "credit\n"}, {usageResetTestKey, "credit\x00id"},
		{usageResetTestKey, "\xff"}, {usageResetTestKey, strings.Repeat("a", 4097)},
	} {
		if _, err := client.ConsumeRateLimitResetCredit(context.Background(), tc.key, tc.credit); err == nil || errors.Is(err, ErrNotInitialized) {
			t.Fatalf("invalid input was not rejected locally: %v", err)
		}
	}
	if _, err := client.ConsumeRateLimitResetCredit(context.Background(), usageResetTestKey, ""); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("consume before initialize = %v", err)
	}
}

func TestResetConsumeUnrecognizedOutcomeRemainsUnconfirmed(t *testing.T) {
	for _, raw := range []string{`{"outcome":"SECRET_UNKNOWN"}`, `{}`, `null`, `{"outcome":123}`, `{"outcome":"reset","outcome":"noCredit"}`} {
		t.Run(raw, func(t *testing.T) {
			client, fake := newFake(t)
			initialize(t, client, fake)
			done := make(chan error, 1)
			go func() {
				_, err := client.ConsumeRateLimitResetCredit(context.Background(), usageResetTestKey, "")
				done <- err
			}()
			request := fake.next(t)
			fake.respond(t, request, json.RawMessage(raw))
			err := <-done
			if err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("unsafe consume error = %v", err)
			}
			if safety := client.UpdateSafety(); safety.UnconfirmedRequests != 1 || client.UpdateQuiescent() {
				t.Fatalf("unknown outcome missing safety blocker: %+v", safety)
			}
		})
	}
}

func TestResetConsumeCanceledAfterSendRemainsUnconfirmed(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := client.ConsumeRateLimitResetCredit(ctx, usageResetTestKey, "")
		done <- err
	}()
	request := fake.next(t)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("consume cancellation = %v", err)
	}
	if safety := client.UpdateSafety(); safety.UnconfirmedRequests != 1 || client.UpdateQuiescent() {
		t.Fatalf("canceled mutation not retained: %+v", safety)
	}
	fake.respond(t, request, map[string]any{"outcome": "reset"})
	if client.UpdateQuiescent() {
		t.Fatal("unobserved late redemption success cleared the safety blocker")
	}
}

func TestResetConsumeOldRuntimeMethodUnavailable(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	done := make(chan error, 1)
	go func() {
		_, err := client.ConsumeRateLimitResetCredit(context.Background(), usageResetTestKey, "")
		done <- err
	}()
	request := fake.next(t)
	fake.write(t, map[string]any{"id": request["id"], "error": map[string]any{"code": -32601, "message": "method unavailable"}})
	if err := <-done; !errors.Is(err, ErrMethodUnavailable) {
		t.Fatalf("consume compatibility = %v", err)
	}
	if client.Supports("account/rateLimitResetCredit/consume") || !client.UpdateQuiescent() {
		t.Fatalf("unsupported consume state = %+v", client.UpdateSafety())
	}
}
