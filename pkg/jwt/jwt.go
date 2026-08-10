// Package jwt wraps golang-jwt/jwt/v5 with BridgeCore's specific claim
// shape and issuance rules for access and refresh tokens.
package jwt

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var (
	// ErrInvalidToken is returned for any token that fails parsing,
	// signature verification, or claim validation.
	ErrInvalidToken = errors.New("jwt: invalid or expired token")
)

// Claims is the BridgeCore-specific JWT payload. UserID and TenantID drive
// every downstream authorization decision, so both are always embedded
// directly in the token rather than requiring a DB lookup to resolve.
type Claims struct {
	UserID   string `json:"uid"`
	TenantID string `json:"tid"`
	Role     string `json:"role"`
	TokenUse string `json:"use"` // "access" or "refresh"
	jwt.RegisteredClaims
}

// Manager issues and verifies access/refresh token pairs.
type Manager struct {
	accessSecret  []byte
	refreshSecret []byte
	accessTTL     time.Duration
	refreshTTL    time.Duration
	issuer        string
}

// NewManager builds a token Manager from the resolved JWT configuration.
func NewManager(accessSecret, refreshSecret string, accessTTL, refreshTTL time.Duration, issuer string) *Manager {
	return &Manager{
		accessSecret:  []byte(accessSecret),
		refreshSecret: []byte(refreshSecret),
		accessTTL:     accessTTL,
		refreshTTL:    refreshTTL,
		issuer:        issuer,
	}
}

func (m *Manager) issue(userID, tenantID, role, use string, secret []byte, ttl time.Duration) (string, time.Time, error) {
	now := time.Now()
	expiresAt := now.Add(ttl)

	claims := Claims{
		UserID:   userID,
		TenantID: tenantID,
		Role:     role,
		TokenUse: use,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    m.issuer,
			Subject:   userID,
			ID:        uuid.NewString(), // jti: guarantees uniqueness even for two tokens issued within the same second
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			NotBefore: jwt.NewNumericDate(now),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(secret)
	if err != nil {
		return "", time.Time{}, err
	}
	return signed, expiresAt, nil
}

// IssueAccessToken mints a short-lived access token for the given user.
func (m *Manager) IssueAccessToken(userID, tenantID, role string) (string, time.Time, error) {
	return m.issue(userID, tenantID, role, "access", m.accessSecret, m.accessTTL)
}

// IssueRefreshToken mints a long-lived refresh token for the given user.
func (m *Manager) IssueRefreshToken(userID, tenantID, role string) (string, time.Time, error) {
	return m.issue(userID, tenantID, role, "refresh", m.refreshSecret, m.refreshTTL)
}

func (m *Manager) verify(tokenStr string, secret []byte, expectedUse string) (*Claims, error) {
	claims := &Claims{}

	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrInvalidToken
		}
		return secret, nil
	}, jwt.WithIssuer(m.issuer), jwt.WithExpirationRequired())

	if err != nil || !token.Valid {
		return nil, ErrInvalidToken
	}
	if claims.TokenUse != expectedUse {
		return nil, ErrInvalidToken
	}

	return claims, nil
}

// VerifyAccessToken validates an access token and returns its claims.
func (m *Manager) VerifyAccessToken(tokenStr string) (*Claims, error) {
	return m.verify(tokenStr, m.accessSecret, "access")
}

// VerifyRefreshToken validates a refresh token and returns its claims.
func (m *Manager) VerifyRefreshToken(tokenStr string) (*Claims, error) {
	return m.verify(tokenStr, m.refreshSecret, "refresh")
}
