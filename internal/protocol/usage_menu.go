package protocol

import (
	"errors"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// UsageMenu carries only arguments to /usage. Redemption arguments contain a
// short-lived worker-issued capability, never an account or reset-credit ID.
type UsageMenu struct {
	Options []UsageOption `json:"options"`
}

type UsageOption struct {
	Args        string `json:"args"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

func ValidUsageMenuArgs(args string) bool {
	switch args {
	case "--menu", "daily", "weekly", "cumulative", "resets", "redeem", "cancel":
		return true
	}
	fields := strings.Split(args, " ")
	if len(fields) == 2 && fields[0] == "resets" {
		page, err := strconv.Atoi(fields[1])
		return err == nil && page >= 0 && page <= 1000 && strconv.Itoa(page) == fields[1]
	}
	if len(fields) != 2 || (fields[0] != "redeem" && fields[0] != "confirm") {
		return false
	}
	id, err := uuid.Parse(fields[1])
	return err == nil && id != uuid.Nil && id.String() == fields[1]
}

func (m *UsageMenu) Validate() error {
	if m == nil || len(m.Options) == 0 || len(m.Options) > 50 {
		return errors.New("invalid usage menu size")
	}
	seen := make(map[string]bool, len(m.Options))
	for _, option := range m.Options {
		if !ValidUsageMenuArgs(option.Args) || strings.TrimSpace(option.Label) == "" ||
			!utf8.ValidString(option.Label) || utf8.RuneCountInString(option.Label) > 120 ||
			strings.IndexFunc(option.Label, unicode.IsControl) >= 0 || !utf8.ValidString(option.Description) ||
			utf8.RuneCountInString(option.Description) > 500 || strings.IndexFunc(option.Description, unicode.IsControl) >= 0 || seen[option.Args] {
			return errors.New("invalid usage menu option")
		}
		seen[option.Args] = true
	}
	return nil
}
