package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/repository"
	"github.com/bridgecore/bridgecore/pkg/jwt"
	"github.com/bridgecore/bridgecore/pkg/utils"
)

var (
	ErrInvalidCredentials = errors.New("service: invalid email or password")
	ErrEmailTaken         = errors.New("service: email already registered")
	ErrTenantSlugTaken    = errors.New("service: tenant slug already taken")
	ErrAccountInactive    = errors.New("service: account is inactive")
	ErrInvalidRefresh     = errors.New("service: invalid or expired refresh token")
)

// UserStore is the subset of user persistence operations AuthService
// depends on. Satisfied by *repository.UserRepository in production and by
// an in-memory fake in tests.
type UserStore interface {
	Create(ctx context.Context, u *models.User) error
	GetByID(ctx context.Context, id string) (*models.User, error)
	GetByEmail(ctx context.Context, email string) (*models.User, error)
	UpdateLastLogin(ctx context.Context, id string) error
}

// TenantStore is the subset of tenant persistence operations AuthService
// depends on.
type TenantStore interface {
	Create(ctx context.Context, t *models.Tenant) error
	GetBySlug(ctx context.Context, slug string) (*models.Tenant, error)
}

// RefreshTokenStore is the subset of refresh-token persistence operations
// AuthService depends on.
type RefreshTokenStore interface {
	Create(ctx context.Context, t *models.RefreshToken) error
	ListActiveByUser(ctx context.Context, userID string) ([]*models.RefreshToken, error)
	Revoke(ctx context.Context, id string) error
	RevokeAllForUser(ctx context.Context, userID string) error
}

// AuthService implements signup, login, token refresh, and logout.
type AuthService struct {
	users         UserStore
	tenants       TenantStore
	refreshTokens RefreshTokenStore
	jwtManager    *jwt.Manager
}

func NewAuthService(
	users UserStore,
	tenants TenantStore,
	refreshTokens RefreshTokenStore,
	jwtManager *jwt.Manager,
) *AuthService {
	return &AuthService{
		users:         users,
		tenants:       tenants,
		refreshTokens: refreshTokens,
		jwtManager:    jwtManager,
	}
}

// SignupInput carries the fields needed to provision a brand-new tenant and
// its first (admin) user in a single call. BridgeCore signup always creates
// a fresh tenant — joining an existing tenant is a separate, invite-based
// flow that is out of scope for this platform's core surface.
type SignupInput struct {
	TenantName string
	TenantSlug string
	Email      string
	Password   string
	FirstName  string
	LastName   string
}

// TokenPair is the access/refresh token pair returned by login and refresh.
type TokenPair struct {
	AccessToken           string
	AccessTokenExpiresAt  time.Time
	RefreshToken          string
	RefreshTokenExpiresAt time.Time
}

// Signup creates a new tenant and its first admin user, then issues tokens.
func (s *AuthService) Signup(ctx context.Context, in SignupInput) (*models.User, *models.Tenant, *TokenPair, error) {
	if _, err := s.tenants.GetBySlug(ctx, in.TenantSlug); err == nil {
		return nil, nil, nil, ErrTenantSlugTaken
	} else if !errors.Is(err, repository.ErrNotFound) {
		return nil, nil, nil, fmt.Errorf("auth: check tenant slug: %w", err)
	}

	if _, err := s.users.GetByEmail(ctx, in.Email); err == nil {
		return nil, nil, nil, ErrEmailTaken
	} else if !errors.Is(err, repository.ErrNotFound) {
		return nil, nil, nil, fmt.Errorf("auth: check email: %w", err)
	}

	tenant := &models.Tenant{
		Name:     in.TenantName,
		Slug:     in.TenantSlug,
		Plan:     models.PlanFree,
		IsActive: true,
	}
	if err := s.tenants.Create(ctx, tenant); err != nil {
		return nil, nil, nil, fmt.Errorf("auth: create tenant: %w", err)
	}

	hash, err := utils.HashPassword(in.Password)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("auth: hash password: %w", err)
	}

	user := &models.User{
		TenantID:     tenant.ID,
		Email:        in.Email,
		PasswordHash: hash,
		FirstName:    in.FirstName,
		LastName:     in.LastName,
		Role:         models.RoleAdmin, // first user of a new tenant is always admin
		IsActive:     true,
	}
	if err := s.users.Create(ctx, user); err != nil {
		return nil, nil, nil, fmt.Errorf("auth: create user: %w", err)
	}

	pair, err := s.issueTokenPair(ctx, user)
	if err != nil {
		return nil, nil, nil, err
	}

	return user, tenant, pair, nil
}

// Login authenticates a user by email/password and issues a token pair.
func (s *AuthService) Login(ctx context.Context, email, password string) (*models.User, *TokenPair, error) {
	user, err := s.users.GetByEmail(ctx, email)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, nil, fmt.Errorf("auth: lookup user: %w", err)
	}

	if !utils.CheckPassword(user.PasswordHash, password) {
		return nil, nil, ErrInvalidCredentials
	}
	if !user.IsActive {
		return nil, nil, ErrAccountInactive
	}

	_ = s.users.UpdateLastLogin(ctx, user.ID)

	pair, err := s.issueTokenPair(ctx, user)
	if err != nil {
		return nil, nil, err
	}
	return user, pair, nil
}

// Refresh validates a presented refresh token and issues a brand-new token
// pair, revoking the old refresh token (rotation) to limit replay exposure.
func (s *AuthService) Refresh(ctx context.Context, refreshToken string) (*TokenPair, error) {
	claims, err := s.jwtManager.VerifyRefreshToken(refreshToken)
	if err != nil {
		return nil, ErrInvalidRefresh
	}

	candidates, err := s.refreshTokens.ListActiveByUser(ctx, claims.UserID)
	if err != nil {
		return nil, fmt.Errorf("auth: list refresh tokens: %w", err)
	}

	var matched *models.RefreshToken
	for _, c := range candidates {
		if utils.CheckToken(c.TokenHash, refreshToken) {
			matched = c
			break
		}
	}
	if matched == nil {
		return nil, ErrInvalidRefresh
	}

	user, err := s.users.GetByID(ctx, claims.UserID)
	if err != nil {
		return nil, ErrInvalidRefresh
	}
	if !user.IsActive {
		return nil, ErrAccountInactive
	}

	// Rotate: revoke the presented refresh token before issuing a new pair.
	_ = s.refreshTokens.Revoke(ctx, matched.ID)

	return s.issueTokenPair(ctx, user)
}

// Logout revokes all active refresh tokens for a user, ending every session.
func (s *AuthService) Logout(ctx context.Context, userID string) error {
	return s.refreshTokens.RevokeAllForUser(ctx, userID)
}

func (s *AuthService) CurrentUser(ctx context.Context, userID string) (*models.User, error) {
	return s.users.GetByID(ctx, userID)
}

func (s *AuthService) issueTokenPair(ctx context.Context, user *models.User) (*TokenPair, error) {
	accessToken, accessExpiry, err := s.jwtManager.IssueAccessToken(user.ID, user.TenantID, string(user.Role))
	if err != nil {
		return nil, fmt.Errorf("auth: issue access token: %w", err)
	}

	refreshToken, refreshExpiry, err := s.jwtManager.IssueRefreshToken(user.ID, user.TenantID, string(user.Role))
	if err != nil {
		return nil, fmt.Errorf("auth: issue refresh token: %w", err)
	}

	refreshHash, err := utils.HashToken(refreshToken)
	if err != nil {
		return nil, fmt.Errorf("auth: hash refresh token: %w", err)
	}

	if err := s.refreshTokens.Create(ctx, &models.RefreshToken{
		UserID:    user.ID,
		TokenHash: refreshHash,
		ExpiresAt: refreshExpiry,
	}); err != nil {
		return nil, fmt.Errorf("auth: persist refresh token: %w", err)
	}

	return &TokenPair{
		AccessToken:           accessToken,
		AccessTokenExpiresAt:  accessExpiry,
		RefreshToken:          refreshToken,
		RefreshTokenExpiresAt: refreshExpiry,
	}, nil
}
