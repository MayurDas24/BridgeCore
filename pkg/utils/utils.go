// Package utils holds small, dependency-light helpers shared across
// services: password hashing and secure random API key generation.
package utils

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// HashPassword bcrypt-hashes a plaintext password with the default cost.
// Cost 12 is used explicitly (rather than bcrypt.DefaultCost=10) as a
// deliberate tradeoff toward security given modern hardware.
func HashPassword(plaintext string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), 12)
	if err != nil {
		return "", fmt.Errorf("utils: hash password: %w", err)
	}
	return string(hash), nil
}

// CheckPassword compares a plaintext password against a bcrypt hash.
func CheckPassword(hash, plaintext string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext)) == nil
}

// HashToken hashes an opaque, high-entropy token (refresh token JWT / API
// key) before persistence, so a database leak alone never yields usable
// credentials. Unlike passwords, these tokens are already cryptographically
// random/signed and can exceed bcrypt's 72-byte input limit (a JWT easily
// does), so SHA-256 is used instead of bcrypt here — bcrypt's deliberate
// slowness exists to blunt brute-forcing low-entropy human passwords,
// which doesn't apply to a 256-bit-plus token that can't be guessed either
// way.
func HashToken(token string) (string, error) {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:]), nil
}

// CheckToken compares a plaintext token against its stored SHA-256 hash
// using a constant-time comparison to avoid timing side-channels.
func CheckToken(hash, token string) bool {
	sum := sha256.Sum256([]byte(token))
	candidate := hex.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(hash), []byte(candidate)) == 1
}

var base32Encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateSecret returns a cryptographically random, URL-safe secret of the
// given byte length, base32-encoded (uppercase, no padding) so it reads
// cleanly in UIs and copy-paste flows.
func GenerateSecret(numBytes int) (string, error) {
	buf := make([]byte, numBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("utils: generate secret: %w", err)
	}
	return strings.ToLower(base32Encoding.EncodeToString(buf)), nil
}

// GenerateAPIKey builds a full API key of the form "<prefix><random>",
// e.g. "bc_live_k7j2n9qz...". It returns the full plaintext key (shown to
// the user exactly once) along with its last four characters (safe to
// store and display for identification).
func GenerateAPIKey(prefix string) (fullKey string, lastFour string, err error) {
	secret, err := GenerateSecret(24)
	if err != nil {
		return "", "", err
	}
	fullKey = prefix + secret
	if len(fullKey) >= 4 {
		lastFour = fullKey[len(fullKey)-4:]
	} else {
		lastFour = fullKey
	}
	return fullKey, lastFour, nil
}
