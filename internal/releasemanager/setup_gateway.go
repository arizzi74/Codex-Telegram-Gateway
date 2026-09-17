package releasemanager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/iaia/telegramgw/internal/config"
)

const setupWebhookSecretEnv = "CODEX_GATEWAY_TELEGRAM_WEBHOOK_SECRET"

// SetupGateway adopts an installed gateway, uses a prepared private configuration,
// or collects the settings through the controlling terminal for curl | sudo sh.
func (m *Manager) SetupGateway(ctx context.Context) error {
	l, err := NewLayout("gateway")
	if err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("gateway setup requires sudo")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	return m.setupGateway(ctx, l, cwd, openWorkerSetupTerminal, m.execute)
}

func (m *Manager) setupGateway(ctx context.Context, l *Layout, cwd string, openPrompt func() (workerSetupPrompt, error), execute func(context.Context, options) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	version := os.Getenv("CODEX_TELEGRAMGW_BOOTSTRAP_RELEASE")
	if version != "" {
		if _, err := ParseVersion(version); err != nil {
			return errors.New("invalid bootstrap release version")
		}
	}
	if _, err := os.Lstat(l.Config); err == nil {
		// Existing gateway adoption performs its own ownership and path checks.
		return execute(ctx, options{Action: "adopt", Component: "gateway", AutoUpdate: true})
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, path := range []string{l.Environment, filepath.Join(filepath.Dir(l.Config), ".botsecrets"), l.Unit} {
		if _, err := os.Lstat(path); err == nil {
			return errors.New("an incomplete gateway installation exists; restore its gateway.json before running setup again")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	prepared := filepath.Join(cwd, "gateway.json")
	environment := filepath.Join(cwd, "secrets.env")
	preparedExists := false
	for _, path := range []string{prepared, environment, filepath.Join(cwd, ".botsecrets")} {
		if _, err := os.Lstat(path); err == nil {
			preparedExists = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if preparedExists {
		if err := validatePreparedGateway(prepared, environment); err != nil {
			return err
		}
		return m.installGuidedGateway(ctx, options{Action: "install", Component: "gateway", Config: prepared, Environment: environment, Version: version, AutoUpdate: true}, execute)
	}
	prompt, err := openPrompt()
	if err != nil {
		return errors.New("gateway setup needs a terminal or private gateway.json, .botsecrets, and secrets.env files in the current directory; run this command in an interactive terminal")
	}
	defer prompt.Close()
	fmt.Fprintln(m.Out, "Set up a gateway using your Telegram bot and public HTTPS address. Daily automatic updates will be enabled.")
	origin, err := prompt.Ask(ctx, "Public HTTPS address", "", false)
	if err != nil {
		return err
	}
	publicURL, err := config.ParseHTTPSOrigin(strings.TrimSpace(origin))
	if err != nil {
		return errors.New("public address must be an HTTPS origin without credentials, path, query, or fragment")
	}
	bot, err := prompt.Ask(ctx, "Telegram bot username", "", false)
	if err != nil {
		return err
	}
	bot = strings.TrimPrefix(strings.TrimSpace(bot), "@")
	if !regexp.MustCompile(`^[A-Za-z0-9_]+$`).MatchString(bot) {
		return errors.New("Telegram bot username must contain only letters, digits, and underscores")
	}
	owner, err := prompt.Ask(ctx, "Your Telegram user ID", "", false)
	if err != nil {
		return err
	}
	owner = strings.TrimSpace(owner)
	ownerID, err := strconv.ParseInt(owner, 10, 64)
	if err != nil || ownerID <= 0 || !regexp.MustCompile(`^[0-9]+$`).MatchString(owner) {
		return errors.New("Telegram user ID must be a positive number")
	}
	label, err := prompt.Ask(ctx, "Your display name", "owner", false)
	if err != nil {
		return err
	}
	if strings.ContainsFunc(label, unicode.IsControl) {
		return errors.New("display name must be a single line without control characters")
	}
	if label = strings.TrimSpace(label); label == "" {
		label = "owner"
	}
	portValue, err := prompt.Ask(ctx, "Local listen port", "8080", false)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(strings.TrimSpace(portValue))
	if err != nil || port < 1024 || port > 65535 {
		return errors.New("local listen port must be 1024..65535")
	}
	// Collect the token last, after the public settings have been validated.
	token, err := prompt.Ask(ctx, "Telegram bot token (hidden)", "", true)
	if err != nil {
		return err
	}
	if strings.ContainsFunc(token, unicode.IsControl) {
		return errors.New("Telegram bot token must be a single line without control characters")
	}
	token = strings.TrimSpace(token)
	if !regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`).MatchString(token) {
		return errors.New("Telegram bot token must have the numeric ID and secret separated by a colon")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return errors.New("could not generate a webhook secret")
	}
	stage, err := os.MkdirTemp("", "codex-telegramgw-gateway-setup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	botPath := filepath.Join(stage, ".botsecrets")
	botData := fmt.Sprintf("BOTNAME=%s\nBOTTOKEN=%s\nWLNAME=%s\nWLID=%d\n", bot, token, label, ownerID)
	if err := os.WriteFile(botPath, []byte(botData), 0600); err != nil {
		return err
	}
	environment = filepath.Join(stage, "secrets.env")
	if err := os.WriteFile(environment, []byte(setupWebhookSecretEnv+"="+hex.EncodeToString(random[:])+"\n"), 0600); err != nil {
		return err
	}
	root := l.DataRoot
	if root == "" {
		root = GatewayDataRoot
	}
	configuration := filepath.Join(stage, "gateway.json")
	if err := WriteJSON(configuration, map[string]any{
		"listen": fmt.Sprintf("127.0.0.1:%d", port), "public_base_url": publicURL.String(),
		"database_path": filepath.Join(root, "gateway.db"), "bot_secrets_file": botPath,
		"webhook_secret_env": setupWebhookSecretEnv, "allowed_user_ids": []int64{ownerID},
	}); err != nil {
		return err
	}
	if err := validatePreparedGateway(configuration, environment); err != nil {
		return errors.New("gateway setup configuration failed validation; check the gateway and Telegram settings")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return m.installGuidedGateway(ctx, options{Action: "install", Component: "gateway", Config: configuration, Environment: environment, Version: version, AutoUpdate: true}, execute)
}

func (m *Manager) installGuidedGateway(ctx context.Context, opts options, execute func(context.Context, options) error) error {
	cfg, err := config.LoadGateway(opts.Config)
	if err != nil {
		return errors.New("gateway setup configuration failed validation")
	}
	if err := execute(ctx, opts); err != nil {
		return err
	}
	fmt.Fprintf(m.Out, "Configure your HTTPS reverse proxy to %s, then finish Telegram webhook, menu, and admin setup:\nhttps://github.com/%s/blob/main/docs/installation.md#finish-gateway-setup\n", strconv.Quote(cfg.Listen), DefaultRepo)
	return nil
}

func validatePreparedGateway(path, environment string) error {
	for _, file := range []string{path, environment} {
		if !privateRegularSetupFile(file) {
			return errors.New("gateway.json and secrets.env must both be regular private files; set their permissions to 0600")
		}
	}
	cfg, err := config.LoadGateway(path)
	if err != nil || !privateRegularSetupFile(cfg.BotSecretsFile) {
		return errors.New("gateway.json is not a valid private gateway configuration; check its settings and bot secrets file")
	}
	data, err := os.ReadFile(environment)
	if err != nil {
		return errors.New("could not read the private gateway environment file")
	}
	valid := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(key) || strings.ContainsFunc(value, unicode.IsControl) {
			return errors.New("secrets.env must contain single-line KEY=value assignments")
		}
		if key != cfg.WebhookSecretEnv {
			continue
		}
		if len(value) >= 2 && (value[0] == '\'' || value[0] == '"') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		if valid || !regexp.MustCompile(`^[A-Za-z0-9_-]{32,256}$`).MatchString(value) {
			return errors.New("secrets.env must define the configured webhook secret once, using 32..256 letters, digits, underscores or hyphens")
		}
		valid = true
	}
	if !valid {
		return errors.New("secrets.env must define the webhook secret variable named in gateway.json")
	}
	return nil
}

func privateRegularSetupFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0077 == 0
}
