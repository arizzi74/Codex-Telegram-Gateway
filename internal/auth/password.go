package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	passwordMemory  = 64 * 1024
	passwordTime    = 3
	passwordThreads = 1
	passwordSaltLen = 16
	passwordKeyLen  = 32
	passwordPrefix  = "$argon2id$v=19$m=65536,t=3,p=1$"
)

// HashPassword uses a fresh salt and a fixed Argon2id cost. Callers apply their
// password policy and concurrency limits before invoking this expensive work.
func HashPassword(password string) (string, error) {
	if len(password) > 256 {
		return "", errors.New("auth: password exceeds maximum size")
	}
	salt := make([]byte, passwordSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate password salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, passwordTime, passwordMemory, passwordThreads, passwordKeyLen)
	return passwordPrefix + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}

// VerifyPassword accepts only the bounded parameter profile produced here.
// A malformed durable value can never request excessive memory or CPU work.
func VerifyPassword(password, encoded string) bool {
	if len(password) > 256 || len(encoded) > 128 || !strings.HasPrefix(encoded, passwordPrefix) {
		return false
	}
	saltText, keyText, ok := strings.Cut(strings.TrimPrefix(encoded, passwordPrefix), "$")
	if !ok || len(saltText) != base64.RawStdEncoding.EncodedLen(passwordSaltLen) || len(keyText) != base64.RawStdEncoding.EncodedLen(passwordKeyLen) {
		return false
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(saltText)
	if err != nil || len(salt) != passwordSaltLen {
		return false
	}
	want, err := base64.RawStdEncoding.Strict().DecodeString(keyText)
	if err != nil || len(want) != passwordKeyLen {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, passwordTime, passwordMemory, passwordThreads, passwordKeyLen)
	return subtle.ConstantTimeCompare(actual, want) == 1
}

// DummyVerifyPassword performs the normal verification cost for unavailable
// accounts, avoiding an inexpensive username or enabled-state oracle.
func DummyVerifyPassword(password string) {
	if len(password) > 256 {
		return
	}
	key := argon2.IDKey([]byte(password), make([]byte, passwordSaltLen), passwordTime, passwordMemory, passwordThreads, passwordKeyLen)
	_ = subtle.ConstantTimeCompare(key, make([]byte, passwordKeyLen))
}
