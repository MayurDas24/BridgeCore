package jwt

import (
	"testing"
	"time"
)

func newTestManager() *Manager {
	return NewManager(
		"test-access-secret",
		"test-refresh-secret",
		15*time.Minute,
		7*24*time.Hour,
		"bridgecore-test",
	)
}

func TestIssueAndVerifyAccessToken(t *testing.T) {
	m := newTestManager()

	token, expiresAt, err := m.IssueAccessToken("user-1", "tenant-1", "admin")
	if err != nil {
		t.Fatalf("IssueAccessToken() error = %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}
	if !expiresAt.After(time.Now()) {
		t.Fatal("expected expiry to be in the future")
	}

	claims, err := m.VerifyAccessToken(token)
	if err != nil {
		t.Fatalf("VerifyAccessToken() error = %v", err)
	}
	if claims.UserID != "user-1" || claims.TenantID != "tenant-1" || claims.Role != "admin" {
		t.Fatalf("unexpected claims: %+v", claims)
	}
	if claims.TokenUse != "access" {
		t.Fatalf("expected token_use=access, got %q", claims.TokenUse)
	}
}

func TestIssueAndVerifyRefreshToken(t *testing.T) {
	m := newTestManager()

	token, _, err := m.IssueRefreshToken("user-2", "tenant-2", "viewer")
	if err != nil {
		t.Fatalf("IssueRefreshToken() error = %v", err)
	}

	claims, err := m.VerifyRefreshToken(token)
	if err != nil {
		t.Fatalf("VerifyRefreshToken() error = %v", err)
	}
	if claims.TokenUse != "refresh" {
		t.Fatalf("expected token_use=refresh, got %q", claims.TokenUse)
	}
}

func TestVerifyAccessToken_RejectsRefreshToken(t *testing.T) {
	m := newTestManager()

	refreshToken, _, err := m.IssueRefreshToken("user-3", "tenant-3", "developer")
	if err != nil {
		t.Fatalf("IssueRefreshToken() error = %v", err)
	}

	// A refresh token must never be usable as an access token, even though
	// it's structurally a valid JWT — token_use must be checked.
	if _, err := m.VerifyAccessToken(refreshToken); err == nil {
		t.Fatal("expected VerifyAccessToken to reject a refresh token, got nil error")
	}
}

func TestVerifyAccessToken_RejectsTamperedSignature(t *testing.T) {
	m := newTestManager()

	token, _, err := m.IssueAccessToken("user-4", "tenant-4", "admin")
	if err != nil {
		t.Fatalf("IssueAccessToken() error = %v", err)
	}

	tampered := token + "tampered"
	if _, err := m.VerifyAccessToken(tampered); err == nil {
		t.Fatal("expected VerifyAccessToken to reject a tampered token, got nil error")
	}
}

func TestVerifyAccessToken_RejectsWrongSecret(t *testing.T) {
	issuer := newTestManager()
	token, _, err := issuer.IssueAccessToken("user-5", "tenant-5", "admin")
	if err != nil {
		t.Fatalf("IssueAccessToken() error = %v", err)
	}

	verifier := NewManager("different-secret", "different-refresh-secret", 15*time.Minute, 7*24*time.Hour, "bridgecore-test")
	if _, err := verifier.VerifyAccessToken(token); err == nil {
		t.Fatal("expected VerifyAccessToken to reject a token signed with a different secret")
	}
}

func TestVerifyAccessToken_RejectsExpiredToken(t *testing.T) {
	m := NewManager("test-access-secret", "test-refresh-secret", -1*time.Minute, 7*24*time.Hour, "bridgecore-test")

	token, _, err := m.IssueAccessToken("user-6", "tenant-6", "admin")
	if err != nil {
		t.Fatalf("IssueAccessToken() error = %v", err)
	}

	if _, err := m.VerifyAccessToken(token); err == nil {
		t.Fatal("expected VerifyAccessToken to reject an already-expired token")
	}
}
