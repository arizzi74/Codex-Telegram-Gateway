package releasemanager

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWorkerPrepareFailureClassifiesFailedSubagentSafetyChecks(t *testing.T) {
	for _, test := range []struct {
		reason string
		want   string
	}{
		{"worker update: background terminals are active or cannot be verified", "worker update deferred: background terminals are active or cannot be verified"},
		{"worker update: failed subagent ancestry cannot be verified", "worker update deferred: failed subagent completion cannot be verified"},
		{"worker update: failed subagent turn does not match observed native activity", "worker update deferred: failed subagent completion cannot be verified"},
		{"worker update: native terminal subagent completion could not be verified", "worker update deferred: failed subagent completion cannot be verified"},
		{"worker update: failed subagent changed during idle verification", "worker update deferred: activity changed during failed subagent recovery; retrying at the next check"},
		{"worker update: native thread changed during failed subagent recovery", "worker update deferred: activity changed during failed subagent recovery; retrying at the next check"},
		{"worker update: too many native queues or failed subagents to verify", "worker update deferred: too many native threads or queues to verify"},
		{"worker update: too many native threads to verify", "worker update deferred: too many native threads or queues to verify"},
	} {
		t.Run(test.reason, func(t *testing.T) {
			output, err := json.Marshal(map[string]string{"error": test.reason})
			if err != nil {
				t.Fatal(err)
			}
			if got := workerPrepareFailure(output); got != test.want {
				t.Fatalf("failure classification = %q; want %q", got, test.want)
			}
			const secret = "PRIVATE_NATIVE_INPUT_TOKEN"
			for _, contaminated := range []map[string]string{
				{"error": test.reason + ": " + secret},
				{"error": test.reason, "native_output": secret},
			} {
				output, err := json.Marshal(contaminated)
				if err != nil {
					t.Fatal(err)
				}
				if got := workerPrepareFailure(output); got != workerPrepareUnknownReason || strings.Contains(got, secret) {
					t.Fatalf("private subprocess output was accepted: %q", got)
				}
			}
		})
	}
}
