package protocol

import (
	"errors"
	"math"
)

// SessionSettings is a confirmed native session preference, independent of
// model/effort recorded in historical turn usage. An empty ReasoningEffort
// authoritatively selects the model default. Revisions increase within a
// runtime generation and are persisted by the worker before publication.
type SessionSettings struct {
	RuntimeGeneration uint64 `json:"runtime_generation"`
	Revision          uint64 `json:"revision"`
	Model             string `json:"model"`
	ReasoningEffort   string `json:"reasoning_effort"`
}

func (s SessionSettings) Validate() error {
	if s.RuntimeGeneration == 0 || s.RuntimeGeneration > math.MaxInt64 || s.Revision == 0 || s.Revision > math.MaxInt64 ||
		!ModelMenuToken(s.Model) || (s.ReasoningEffort != "" && !ModelMenuToken(s.ReasoningEffort)) {
		return errors.New("invalid session settings")
	}
	return nil
}
