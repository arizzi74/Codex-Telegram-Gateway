package config

import "testing"

func TestGatewayVoiceConfig(t *testing.T) {
	for _, tc := range []struct {
		name        string
		v           VoiceConfig
		wantThreads int
		invalid     bool
	}{
		{"disabled by default", VoiceConfig{}, 2, false},
		{"enabled defaults", VoiceConfig{Enabled: true}, 2, false},
		{"one thread", VoiceConfig{Threads: 1}, 1, false},
		{"two threads", VoiceConfig{Threads: 2}, 2, false},
		{"small model with three threads", VoiceConfig{Enabled: true, ModelPath: "/models/ggml-small.bin", Threads: 3}, 3, false},
		{"maximum threads", VoiceConfig{Threads: 32}, 32, false},
		{"relative helper", VoiceConfig{HelperPath: "whisper"}, 0, true},
		{"relative model", VoiceConfig{ModelPath: "model.bin"}, 0, true},
		{"negative threads", VoiceConfig{Threads: -1}, 0, true},
		{"excessive threads", VoiceConfig{Threads: 33}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := (gatewayJSON{DatabasePath: "gateway.db", WebhookSecretEnv: "WH", Voice: tc.v}).config()
			if (err != nil) != tc.invalid {
				t.Fatalf("validation err=%v", err)
			}
			if err == nil && (cfg.Voice.Enabled != tc.v.Enabled || cfg.Voice.Threads != tc.wantThreads || cfg.Voice.HelperPath == "" || cfg.Voice.ModelPath == "") {
				t.Fatalf("defaults %+v", cfg.Voice)
			}
			if err == nil && tc.v.ModelPath != "" && cfg.Voice.ModelPath != tc.v.ModelPath {
				t.Fatalf("model path = %q, want %q", cfg.Voice.ModelPath, tc.v.ModelPath)
			}
		})
	}
}
