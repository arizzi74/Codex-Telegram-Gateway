package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
)

const (
	telegramManagedBotUsernameCandidates = 1000
	telegramManagedBotUsernameChecks     = 20
	telegramManagedBotUsernameBudget     = 12 * time.Second
	telegramUsernameProfileBodyLimit     = 128 * 1024
)

var (
	ErrTelegramBotUsernameUnverified  = errors.New("Telegram username could not be checked")
	ErrTelegramBotUsernameUnavailable = errors.New("no unused Telegram username found within the attempt limit")
	telegramUsernameProfileHTTP       = &http.Client{Timeout: 3 * time.Second}
)

// telegramManagedBotUsername derives a Telegram-safe ASCII username, preserving
// each space or unsupported character as an underscore. A letter prefix is
// needed for names beginning with digits, underscores, or non-ASCII characters.
func telegramManagedBotUsername(displayName string, number int) string {
	var stem strings.Builder
	for _, r := range strings.ToLower(displayName) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			stem.WriteRune(r)
		} else {
			stem.WriteByte('_')
		}
	}
	base := stem.String()
	if base == "" || base[0] < 'a' || base[0] > 'z' {
		base = "bot_" + base
	}
	suffix := ""
	if number > 0 {
		suffix = strconv.Itoa(number)
	}
	if max := 32 - len("_bot") - len(suffix); len(base) > max {
		base = base[:max]
	}
	return base + suffix + "_bot"
}

// nextTelegramManagedBotUsername skips all local reservations before making a
// bounded public profile lookup. A missing public profile is provisional:
// Telegram's authoritative bots.checkUsername method requires a user MTProto
// session, and Telegram validates the suggestion again during owner confirmation.
func (m *TelegramBotManager) nextTelegramManagedBotUsername(ctx context.Context, displayName string, number, checks *int) (string, error) {
	for *number < telegramManagedBotUsernameCandidates {
		if err := ctx.Err(); err != nil {
			return "", ErrTelegramBotUsernameUnverified
		}
		username := telegramManagedBotUsername(displayName, *number)
		*number++
		reserved, err := m.store.TelegramManagedBotUsernameReserved(ctx, username)
		if err != nil {
			return "", errors.New("Telegram username reservations could not be read")
		}
		if reserved {
			continue
		}
		if *checks >= telegramManagedBotUsernameChecks {
			return "", ErrTelegramBotUsernameUnavailable
		}
		*checks++
		exists, err := m.usernameExists(ctx, username)
		if err != nil {
			return "", ErrTelegramBotUsernameUnverified
		}
		if !exists {
			return username, nil
		}
	}
	return "", ErrTelegramBotUsernameUnavailable
}

// telegramPublicUsernameExists sends no bot credentials and follows no
// redirects. Only Telegram's recognized generic contact page counts as an
// unresolved public profile; arbitrary HTML, rate limits and outages fail closed.
func telegramPublicUsernameExists(ctx context.Context, client *http.Client, username string) (bool, error) {
	if !validTelegramBotUsername(username) {
		return false, ErrTelegramBotInvalid
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://t.me/"+username, nil)
	if err != nil {
		return false, ErrTelegramBotUsernameUnverified
	}
	request.Header.Set("Accept", "text/html")
	request.Header.Set("Accept-Language", "en")
	lookup := *client
	lookup.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := lookup.Do(request)
	if err != nil {
		return false, ErrTelegramBotUsernameUnverified
	}
	defer response.Body.Close()
	contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusOK || err != nil || contentType != "text/html" {
		return false, ErrTelegramBotUsernameUnverified
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, telegramUsernameProfileBodyLimit+1))
	if err != nil || len(body) > telegramUsernameProfileBodyLimit || !utf8.Valid(body) {
		return false, ErrTelegramBotUsernameUnverified
	}
	return classifyTelegramUsernameProfile(body, username)
}

func classifyTelegramUsernameProfile(body []byte, username string) (bool, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return false, ErrTelegramBotUsernameUnverified
	}
	var title, ogTitle, siteName string
	var profileMarker, profile, resolve, genericUsername, genericAction bool
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			switch node.Data {
			case "title":
				title = telegramProfileText(node)
			case "meta":
				switch telegramProfileAttribute(node, "property") {
				case "og:title":
					ogTitle = telegramProfileAttribute(node, "content")
				case "og:site_name":
					siteName = telegramProfileAttribute(node, "content")
				}
			case "div":
				if telegramProfileClass(node, "tgme_page_title") {
					profileMarker = true
					profile = profile || telegramProfileText(node) != ""
				}
			case "a":
				link, parseErr := url.Parse(telegramProfileAttribute(node, "href"))
				if parseErr == nil && link.Scheme == "tg" && link.Host == "resolve" && link.Path == "" && link.RawQuery == "domain="+username && link.Fragment == "" {
					resolve = true
					genericUsername = genericUsername || (telegramProfileClass(node, "tgme_username_link") && telegramProfileText(node) == "@"+username)
					genericAction = genericAction || (telegramProfileClass(node, "tgme_action_button_new") && telegramProfileText(node) == "Send Message")
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	profileHeading := title == "Telegram: Launch @"+username || title == "Telegram: View @"+username || title == "Telegram: Contact @"+username
	if siteName == "Telegram" && profile && resolve && profileHeading && ogTitle != "" {
		return true, nil
	}
	if siteName == "Telegram" && !profileMarker && title == "Telegram: Contact @"+username && ogTitle == title && genericUsername && genericAction {
		return false, nil
	}
	return false, ErrTelegramBotUsernameUnverified
}

func telegramProfileAttribute(node *html.Node, key string) string {
	for _, attr := range node.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

func telegramProfileClass(node *html.Node, class string) bool {
	for _, token := range strings.Fields(telegramProfileAttribute(node, "class")) {
		if token == class {
			return true
		}
	}
	return false
}

func telegramProfileText(node *html.Node) string {
	var text strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			text.WriteString(n.Data)
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return strings.Join(strings.Fields(text.String()), " ")
}
