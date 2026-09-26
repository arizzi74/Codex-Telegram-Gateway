package protocol

import (
	"errors"
	"strings"
	"time"
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
	RequestID string             `json:"request_id"`
	State     string             `json:"state"`
	Version   string             `json:"version,omitempty"`
	ErrorCode string             `json:"error_code,omitempty"`
	Codex     *CodexUpdateReport `json:"codex,omitempty"`
}

// CodexUpdateReport is independent of worker binary maintenance: a runtime
// check or installation failure must not erase a successful worker update.
type CodexUpdateReport struct {
	State         string                `json:"state"`
	LatestVersion string                `json:"latest_version,omitempty"`
	CheckedAt     time.Time             `json:"checked_at,omitempty"`
	ErrorCode     string                `json:"error_code,omitempty"`
	Profiles      []CodexRuntimeVersion `json:"profiles,omitempty"`
}

type CodexRuntimeVersion struct {
	ProfileID        string `json:"profile_id"`
	InstalledVersion string `json:"installed_version,omitempty"`
	RunningVersion   string `json:"running_version,omitempty"`
	Support          string `json:"support"`
}

func validCodexReportVersion(value string) bool {
	if len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || strings.ContainsRune(".-_+", r)) {
			return false
		}
	}
	return true
}

func (r CodexUpdateReport) Validate() error {
	if !validCodexReportVersion(r.LatestVersion) || len(r.Profiles) > 128 || r.CheckedAt.Year() < 1 || r.CheckedAt.Year() > 9999 {
		return errors.New("invalid Codex update report")
	}
	switch r.State {
	case "up_to_date", "completed", "unsupported", "no_runtimes":
		if r.ErrorCode != "" {
			return errors.New("invalid successful Codex update report")
		}
	case "failed":
		switch r.ErrorCode {
		case "check_failed", "inspection_failed", "update_failed", "worker_update_failed":
		default:
			return errors.New("invalid Codex update error")
		}
	case "withheld":
		if r.ErrorCode != "rejected_release" {
			return errors.New("invalid withheld Codex update report")
		}
	default:
		return errors.New("invalid Codex update state")
	}
	seen := make(map[string]bool)
	for _, profile := range r.Profiles {
		if profile.ProfileID == "" || len(profile.ProfileID) > 256 || !utf8.ValidString(profile.ProfileID) || strings.IndexFunc(profile.ProfileID, unicode.IsControl) >= 0 || seen[profile.ProfileID] || !validCodexReportVersion(profile.InstalledVersion) || !validCodexReportVersion(profile.RunningVersion) {
			return errors.New("invalid Codex runtime version report")
		}
		seen[profile.ProfileID] = true
		switch profile.Support {
		case "supported", "external", "unavailable":
		default:
			return errors.New("invalid Codex runtime update support")
		}
	}
	return nil
}

func (r WorkerUpdateResult) Validate() error {
	id, err := uuid.Parse(r.RequestID)
	if err != nil || id == uuid.Nil || id.String() != r.RequestID {
		return errors.New("invalid worker update result identity")
	}
	if r.Codex != nil {
		if err := r.Codex.Validate(); err != nil {
			return err
		}
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
