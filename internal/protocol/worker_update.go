package protocol

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Maintenance targets a worker independently of its sessions and runtimes.
// It carries no executable, download URL, version override, or shell arguments.
type WorkerUpdateRequest struct {
	RequestID string `json:"request_id"`
	WorkerID  string `json:"worker_id"`
}

func (r WorkerUpdateRequest) Validate() error {
	for _, value := range []string{r.RequestID, r.WorkerID} {
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil || id.String() != value {
			return errors.New("invalid worker update identity")
		}
	}
	return nil
}

type WorkerUpdateResult struct {
	RequestID string `json:"request_id"`
	State     string `json:"state"`
	Version   string `json:"version,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
}

func (r WorkerUpdateResult) Validate() error {
	id, err := uuid.Parse(r.RequestID)
	if err != nil || id == uuid.Nil || id.String() != r.RequestID {
		return errors.New("invalid worker update result identity")
	}
	if len(r.Version) > 128 || !utf8.ValidString(r.Version) || strings.IndexFunc(r.Version, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return errors.New("invalid worker update version")
	}
	switch r.State {
	case "completed", "up_to_date":
		if r.Version == "" || r.ErrorCode != "" {
			return errors.New("invalid successful worker update")
		}
	case "failed":
		if len(r.ErrorCode) == 0 || len(r.ErrorCode) > 64 {
			return errors.New("invalid worker update error")
		}
		for _, r := range r.ErrorCode {
			if (r < 'a' || r > 'z') && r != '_' {
				return errors.New("invalid worker update error")
			}
		}
	default:
		return errors.New("invalid worker update state")
	}
	return nil
}
