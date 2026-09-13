package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// LoadBotSecrets reads KEY=value records without evaluating them as shell.
// Required keys are BOTNAME, BOTTOKEN, WLNAME, and WLID.
func LoadBotSecrets(path string) (BotSecrets, error) {
	if err := CheckSecretFilePermissions(path); err != nil {
		return BotSecrets{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return BotSecrets{}, fmt.Errorf("open bot secrets: %w", err)
	}
	defer f.Close()
	values := make(map[string]string)
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return BotSecrets{}, errors.New("parse bot secrets: expected KEY=value")
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key != "BOTNAME" && key != "BOTTOKEN" && key != "WLNAME" && key != "WLID" {
			return BotSecrets{}, fmt.Errorf("parse bot secrets: unsupported key %q", key)
		}
		if _, exists := values[key]; exists {
			return BotSecrets{}, fmt.Errorf("parse bot secrets: duplicate key %q", key)
		}
		values[key] = value
	}
	if err := s.Err(); err != nil {
		return BotSecrets{}, fmt.Errorf("read bot secrets: %w", err)
	}
	for _, key := range []string{"BOTNAME", "BOTTOKEN", "WLNAME", "WLID"} {
		if values[key] == "" {
			return BotSecrets{}, fmt.Errorf("parse bot secrets: %s is required", key)
		}
	}
	id, err := strconv.ParseInt(values["WLID"], 10, 64)
	if err != nil || id <= 0 {
		return BotSecrets{}, errors.New("parse bot secrets: WLID must be a positive numeric Telegram user ID")
	}
	return BotSecrets{BotName: values["BOTNAME"], BotToken: values["BOTTOKEN"], WLName: values["WLNAME"], WLID: id}, nil
}

// CheckSecretFilePermissions requires a regular file that is readable only by
// its owner (0600 or stricter). It is portable for Unix service deployments.
func CheckSecretFilePermissions(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat secret file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("secret file must be regular")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("secret file permissions must be 0600 or stricter")
	}
	return nil
}
