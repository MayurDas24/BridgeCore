package utils

import (
	"strings"
	"testing"
)

func TestHashPassword_And_CheckPassword(t *testing.T) {
	hash, err := HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if hash == "" || hash == "correct-horse-battery-staple" {
		t.Fatal("expected a bcrypt hash distinct from the plaintext")
	}

	if !CheckPassword(hash, "correct-horse-battery-staple") {
		t.Fatal("expected CheckPassword to succeed with the correct password")
	}
	if CheckPassword(hash, "wrong-password") {
		t.Fatal("expected CheckPassword to fail with an incorrect password")
	}
}

func TestHashPassword_ProducesDifferentHashesForSamePassword(t *testing.T) {
	// bcrypt salts each hash, so hashing the same password twice must never
	// produce the same output — this guards against a regression to a
	// non-salted scheme.
	h1, err := HashPassword("same-password")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	h2, err := HashPassword("same-password")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if h1 == h2 {
		t.Fatal("expected two hashes of the same password to differ (salting)")
	}
}

func TestGenerateAPIKey_HasExpectedPrefixAndLastFour(t *testing.T) {
	prefix := "bc_live_"
	fullKey, lastFour, err := GenerateAPIKey(prefix)
	if err != nil {
		t.Fatalf("GenerateAPIKey() error = %v", err)
	}

	if !strings.HasPrefix(fullKey, prefix) {
		t.Fatalf("expected key to start with %q, got %q", prefix, fullKey)
	}
	if len(lastFour) != 4 {
		t.Fatalf("expected last_four to be 4 characters, got %q", lastFour)
	}
	if !strings.HasSuffix(fullKey, lastFour) {
		t.Fatal("expected last_four to match the tail of the full key")
	}
}

func TestGenerateAPIKey_IsUnique(t *testing.T) {
	k1, _, err := GenerateAPIKey("bc_live_")
	if err != nil {
		t.Fatalf("GenerateAPIKey() error = %v", err)
	}
	k2, _, err := GenerateAPIKey("bc_live_")
	if err != nil {
		t.Fatalf("GenerateAPIKey() error = %v", err)
	}
	if k1 == k2 {
		t.Fatal("expected two generated API keys to be different")
	}
}

func TestHashToken_And_CheckToken(t *testing.T) {
	token := "bc_live_abcdef1234567890"
	hash, err := HashToken(token)
	if err != nil {
		t.Fatalf("HashToken() error = %v", err)
	}
	if !CheckToken(hash, token) {
		t.Fatal("expected CheckToken to succeed for the correct token")
	}
	if CheckToken(hash, "bc_live_wrongtoken") {
		t.Fatal("expected CheckToken to fail for an incorrect token")
	}
}
