// Package config loads the gateway and worker configuration files.
//
// Configuration is JSON deliberately: encoding/json is in the Go standard
// library, the format has unambiguous primitive types, and it keeps the first
// deployment dependency-free. Secrets remain in environment variables or
// permission-restricted files and are never stored in these JSON files.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
)

const (
	DefaultGatewayListen     = "127.0.0.1:8080"
	DefaultPublicBaseURL     = "https://gateway.example.com"
	DefaultBotSecretsFile    = ".botsecrets"
	DefaultHeartbeatInterval = 10 * time.Second
	DefaultUnreachableAfter  = 30 * time.Second
	DefaultCommandExpiry     = time.Hour
	DefaultReadHeaderTimeout = 5 * time.Second
)

// GatewayConfig is the JSON configuration for codex-gateway.
type GatewayConfig struct {
	Listen            string        `json:"listen"`
	PublicBaseURL     string        `json:"public_base_url"`
	DatabasePath      string        `json:"database_path"`
	BotSecretsFile    string        `json:"bot_secrets_file"`
	WebhookSecretEnv  string        `json:"webhook_secret_env"`
	AllowedUserIDs    []int64       `json:"allowed_user_ids"`
	AllowedChatIDs    []int64       `json:"allowed_chat_ids"`
	HeartbeatInterval time.Duration `json:"heartbeat_interval"`
	UnreachableAfter  time.Duration `json:"unreachable_after"`
	CommandExpiry     time.Duration `json:"command_expiry"`
	ReadHeaderTimeout time.Duration `json:"read_header_timeout"`
	Secrets           BotSecrets    `json:"-"`
}

// WorkerConfig is the JSON configuration for codex-worker. WorkerID is the
// stable identity assigned at enrollment; Name is descriptive and may change.
type WorkerConfig struct {
	WorkerID              string           `json:"worker_id"`
	Name                  string           `json:"name"`
	StateFile             string           `json:"state_file"`
	GatewayURL            string           `json:"gateway_url"`
	TokenFile             string           `json:"token_file"`
	Runtimes              []RuntimeProfile `json:"runtimes"`
	AllowedWorkspaceRoots []string         `json:"allowed_workspace_roots"`
	MaxQueuedTurns        int              `json:"max_queued_turns"`
	RedactPatterns        []string         `json:"redact_patterns"`
}

// RuntimeProfile defines one local, supervised Codex app-server.
type RuntimeProfile struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	CodexBinary      string `json:"codex_binary"`
	WorkingDirectory string `json:"working_directory"`
	Autostart        bool   `json:"autostart"`
	RestartPolicy    string `json:"restart_policy"`
}

// BotSecrets are local Telegram identity and token settings. Values are kept
// out of error strings and callers must not log them.
type BotSecrets struct {
	BotName  string
	BotToken string
	WLName   string
	WLID     int64
}

// LoadGateway reads a JSON gateway config, applies safe defaults, and reads
// the permission-restricted bot secrets file. Relative secret paths are
// resolved relative to the JSON config file.
func LoadGateway(path string) (GatewayConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return GatewayConfig{}, fmt.Errorf("read gateway config: %w", err)
	}
	var raw gatewayJSON
	if err := decodeConfig(data, &raw); err != nil {
		return GatewayConfig{}, fmt.Errorf("parse gateway config: %w", err)
	}
	cfg, err := raw.config()
	if err != nil {
		return GatewayConfig{}, err
	}
	if !filepath.IsAbs(cfg.DatabasePath) {
		cfg.DatabasePath = filepath.Join(filepath.Dir(path), cfg.DatabasePath)
	}
	cfg.DatabasePath, err = filepath.Abs(cfg.DatabasePath)
	if err != nil {
		return GatewayConfig{}, fmt.Errorf("gateway config: database_path: %w", err)
	}
	if cfg.BotSecretsFile == "" {
		cfg.BotSecretsFile = DefaultBotSecretsFile
	}
	if !filepath.IsAbs(cfg.BotSecretsFile) {
		cfg.BotSecretsFile = filepath.Join(filepath.Dir(path), cfg.BotSecretsFile)
	}
	secrets, err := LoadBotSecrets(cfg.BotSecretsFile)
	if err != nil {
		return GatewayConfig{}, err
	}
	cfg.Secrets = secrets
	if len(cfg.AllowedUserIDs) == 0 {
		cfg.AllowedUserIDs = []int64{secrets.WLID}
	}
	if !containsOnly(cfg.AllowedUserIDs, secrets.WLID) {
		return GatewayConfig{}, errors.New("gateway config: allowed_user_ids must contain only the .botsecrets WLID")
	}
	return cfg, nil
}

// LoadWorker reads a JSON worker config and validates its stable identity and
// configured workspace roots. It does not read the enrollment token.
func LoadWorker(path string) (WorkerConfig, error) {
	if err := CheckSecretFilePermissions(path); err != nil {
		return WorkerConfig{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return WorkerConfig{}, fmt.Errorf("read worker config: %w", err)
	}
	var cfg WorkerConfig
	if err := decodeConfig(data, &cfg); err != nil {
		return WorkerConfig{}, fmt.Errorf("parse worker config: %w", err)
	}
	if _, err := uuid.Parse(cfg.WorkerID); err != nil {
		return WorkerConfig{}, errors.New("worker config: worker_id is required")
	}
	if strings.TrimSpace(cfg.Name) == "" {
		return WorkerConfig{}, errors.New("worker config: name is required")
	}
	if strings.TrimSpace(cfg.StateFile) == "" || strings.TrimSpace(cfg.GatewayURL) == "" || strings.TrimSpace(cfg.TokenFile) == "" {
		return WorkerConfig{}, errors.New("worker config: state_file, gateway_url, and token_file are required")
	}
	if len(cfg.AllowedWorkspaceRoots) == 0 {
		return WorkerConfig{}, errors.New("worker config: allowed_workspace_roots is required")
	}
	u, err := url.Parse(cfg.GatewayURL)
	if err != nil || u.Scheme != "wss" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return WorkerConfig{}, errors.New("worker config: gateway_url must be wss without embedded credentials, query, or fragment")
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return WorkerConfig{}, err
	}
	resolve := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	cfg.StateFile = resolve(cfg.StateFile)
	cfg.TokenFile = resolve(cfg.TokenFile)
	if err := CheckSecretFilePermissions(cfg.TokenFile); err != nil {
		return WorkerConfig{}, err
	}
	for i, root := range cfg.AllowedWorkspaceRoots {
		cfg.AllowedWorkspaceRoots[i] = resolve(root)
	}
	cfg.AllowedWorkspaceRoots, err = auth.CanonicalWorkspaceRoots(cfg.AllowedWorkspaceRoots)
	if err != nil {
		return WorkerConfig{}, err
	}
	if cfg.MaxQueuedTurns == 0 {
		cfg.MaxQueuedTurns = 20
	}
	if cfg.MaxQueuedTurns < 1 || cfg.MaxQueuedTurns > 1000 {
		return WorkerConfig{}, errors.New("worker config: max_queued_turns must be 1..1000")
	}
	if _, err := auth.NewRedactor(cfg.RedactPatterns, ""); err != nil {
		return WorkerConfig{}, err
	}
	seen := map[string]bool{}
	for i := range cfg.Runtimes {
		r := &cfg.Runtimes[i]
		if strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.CodexBinary) == "" || strings.TrimSpace(r.WorkingDirectory) == "" {
			return WorkerConfig{}, errors.New("worker config: runtime id, codex_binary, and working_directory are required")
		}
		if seen[r.ID] {
			return WorkerConfig{}, errors.New("worker config: duplicate runtime profile")
		}
		seen[r.ID] = true
		if r.Name == "" {
			r.Name = r.ID
		}
		if r.RestartPolicy == "" {
			r.RestartPolicy = "on-failure"
		}
		if r.RestartPolicy != "on-failure" && r.RestartPolicy != "never" {
			return WorkerConfig{}, errors.New("worker config: restart_policy must be on-failure or never")
		}
		r.WorkingDirectory, err = auth.CanonicalWorkspace(resolve(r.WorkingDirectory), cfg.AllowedWorkspaceRoots)
		if err != nil {
			return WorkerConfig{}, err
		}
		if strings.ContainsRune(r.CodexBinary, filepath.Separator) {
			r.CodexBinary = resolve(r.CodexBinary)
		}
	}
	return cfg, nil
}

type gatewayJSON struct {
	Listen            string  `json:"listen"`
	PublicBaseURL     string  `json:"public_base_url"`
	DatabasePath      string  `json:"database_path"`
	BotSecretsFile    string  `json:"bot_secrets_file"`
	WebhookSecretEnv  string  `json:"webhook_secret_env"`
	AllowedUserIDs    []int64 `json:"allowed_user_ids"`
	AllowedChatIDs    []int64 `json:"allowed_chat_ids"`
	HeartbeatInterval string  `json:"heartbeat_interval"`
	UnreachableAfter  string  `json:"unreachable_after"`
	CommandExpiry     string  `json:"command_expiry"`
	ReadHeaderTimeout string  `json:"read_header_timeout"`
}

func (r gatewayJSON) config() (GatewayConfig, error) {
	heartbeat, err := durationOrDefault(r.HeartbeatInterval, DefaultHeartbeatInterval)
	if err != nil {
		return GatewayConfig{}, fmt.Errorf("gateway config: heartbeat_interval: %w", err)
	}
	unreachable, err := durationOrDefault(r.UnreachableAfter, DefaultUnreachableAfter)
	if err != nil {
		return GatewayConfig{}, fmt.Errorf("gateway config: unreachable_after: %w", err)
	}
	expiry, err := durationOrDefault(r.CommandExpiry, DefaultCommandExpiry)
	if err != nil {
		return GatewayConfig{}, fmt.Errorf("gateway config: command_expiry: %w", err)
	}
	readHeader, err := durationOrDefault(r.ReadHeaderTimeout, DefaultReadHeaderTimeout)
	if err != nil {
		return GatewayConfig{}, fmt.Errorf("gateway config: read_header_timeout: %w", err)
	}
	if heartbeat <= 0 || unreachable <= heartbeat || expiry <= 0 || readHeader <= 0 {
		return GatewayConfig{}, errors.New("gateway config: durations must be positive and unreachable_after must exceed heartbeat_interval")
	}
	cfg := GatewayConfig{Listen: r.Listen, PublicBaseURL: r.PublicBaseURL, DatabasePath: r.DatabasePath, BotSecretsFile: r.BotSecretsFile, WebhookSecretEnv: r.WebhookSecretEnv, AllowedUserIDs: r.AllowedUserIDs, AllowedChatIDs: r.AllowedChatIDs, HeartbeatInterval: heartbeat, UnreachableAfter: unreachable, CommandExpiry: expiry, ReadHeaderTimeout: readHeader}
	if cfg.Listen == "" {
		cfg.Listen = DefaultGatewayListen
	}
	if cfg.PublicBaseURL == "" {
		cfg.PublicBaseURL = DefaultPublicBaseURL
	}
	u, err := url.Parse(cfg.PublicBaseURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return GatewayConfig{}, errors.New("gateway config: public_base_url must be an HTTPS origin")
	}
	cfg.PublicBaseURL = strings.TrimRight(cfg.PublicBaseURL, "/")
	host, _, err := net.SplitHostPort(cfg.Listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return GatewayConfig{}, errors.New("gateway config: listen must be a loopback IP and port behind nginx")
	}
	if strings.TrimSpace(cfg.DatabasePath) == "" || strings.TrimSpace(cfg.WebhookSecretEnv) == "" {
		return GatewayConfig{}, errors.New("gateway config: database_path and webhook_secret_env are required")
	}
	if cfg.DatabasePath == ":memory:" || strings.Contains(cfg.DatabasePath, "://") || strings.HasPrefix(cfg.DatabasePath, "file:") {
		return GatewayConfig{}, errors.New("gateway config: database_path must be a local SQLite file path")
	}
	return cfg, nil
}

func decodeConfig(data []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("configuration must contain exactly one JSON object")
	}
	return nil
}

func durationOrDefault(value string, fallback time.Duration) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}
	return time.ParseDuration(value)
}
func containsOnly(ids []int64, id int64) bool {
	if len(ids) != 1 {
		return false
	}
	return ids[0] == id
}
