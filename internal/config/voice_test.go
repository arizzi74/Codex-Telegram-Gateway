package config

import "testing"

func TestGatewayVoiceConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		v       VoiceConfig
		invalid bool
	}{
		{"disabled by default", VoiceConfig{}, false},
		{"enabled defaults", VoiceConfig{Enabled: true}, false},
		{"relative helper", VoiceConfig{HelperPath: "whisper"}, true},
		{"relative model", VoiceConfig{ModelPath: "model.bin"}, true},
		{"negative threads", VoiceConfig{Threads: -1}, true},
		{"excessive threads", VoiceConfig{Threads: 3}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := (gatewayJSON{DatabasePath: "gateway.db", WebhookSecretEnv: "WH", Voice: tc.v}).config()
			if (err != nil) != tc.invalid {
				t.Fatalf("validation err=%v", err)
			}
			if err == nil && (cfg.Voice.Enabled != tc.v.Enabled || cfg.Voice.Threads != 2 || cfg.Voice.HelperPath == "" || cfg.Voice.ModelPath == "") {
				t.Fatalf("defaults %+v", cfg.Voice)
			}
		})
	}
}
