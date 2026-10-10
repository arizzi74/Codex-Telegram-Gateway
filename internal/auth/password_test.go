package auth

import (
	"strings"
	"testing"
)

func TestPasswordHashSaltAndVerification(t *testing.T) {
	password := "  a secret pass phrase  "
	first, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	second, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || strings.Contains(first, password) || !strings.HasPrefix(first, passwordPrefix) {
		t.Fatal("password hashes must be salted Argon2id values")
	}
	if !VerifyPassword(password, first) || VerifyPassword(strings.TrimSpace(password), first) || VerifyPassword("wrong password", first) {
		t.Fatal("incorrect password verification or whitespace normalization")
	}
}

func TestPasswordVerificationRejectsUnboundedOrMalformedHashes(t *testing.T) {
	suffix := "AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	for _, encoded := range []string{
		"", "$argon2i$v=19$m=65536,t=3,p=1$" + suffix,
		"$argon2id$v=19$m=4294967295,t=3,p=1$" + suffix,
		"$argon2id$v=19$m=65536,t=4294967295,p=1$" + suffix,
		"$argon2id$v=19$m=65536,t=3,p=255$" + suffix,
		"$argon2id$v=16$m=65536,t=3,p=1$" + suffix,
		passwordPrefix + "bad$bad", passwordPrefix + suffix + "$extra",
		passwordPrefix + strings.Repeat("A", 22) + "$" + strings.Repeat("A", 1000),
	} {
		if VerifyPassword("correct horse battery staple", encoded) {
			t.Fatalf("accepted malformed or unbounded hash %q", encoded)
		}
	}
	if _, err := HashPassword(strings.Repeat("x", 257)); err == nil {
		t.Fatal("oversized password hashed")
	}
	if VerifyPassword(strings.Repeat("x", 257), passwordPrefix+suffix) {
		t.Fatal("oversized password verified")
	}
}
