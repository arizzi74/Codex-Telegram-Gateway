package protocol

import (
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	WorkerEnrollmentCodeLength        = 12
	WorkerEnrollmentCodeAlphabet      = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	WorkerEnrollmentLifetime          = 10 * time.Minute
	WorkerEnrollmentPath              = "/tgw/enroll/"
	WorkerEnrollmentRedeemPath        = "/tgw/api/v1/worker-enrollments/redeem"
	WorkerEnrollmentServiceRestricted = "restricted"
	WorkerEnrollmentServiceFull       = "full"
)

type CreateWorkerEnrollmentRequest struct {
	ServiceAccess string `json:"service_access,omitempty"`
}

// CreateWorkerEnrollmentResponse contains a temporary enrollment capability,
// not worker credentials. Treat EnrollmentURL as a secret until it expires.
type CreateWorkerEnrollmentResponse struct {
	EnrollmentID  string    `json:"enrollment_id"`
	EnrollmentURL string    `json:"enrollment_url"`
	ExpiresAt     time.Time `json:"expires_at"`
	ServiceAccess string    `json:"service_access"`
}

type RedeemWorkerEnrollmentRequest struct {
	Code string `json:"code"`
	Name string `json:"name"`
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// RedeemWorkerEnrollmentResponse is returned once to the installer. Token must
// be saved privately and must not appear in logs or installer output.
type RedeemWorkerEnrollmentResponse struct {
	WorkerID      string `json:"worker_id"`
	Token         string `json:"token"`
	GatewayURL    string `json:"gateway_url"`
	ServiceAccess string `json:"service_access"`
}

func NormalizeWorkerEnrollmentCode(code string) (string, error) {
	if len(code) != WorkerEnrollmentCodeLength {
		return "", errors.New("invalid enrollment code")
	}
	var normalized [WorkerEnrollmentCodeLength]byte
	for i := range code {
		c := code[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if !strings.ContainsRune(WorkerEnrollmentCodeAlphabet, rune(c)) {
			return "", errors.New("invalid enrollment code")
		}
		normalized[i] = c
	}
	return string(normalized[:]), nil
}

func ValidateWorkerEnrollmentName(name string) error {
	if !utf8.ValidString(name) || strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > 160 {
		return errors.New("worker name must contain 1 to 160 characters")
	}
	for _, r := range name {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return errors.New("worker name must be a single line without control characters")
		}
	}
	return nil
}

func NormalizeWorkerEnrollmentServiceAccess(access string) (string, error) {
	if access == "" {
		return WorkerEnrollmentServiceRestricted, nil
	}
	switch access {
	case WorkerEnrollmentServiceRestricted, WorkerEnrollmentServiceFull:
		return access, nil
	default:
		return "", errors.New("invalid worker service access")
	}
}
