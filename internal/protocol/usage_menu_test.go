package protocol

import (
	"testing"

	"github.com/google/uuid"
)

func TestUsageMenuRestrictsCommandsAndConfirmationCapabilities(t *testing.T) {
	for _, args := range []string{"--menu", "daily", "weekly", "cumulative", "resets", "resets 2", "redeem", "cancel", "redeem " + uuid.NewString(), "confirm " + uuid.NewString()} {
		if err := (&UsageMenu{Options: []UsageOption{{Args: args, Label: "Choice"}}}).Validate(); err != nil {
			t.Errorf("valid %q: %v", args, err)
		}
	}
	for _, args := range []string{"", "confirm", "confirm fake", "confirm " + uuid.Nil.String(), "account/logout", "redeem credit-id", "cancel\n", "resets -1", "resets 01", "resets 1001"} {
		if ValidUsageMenuArgs(args) {
			t.Errorf("accepted %q", args)
		}
	}
	if (&UsageMenu{Options: []UsageOption{{Args: "resets", Label: "Choose"}, {Args: "resets", Label: "Duplicate"}}}).Validate() == nil {
		t.Fatal("duplicate actions accepted")
	}
}
