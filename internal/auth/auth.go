// Package auth provides small, dependency-free authentication and local path
// authorization helpers for the gateway and worker.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

const workerTokenPrefix = "cwk_"

// GenerateWorkerToken returns a one-time enrollment token made from at least
// 32 random bytes. Store only HashWorkerToken(token).
func GenerateWorkerToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate worker token: %w", err)
	}
	return workerTokenPrefix + hex.EncodeToString(b), nil
}

// HashWorkerToken returns the SHA-256 digest used for persistent token storage.
func HashWorkerToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// VerifyWorkerToken compares a candidate token to a stored SHA-256 digest in
// constant time. Malformed stored hashes never authorize a token.
func VerifyWorkerToken(token string, storedHash []byte) bool {
	if len(storedHash) != sha256.Size {
		return false
	}
	candidate := HashWorkerToken(token)
	return subtle.ConstantTimeCompare(candidate, storedHash) == 1
}

// ValidateWebhookSecret compares the Telegram header to the configured secret
// in constant time. Empty secrets are never accepted.
func ValidateWebhookSecret(received, expected string) bool {
	if received == "" || expected == "" {
		return false
	}
	receivedHash := sha256.Sum256([]byte(received))
	expectedHash := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(receivedHash[:], expectedHash[:]) == 1
}

// AuthorizedTelegramUser matches the immutable numeric Telegram user ID. A
// username is only descriptive and must not be used for authorization.
func AuthorizedTelegramUser(userID int64, allowedUserIDs []int64) bool {
	if userID <= 0 {
		return false
	}
	for _, allowed := range allowedUserIDs {
		if userID == allowed {
			return true
		}
	}
	return false
}

// CanonicalWorkspace returns a symlink-resolved path only when it lies within
// one of the configured symlink-resolved workspace roots.
func CanonicalWorkspace(path string, allowedRoots []string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("workspace path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("make workspace path absolute: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(filepath.Clean(absolute))
	if err != nil {
		return "", fmt.Errorf("resolve workspace path: %w", err)
	}
	for _, root := range allowedRoots {
		rootPath, err := canonicalRoot(root)
		if err != nil {
			return "", err
		}
		rel, err := filepath.Rel(rootPath, canonical)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return canonical, nil
		}
	}
	return "", errors.New("workspace path is outside configured allowed roots")
}

// CanonicalWorkspaceRoots resolves configured roots once for callers that need
// to validate many paths.
func CanonicalWorkspaceRoots(roots []string) ([]string, error) {
	if len(roots) == 0 {
		return nil, errors.New("at least one workspace root is required")
	}
	canonical := make([]string, 0, len(roots))
	for _, root := range roots {
		resolved, err := canonicalRoot(root)
		if err != nil {
			return nil, err
		}
		canonical = append(canonical, resolved)
	}
	return canonical, nil
}

func canonicalRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("workspace root is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("make workspace root absolute: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(abs))
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	return resolved, nil
}

// Redactor removes configured sensitive values from text before it is logged.
type Redactor struct {
	expressions []*regexp.Regexp
	replacement string
}

// NewRedactor compiles regular expressions once. An empty replacement becomes
// "[REDACTED]" so a misconfiguration cannot accidentally disclose a match.
func NewRedactor(patterns []string, replacement string) (*Redactor, error) {
	if replacement == "" {
		replacement = "[REDACTED]"
	}
	r := &Redactor{replacement: replacement}
	for _, pattern := range patterns {
		expression, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("compile redaction pattern: %w", err)
		}
		r.expressions = append(r.expressions, expression)
	}
	return r, nil
}

// Redact replaces all configured matches. A nil Redactor leaves text alone.
func (r *Redactor) Redact(text string) string {
	if r == nil {
		return text
	}
	for _, expression := range r.expressions {
		text = expression.ReplaceAllString(text, r.replacement)
	}
	return text
}
