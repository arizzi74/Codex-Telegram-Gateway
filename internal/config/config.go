// Package config loads the gateway and worker configuration files.
//
// Configuration is JSON deliberately: encoding/json is in the Go standard
// library, the format has unambiguous primitive types, and it keeps the first
// deployment dependency-free. Secrets remain in environment variables or
// permission-restricted files and are never stored in these JSON files.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
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
	DatabaseURLEnv    string        `json:"database_url_env"`
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
	if err := json.Unmarshal(data, &raw); err != nil {
		return GatewayConfig{}, fmt.Errorf("parse gateway config: %w", err)
	}
	cfg, err := raw.config()
	if err != nil {
		return GatewayConfig{}, err
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
	data, err := os.ReadFile(path)
	if err != nil {
		return WorkerConfig{}, fmt.Errorf("read worker config: %w", err)
	}
	var cfg WorkerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return WorkerConfig{}, fmt.Errorf("parse worker config: %w", err)
	}
	if strings.TrimSpace(cfg.WorkerID) == "" {
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
	for _, r := range cfg.Runtimes {
		if strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.CodexBinary) == "" || strings.TrimSpace(r.WorkingDirectory) == "" {
			return WorkerConfig{}, errors.New("worker config: runtime id, codex_binary, and working_directory are required")
		}
	}
	return cfg, nil
}

type gatewayJSON struct {
	Listen            string  `json:"listen"`
	PublicBaseURL     string  `json:"public_base_url"`
	DatabaseURLEnv    string  `json:"database_url_env"`
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
	cfg := GatewayConfig{Listen: r.Listen, PublicBaseURL: r.PublicBaseURL, DatabaseURLEnv: r.DatabaseURLEnv, BotSecretsFile: r.BotSecretsFile, WebhookSecretEnv: r.WebhookSecretEnv, AllowedUserIDs: r.AllowedUserIDs, AllowedChatIDs: r.AllowedChatIDs, HeartbeatInterval: heartbeat, UnreachableAfter: unreachable, CommandExpiry: expiry, ReadHeaderTimeout: readHeader}
	if cfg.Listen == "" {
		cfg.Listen = DefaultGatewayListen
	}
	if cfg.PublicBaseURL == "" {
		cfg.PublicBaseURL = DefaultPublicBaseURL
	}
	if strings.TrimSpace(cfg.DatabaseURLEnv) == "" || strings.TrimSpace(cfg.WebhookSecretEnv) == "" {
		return GatewayConfig{}, errors.New("gateway config: database_url_env and webhook_secret_env are required")
	}
	return cfg, nil
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
