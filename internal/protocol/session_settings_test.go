package protocol

import (
	"math"
	"strings"
	"testing"
)

func TestSessionSettingsRequiresBoundedRevisionAndNativeTokens(t *testing.T) {
	valid := SessionSettings{RuntimeGeneration: 1, Revision: 1, Model: "native-model", ReasoningEffort: "high"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	valid.ReasoningEffort = ""
	if err := valid.Validate(); err != nil {
		t.Fatalf("default effort must remain authoritative: %v", err)
	}
	for _, mutate := range []func(*SessionSettings){
		func(s *SessionSettings) { s.RuntimeGeneration = 0 },
		func(s *SessionSettings) { s.RuntimeGeneration = math.MaxUint64 },
		func(s *SessionSettings) { s.Revision = 0 },
		func(s *SessionSettings) { s.Revision = math.MaxUint64 },
		func(s *SessionSettings) { s.Model = "" },
		func(s *SessionSettings) { s.Model = strings.Repeat("m", 257) },
		func(s *SessionSettings) { s.Model = "model\nsecret" },
		func(s *SessionSettings) { s.ReasoningEffort = "high\nsecret" },
	} {
		invalid := valid
		mutate(&invalid)
		if invalid.Validate() == nil {
			t.Fatalf("invalid settings accepted: %+v", invalid)
		}
	}
}
