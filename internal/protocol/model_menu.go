package protocol

import (
	"errors"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const ModelMenuPageSize = 10

// ModelMenu holds only validated arguments to /model, never RPC methods.
// Each menu is tied to its requesting command, including its reasoning step.
type ModelMenu struct {
	Options []ModelOption `json:"options"`
}

type ModelOption struct {
	Args  string `json:"args"`
	Label string `json:"label"`
}

func ModelMenuToken(value string) bool {
	return value != "" && len(value) <= 256 && utf8.ValidString(value) &&
		!strings.HasPrefix(value, "--") && strings.IndexFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) < 0
}

func ValidModelMenuArgs(args string) bool {
	if !utf8.ValidString(args) || strings.IndexFunc(args, unicode.IsControl) >= 0 {
		return false
	}
	fields := strings.Fields(args)
	if len(fields) == 0 || strings.Join(fields, " ") != args || len(fields) > 2 {
		return false
	}
	switch fields[0] {
	case "--cancel":
		return len(fields) == 1
	case "--menu":
		return len(fields) == 1 || ModelMenuToken(fields[1])
	case "--page":
		if len(fields) != 2 {
			return false
		}
		page, err := strconv.Atoi(fields[1])
		return err == nil && page >= 0 && page <= 1000
	default:
		return ModelMenuToken(fields[0]) && (len(fields) == 1 || ModelMenuToken(fields[1]))
	}
}

func (m *ModelMenu) Validate() error {
	if m == nil || len(m.Options) == 0 || len(m.Options) > ModelMenuPageSize+3 {
		return errors.New("invalid model menu size")
	}
	seen := make(map[string]bool, len(m.Options))
	for _, option := range m.Options {
		if !ValidModelMenuArgs(option.Args) || strings.TrimSpace(option.Label) == "" ||
			!utf8.ValidString(option.Label) || utf8.RuneCountInString(option.Label) > 120 ||
			strings.IndexFunc(option.Label, unicode.IsControl) >= 0 || seen[option.Args] {
			return errors.New("invalid model menu option")
		}
		seen[option.Args] = true
	}
	return nil
}
