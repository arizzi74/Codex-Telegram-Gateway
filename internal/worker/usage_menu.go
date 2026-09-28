package worker

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

// Actor-owned capabilities expire, belong to one runtime client, and cannot be
// reconstructed from a credit ID. A lost response never silently starts a new
// redemption. Repeated confirmation returns the first result without another
// RPC; the upstream key also protects against duplication in transit.
type usageResetChoice struct {
	client     *codexadapter.Client
	generation uint64
	expires    time.Time
	creditID   string
	accountID  string
	label      string
	key        string
	confirmed  bool
	submitted  bool
	result     *protocol.Result
}

func usageResult(text string, options ...protocol.UsageOption) (protocol.Result, error) {
	result := protocol.Result{Text: text}
	if len(options) > 0 {
		result.UsageMenu = &protocol.UsageMenu{Options: options}
		if err := result.UsageMenu.Validate(); err != nil {
			return protocol.Result{}, err
		}
	}
	return result, nil
}

func usageNavigation() []protocol.UsageOption {
	return []protocol.UsageOption{{Args: "--menu", Label: "Back to usage"}, {Args: "cancel", Label: "Cancel"}}
}

func (s *sessionActor) codexUsageMenu(ctx context.Context, client *codexadapter.Client, args string) (protocol.Result, error) {
	switch strings.ToLower(args) {
	case "day", "daily":
		args = "daily"
	case "week", "weekly":
		args = "weekly"
	case "cumulative":
		args = "cumulative"
	}
	if args == "" {
		args = "--menu"
	}
	if !protocol.ValidUsageMenuArgs(args) {
		return protocol.Result{}, validationError("Usage: /usage [daily|weekly|cumulative|resets|redeem]")
	}
	if args == "cancel" {
		for token, choice := range s.usageResets {
			if !choice.submitted {
				delete(s.usageResets, token)
			}
		}
		return usageResult("Usage menu closed. No reset was redeemed.")
	}
	if strings.HasPrefix(args, "confirm ") {
		return s.confirmUsageReset(ctx, client, strings.TrimPrefix(args, "confirm "))
	}
	if args == "daily" || args == "weekly" || args == "cumulative" {
		usage, err := client.ReadTokenUsage(ctx, s.session.ThreadID)
		if err != nil {
			return protocol.Result{}, err
		}
		return usageResult(usageActivity(usage, args), usageNavigation()...)
	}
	limits, err := client.ReadUsageRateLimits(ctx)
	if err != nil {
		return protocol.Result{}, err
	}
	if args == "redeem" || strings.HasPrefix(args, "redeem ") {
		var token string
		if args == "redeem" {
			if limits.ResetCredits == nil || limits.ResetCredits.AvailableCount <= 0 {
				return usageResult(usageLimitsText(limits), usageNavigation()...)
			}
			// Let Codex choose the next available reset when no credit was selected.
			token = s.offerUsageReset(client, limits.AccountID, "", "Next available reset")
		} else {
			token = strings.TrimPrefix(args, "redeem ")
		}
		choice, err := s.usageResetChoice(client, token)
		if err != nil {
			return protocol.Result{}, err
		}
		if choice.submitted {
			return protocol.Result{}, validationError("This reset was already submitted. Run /usage to check current limits.")
		}
		if choice.accountID != limits.AccountID {
			return protocol.Result{}, validationError("The Codex account changed. Run /usage again before redeeming a reset.")
		}
		if choice.creditID == "" && choice.accountID == "" {
			return protocol.Result{}, validationError("Codex did not report an account ID or individual reset details. A reset cannot be confirmed safely. Update Codex, then run /usage resets again.")
		}
		if !usageResetAvailable(limits, choice.creditID) {
			return usageResult("That banked reset is no longer available.\n\n"+usageLimitsText(limits), usageNavigation()...)
		}
		choice.confirmed = true
		return usageResult("Redeem one banked reset?\n\n"+choice.label+"\n"+usageResetScope(limits)+"\nThis uses a reset from the Codex account signed in on this worker and affects all sessions using that account. It cannot be undone.\n\n"+usageLimitsText(limits),
			protocol.UsageOption{Args: "confirm " + token, Label: "Redeem one reset"}, protocol.UsageOption{Args: "cancel", Label: "Cancel"})
	}
	if args == "resets" || strings.HasPrefix(args, "resets ") {
		page, _ := strconv.Atoi(strings.TrimPrefix(args, "resets "))
		return s.bankedResetMenu(client, limits, page)
	}
	return usageResult("Codex usage\n\n"+usageLimitsText(limits),
		protocol.UsageOption{Args: "daily", Label: "Daily token activity"},
		protocol.UsageOption{Args: "weekly", Label: "Weekly token activity"},
		protocol.UsageOption{Args: "cumulative", Label: "Cumulative token activity"},
		protocol.UsageOption{Args: "resets", Label: "View banked resets"},
		protocol.UsageOption{Args: "cancel", Label: "Cancel"})
}

func (s *sessionActor) bankedResetMenu(client *codexadapter.Client, limits codexadapter.RateLimits, page int) (protocol.Result, error) {
	text := usageLimitsText(limits)
	var options []protocol.UsageOption
	summary := limits.ResetCredits
	if summary != nil && summary.AvailableCount > 0 {
		credits := append([]codexadapter.RateLimitResetCredit(nil), summary.Credits...)
		sort.SliceStable(credits, func(i, j int) bool {
			return credits[i].ExpiresAt != nil && (credits[j].ExpiresAt == nil || *credits[i].ExpiresAt < *credits[j].ExpiresAt)
		})
		seen := make(map[string]bool)
		available := make([]codexadapter.RateLimitResetCredit, 0, len(credits))
		for _, credit := range credits {
			if len(available) >= summary.AvailableCount {
				break
			}
			if !availableUsageCredit(credit) || seen[credit.ID] {
				continue
			}
			seen[credit.ID] = true
			available = append(available, credit)
		}
		const pageSize = 8
		pages := max(1, (len(available)+pageSize-1)/pageSize)
		if page >= pages {
			return protocol.Result{}, validationError("This reset page is no longer available. Run /usage resets again.")
		}
		if pages > 1 {
			text += fmt.Sprintf("\nPage %d of %d", page+1, pages)
		}
		start := page * pageSize
		for index, credit := range available[start:min(start+pageSize, len(available))] {
			label := permissionText(s.agent.redactor.Redact(credit.Title), 90)
			if label == "" {
				label = "Full usage reset"
			}
			expiration := "Does not expire"
			if credit.ExpiresAt != nil {
				expiration = "Expires " + usageTimestamp(*credit.ExpiresAt)
			}
			description := permissionText(s.agent.redactor.Redact(credit.Description), 250)
			text += fmt.Sprintf("\n\n%d. %s\n%s", start+index+1, label, expiration)
			if credit.GrantedAt > 0 {
				text += "\nGranted " + usageTimestamp(credit.GrantedAt)
			}
			if description != "" {
				text += "\n" + description
			}
			token := s.offerUsageReset(client, limits.AccountID, credit.ID, label+" · "+expiration)
			options = append(options, protocol.UsageOption{Args: "redeem " + token, Label: fmt.Sprintf("%d. %s", start+index+1, label), Description: expiration})
		}
		if len(options) == 0 {
			text += "\n\nIndividual reset details are unavailable. Codex will select the next available reset."
		}
		if len(available) < summary.AvailableCount {
			options = append(options, protocol.UsageOption{Args: "redeem", Label: "Use next available reset", Description: "Codex chooses the reset; confirmation is required."})
		}
		if page > 0 {
			options = append(options, protocol.UsageOption{Args: fmt.Sprintf("resets %d", page-1), Label: "Previous resets"})
		}
		if page+1 < pages {
			options = append(options, protocol.UsageOption{Args: fmt.Sprintf("resets %d", page+1), Label: "Next resets"})
		}
		text += "\n\nChoose a reset to review before redeeming it."
	}
	return usageResult(text, append(options, usageNavigation()...)...)
}

func (s *sessionActor) offerUsageReset(client *codexadapter.Client, accountID, creditID, label string) string {
	for token, choice := range s.usageResets {
		if choice.client != client || choice.generation != s.runtime.Generation || time.Now().After(choice.expires) {
			delete(s.usageResets, token)
		}
	}
	if s.usageResets == nil {
		s.usageResets = make(map[string]*usageResetChoice)
	}
	// Bound state even if a client repeatedly opens new menus.
	if len(s.usageResets) >= 160 {
		var oldest string
		for token, choice := range s.usageResets {
			if oldest == "" || choice.expires.Before(s.usageResets[oldest].expires) {
				oldest = token
			}
		}
		delete(s.usageResets, oldest)
	}
	token := uuid.NewString()
	s.usageResets[token] = &usageResetChoice{client: client, generation: s.runtime.Generation, expires: time.Now().Add(10 * time.Minute), creditID: creditID, accountID: accountID, label: label, key: uuid.NewString()}
	return token
}

func (s *sessionActor) usageResetChoice(client *codexadapter.Client, token string) (*usageResetChoice, error) {
	choice := s.usageResets[token]
	if choice == nil || choice.client != client || choice.generation != s.runtime.Generation || time.Now().After(choice.expires) {
		return nil, validationError("This reset selection expired or the worker restarted. Run /usage again.")
	}
	return choice, nil
}

func (s *sessionActor) confirmUsageReset(ctx context.Context, client *codexadapter.Client, token string) (protocol.Result, error) {
	choice, err := s.usageResetChoice(client, token)
	if err != nil {
		return protocol.Result{}, err
	}
	if !choice.confirmed {
		return protocol.Result{}, validationError("Review and confirm this reset through /usage before redeeming it.")
	}
	if choice.result != nil {
		return *choice.result, nil
	}
	if choice.submitted {
		return protocol.Result{}, validationError("This redemption was submitted but its outcome was not confirmed. Run /usage to check current limits; it will not be submitted again automatically.")
	}
	limits, err := client.ReadUsageRateLimits(ctx)
	if err != nil {
		return protocol.Result{}, err
	}
	if choice.accountID != limits.AccountID {
		return protocol.Result{}, validationError("The Codex account changed. Run /usage again before redeeming a reset.")
	}
	if !usageResetAvailable(limits, choice.creditID) {
		result, _ := usageResult("That banked reset is no longer available.\n\n" + usageLimitsText(limits))
		choice.result = &result
		return result, nil
	}
	choice.submitted = true
	outcome, err := client.ConsumeRateLimitResetCredit(ctx, choice.key, choice.creditID)
	if err != nil {
		return protocol.Result{}, err
	}
	text := map[string]string{
		"reset":           "One banked reset was redeemed.",
		"alreadyRedeemed": "This reset was already redeemed. No additional reset was used.",
		"nothingToReset":  "There is no eligible usage window to reset. No reset was used.",
		"noCredit":        "No banked resets are available. No reset was used.",
	}[outcome]
	if fresh, refreshErr := client.ReadUsageRateLimits(ctx); refreshErr == nil {
		text += "\n\n" + usageLimitsText(fresh)
	} else {
		// A failed refresh must never hide a successful redemption or suggest
		// repeating it. /usage itself is a safe fresh read.
		text += "\nUpdated limits could not be loaded. Run /usage to refresh them."
	}
	result, _ := usageResult(text)
	choice.result = &result
	return result, nil
}

func availableUsageCredit(credit codexadapter.RateLimitResetCredit) bool {
	return credit.ID != "" && credit.Status == "available" && (credit.ExpiresAt == nil || *credit.ExpiresAt > time.Now().Unix())
}

func usageResetAvailable(limits codexadapter.RateLimits, creditID string) bool {
	if limits.ResetCredits == nil || limits.ResetCredits.AvailableCount <= 0 {
		return false
	}
	if creditID == "" {
		return true
	}
	for _, credit := range limits.ResetCredits.Credits {
		if credit.ID == creditID && availableUsageCredit(credit) {
			return true
		}
	}
	return false
}

func usageTimestamp(seconds int64) string {
	return time.Unix(seconds, 0).UTC().Format("2006-01-02 15:04 UTC")
}

func usageLimitsText(limits codexadapter.RateLimits) string {
	lines := rateLimitLines(limits)
	for _, entry := range []struct {
		label  string
		window *codexadapter.RateLimitWindow
	}{{"Primary", limits.Primary}, {"Secondary", limits.Secondary}} {
		if entry.window != nil && entry.window.ResetsAt != nil {
			lines = append(lines, entry.label+" resets: "+usageTimestamp(*entry.window.ResetsAt))
		}
	}
	if limits.ResetCredits == nil {
		lines = append(lines, "Banked resets: unavailable (not reported by this Codex runtime/account)")
	} else {
		lines = append(lines, fmt.Sprintf("Banked resets available: %d", max(0, limits.ResetCredits.AvailableCount)))
	}
	return strings.Join(lines, "\n")
}

func usageResetScope(limits codexadapter.RateLimits) string {
	for _, window := range []*codexadapter.RateLimitWindow{limits.Primary, limits.Secondary} {
		if window != nil && window.WindowDurationMins != nil && *window.WindowDurationMins >= 28*24*60 {
			return "Reset the eligible monthly usage limit."
		}
	}
	for _, window := range []*codexadapter.RateLimitWindow{limits.Primary, limits.Secondary} {
		if window != nil && window.WindowDurationMins != nil && (*window.WindowDurationMins == 300 || *window.WindowDurationMins == 7*24*60) {
			return "Reset the eligible 5-hour and weekly usage limits."
		}
	}
	return "Reset eligible Codex usage limits."
}

func usageActivity(usage codexadapter.TokenUsage, view string) string {
	lines := append([]string{"Codex token activity · " + view}, tokenUsageLines(usage)...)
	if usage.DailyUsageBuckets == nil {
		return strings.Join(append(lines, "Daily activity is not reported by this Codex runtime/account."), "\n")
	}
	// Sort and aggregate service-provided UTC days, never infer local sessions'
	// token usage as account-wide totals.
	buckets := make(map[string]int64)
	today, _ := time.Parse("2006-01-02", time.Now().UTC().Format("2006-01-02"))
	oldest := today.AddDate(0, 0, -int(today.Weekday())-51*7)
	for _, bucket := range usage.DailyUsageBuckets {
		day, err := time.Parse("2006-01-02", bucket.StartDate)
		if err != nil || day.Before(oldest) || day.After(today) {
			continue
		}
		if view == "weekly" || view == "cumulative" {
			day = day.AddDate(0, 0, -int(day.Weekday()))
		}
		buckets[day.Format("2006-01-02")] += max(0, bucket.Tokens)
	}
	keys := make([]string, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return strings.Join(append(lines, "No token activity recorded in the last 52 weeks."), "\n")
	}
	sort.Strings(keys)
	var cumulative int64
	rows := make([]string, 0, len(keys))
	for _, day := range keys {
		value := buckets[day]
		cumulative += value
		if view == "cumulative" {
			value = cumulative
		}
		prefix := day
		if view == "weekly" || view == "cumulative" {
			prefix = "Week of " + day
		}
		rows = append(rows, prefix+": "+formatInt(value)+" tokens")
	}
	if len(rows) > 31 {
		rows = rows[len(rows)-31:]
		lines = append(lines, "Showing the latest 31 reported periods.")
	}
	if view == "cumulative" {
		lines = append(lines, "Cumulative within the returned activity history (UTC):")
	} else {
		lines = append(lines, "Activity (UTC):")
	}
	return strings.Join(append(lines, rows...), "\n")
}
