package gateway

import (
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestInboundFileUnknownOutcomeKeepsSavedPath(t *testing.T) {
	redactor, err := auth.NewRedactor([]string{`private-name`}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, saved := range []bool{false, true} {
		message := "unconfirmed RPC diagnostics"
		if saved {
			message = "File saved in /work/inbound/private-name.pdf\n" + message
		}
		result := protocol.Result{Error: &protocol.Error{Code: protocol.OutcomeUnknown, Message: message}}
		text, _, err := testSender(renderFixture(), redactor).render(t.Context(), eventRow(t, "command_result_unknown", result, testSessionID.String()))
		if err != nil || !strings.Contains(text, "The command outcome is unknown") || strings.Contains(text, "unconfirmed RPC diagnostics") || strings.Contains(text, "private-name") {
			t.Fatalf("unknown outcome rendering: %q %v", text, err)
		}
		if strings.Contains(text, "File saved in /work/inbound/[REDACTED].pdf") != saved {
			t.Fatalf("saved path visibility: %q saved=%v", text, saved)
		}
	}
}
