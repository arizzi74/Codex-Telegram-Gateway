package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadGatewayDefaultsAndSecretAllowlist(t *testing.T) {
	dir := t.TempDir()
	secrets := filepath.Join(dir, ".botsecrets")
	writeSecret(t, secrets, "BOTNAME=bot\nBOTTOKEN=do-not-log\nWLNAME=person\nWLID=12345\n")
	configPath := filepath.Join(dir, "gateway.json")
	writeFile(t, configPath, `{"database_url_env":"DATABASE_URL","webhook_secret_env":"WEBHOOK_SECRET"}`)
	cfg, err := LoadGateway(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != DefaultGatewayListen || cfg.PublicBaseURL != "https://gateway.example.com" {
		t.Fatalf("defaults not applied: %#v", cfg)
	}
	if cfg.HeartbeatInterval != 10*time.Second || cfg.UnreachableAfter != 30*time.Second {
		t.Fatal("timeout defaults not applied")
	}
	if len(cfg.AllowedUserIDs) != 1 || cfg.AllowedUserIDs[0] != 12345 {
		t.Fatal("secrets WLID was not the single allowlist identity")
	}
}

func TestLoadGatewayRejectsAdditionalUserIdentity(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, filepath.Join(dir, ".botsecrets"), "BOTNAME=b\nBOTTOKEN=x\nWLNAME=n\nWLID=7\n")
	path := filepath.Join(dir, "gateway.json")
	writeFile(t, path, `{"database_url_env":"DB","webhook_secret_env":"WH","allowed_user_ids":[7,8]}`)
	if _, err := LoadGateway(path); err == nil {
		t.Fatal("multiple IDs accepted")
	}
}

func TestBotSecretsRejectsUnsafePermissionsAndShellSyntax(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".botsecrets")
	writeFile(t, path, "BOTNAME=$(id)\nBOTTOKEN=abc\nWLNAME=name\nWLID=99\n")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBotSecrets(path); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("unsafe permissions result: %v", err)
	}
	writeSecret(t, path, "BOTNAME=$(id)\nBOTTOKEN=abc\nWLNAME=name\nWLID=99\n")
	secrets, err := LoadBotSecrets(path)
	if err != nil {
		t.Fatal(err)
	}
	if secrets.BotName != "$(id)" {
		t.Fatal("value was unexpectedly evaluated")
	}
}

func TestLoadWorkerRequiresStableIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.json")
	writeFile(t, path, `{"name":"machine","state_file":"state.db","gateway_url":"wss://gateway","token_file":"token","allowed_workspace_roots":["/tmp"]}`)
	if _, err := LoadWorker(path); err == nil {
		t.Fatal("worker without stable ID accepted")
	}
}

func writeSecret(t *testing.T, path, contents string) {
	t.Helper()
	writeFile(t, path, contents)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}
func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRejectUnknownConfigurationAndPublicListener(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, filepath.Join(dir, ".botsecrets"), "BOTNAME=b\nBOTTOKEN=x\nWLNAME=n\nWLID=7\n")
	path := filepath.Join(dir, "gateway.json")
	for _, extra := range []string{`,"allowd_user_ids":[8]`, `,"listen":"0.0.0.0:8080"`, `,"public_base_url":"http://example.com"`} {
		writeFile(t, path, `{"database_url_env":"DB","webhook_secret_env":"WH"`+extra+`}`)
		if _, err := LoadGateway(path); err == nil {
			t.Fatal("unsafe configuration accepted")
		}
	}
}
