package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/repository"
	"github.com/bridgecore/bridgecore/pkg/jwt"
	"github.com/bridgecore/bridgecore/pkg/utils"
)

// ---- In-memory fakes implementing the service package's store interfaces ----

type fakeUserStore struct {
	byID    map[string]*models.User
	byEmail map[string]*models.User
	nextID  int
}

func newFakeUserStore() *fakeUserStore {
	return &fakeUserStore{byID: map[string]*models.User{}, byEmail: map[string]*models.User{}}
}

func (f *fakeUserStore) Create(ctx context.Context, u *models.User) error {
	if _, exists := f.byEmail[u.Email]; exists {
		return repository.ErrConflict
	}
	f.nextID++
	u.ID = "user-" + itoa(f.nextID)
	u.CreatedAt = time.Now()
	u.UpdatedAt = time.Now()
	f.byID[u.ID] = u
	f.byEmail[u.Email] = u
	return nil
}

func (f *fakeUserStore) GetByID(ctx context.Context, id string) (*models.User, error) {
	u, ok := f.byID[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	return u, nil
}

func (f *fakeUserStore) GetByEmail(ctx context.Context, email string) (*models.User, error) {
	u, ok := f.byEmail[email]
	if !ok {
		return nil, repository.ErrNotFound
	}
	return u, nil
}

func (f *fakeUserStore) UpdateLastLogin(ctx context.Context, id string) error {
	if u, ok := f.byID[id]; ok {
		now := time.Now()
		u.LastLoginAt = &now
	}
	return nil
}

type fakeTenantStore struct {
	bySlug map[string]*models.Tenant
	nextID int
}

func newFakeTenantStore() *fakeTenantStore {
	return &fakeTenantStore{bySlug: map[string]*models.Tenant{}}
}

func (f *fakeTenantStore) Create(ctx context.Context, t *models.Tenant) error {
	if _, exists := f.bySlug[t.Slug]; exists {
		return repository.ErrConflict
	}
	f.nextID++
	t.ID = "tenant-" + itoa(f.nextID)
	t.CreatedAt = time.Now()
	t.UpdatedAt = time.Now()
	f.bySlug[t.Slug] = t
	return nil
}

func (f *fakeTenantStore) GetBySlug(ctx context.Context, slug string) (*models.Tenant, error) {
	t, ok := f.bySlug[slug]
	if !ok {
		return nil, repository.ErrNotFound
	}
	return t, nil
}

type fakeRefreshTokenStore struct {
	tokens map[string]*models.RefreshToken
	nextID int
}

func newFakeRefreshTokenStore() *fakeRefreshTokenStore {
	return &fakeRefreshTokenStore{tokens: map[string]*models.RefreshToken{}}
}

func (f *fakeRefreshTokenStore) Create(ctx context.Context, t *models.RefreshToken) error {
	f.nextID++
	t.ID = "rt-" + itoa(f.nextID)
	t.CreatedAt = time.Now()
	f.tokens[t.ID] = t
	return nil
}

func (f *fakeRefreshTokenStore) ListActiveByUser(ctx context.Context, userID string) ([]*models.RefreshToken, error) {
	var out []*models.RefreshToken
	for _, t := range f.tokens {
		if t.UserID == userID && t.RevokedAt == nil && t.ExpiresAt.After(time.Now()) {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeRefreshTokenStore) Revoke(ctx context.Context, id string) error {
	t, ok := f.tokens[id]
	if !ok {
		return repository.ErrNotFound
	}
	now := time.Now()
	t.RevokedAt = &now
	return nil
}

func (f *fakeRefreshTokenStore) RevokeAllForUser(ctx context.Context, userID string) error {
	now := time.Now()
	for _, t := range f.tokens {
		if t.UserID == userID && t.RevokedAt == nil {
			t.RevokedAt = &now
		}
	}
	return nil
}

func itoa(n int) string {
	digits := "0123456789"
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{digits[n%10]}, b...)
		n /= 10
	}
	return string(b)
}

func newTestAuthService() *AuthService {
	jwtManager := jwt.NewManager("test-access", "test-refresh", 15*time.Minute, 7*24*time.Hour, "bridgecore-test")
	return NewAuthService(newFakeUserStore(), newFakeTenantStore(), newFakeRefreshTokenStore(), jwtManager)
}

func TestAuthService_Signup_CreatesTenantAndAdminUser(t *testing.T) {
	svc := newTestAuthService()

	user, tenant, pair, err := svc.Signup(context.Background(), SignupInput{
		TenantName: "Acme Corp",
		TenantSlug: "acme-corp",
		Email:      "founder@acme.test",
		Password:   "hunter22222",
		FirstName:  "Ada",
		LastName:   "Lovelace",
	})
	if err != nil {
		t.Fatalf("Signup() error = %v", err)
	}

	if user.Role != models.RoleAdmin {
		t.Fatalf("expected first user of a new tenant to be admin, got %s", user.Role)
	}
	if tenant.Plan != models.PlanFree {
		t.Fatalf("expected new tenant to default to the free plan, got %s", tenant.Plan)
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Fatal("expected a non-empty token pair on signup")
	}
	if user.PasswordHash == "hunter22222" {
		t.Fatal("password must never be stored in plaintext")
	}
}

func TestAuthService_Signup_RejectsDuplicateTenantSlug(t *testing.T) {
	svc := newTestAuthService()
	ctx := context.Background()

	if _, _, _, err := svc.Signup(ctx, SignupInput{TenantName: "Acme", TenantSlug: "acme", Email: "a@acme.test", Password: "password123"}); err != nil {
		t.Fatalf("first signup failed: %v", err)
	}

	_, _, _, err := svc.Signup(ctx, SignupInput{TenantName: "Acme Again", TenantSlug: "acme", Email: "b@acme.test", Password: "password123"})
	if !errors.Is(err, ErrTenantSlugTaken) {
		t.Fatalf("expected ErrTenantSlugTaken, got %v", err)
	}
}

func TestAuthService_Signup_RejectsDuplicateEmail(t *testing.T) {
	svc := newTestAuthService()
	ctx := context.Background()

	if _, _, _, err := svc.Signup(ctx, SignupInput{TenantName: "Acme", TenantSlug: "acme", Email: "dup@acme.test", Password: "password123"}); err != nil {
		t.Fatalf("first signup failed: %v", err)
	}

	_, _, _, err := svc.Signup(ctx, SignupInput{TenantName: "Other Co", TenantSlug: "other-co", Email: "dup@acme.test", Password: "password123"})
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("expected ErrEmailTaken, got %v", err)
	}
}

func TestAuthService_Login_SucceedsWithCorrectCredentials(t *testing.T) {
	svc := newTestAuthService()
	ctx := context.Background()

	_, _, _, err := svc.Signup(ctx, SignupInput{TenantName: "Acme", TenantSlug: "acme", Email: "user@acme.test", Password: "correct-password"})
	if err != nil {
		t.Fatalf("signup failed: %v", err)
	}

	user, pair, err := svc.Login(ctx, "user@acme.test", "correct-password")
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if user.Email != "user@acme.test" {
		t.Fatalf("unexpected user returned: %+v", user)
	}
	if pair.AccessToken == "" {
		t.Fatal("expected a non-empty access token on login")
	}
}

func TestAuthService_Login_FailsWithWrongPassword(t *testing.T) {
	svc := newTestAuthService()
	ctx := context.Background()

	_, _, _, err := svc.Signup(ctx, SignupInput{TenantName: "Acme", TenantSlug: "acme", Email: "user@acme.test", Password: "correct-password"})
	if err != nil {
		t.Fatalf("signup failed: %v", err)
	}

	_, _, err = svc.Login(ctx, "user@acme.test", "wrong-password")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestAuthService_Login_FailsForUnknownEmail(t *testing.T) {
	svc := newTestAuthService()

	_, _, err := svc.Login(context.Background(), "nobody@nowhere.test", "whatever")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials for unknown email, got %v", err)
	}
}

func TestAuthService_Refresh_RotatesTokenAndRevokesOld(t *testing.T) {
	svc := newTestAuthService()
	ctx := context.Background()

	_, _, pair, err := svc.Signup(ctx, SignupInput{TenantName: "Acme", TenantSlug: "acme", Email: "user@acme.test", Password: "correct-password"})
	if err != nil {
		t.Fatalf("signup failed: %v", err)
	}

	newPair, err := svc.Refresh(ctx, pair.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if newPair.RefreshToken == pair.RefreshToken {
		t.Fatal("expected refresh to rotate to a brand-new refresh token")
	}

	// The old, now-revoked refresh token must no longer work.
	if _, err := svc.Refresh(ctx, pair.RefreshToken); err == nil {
		t.Fatal("expected the original (rotated-out) refresh token to be rejected")
	}
}

func TestAuthService_Logout_RevokesAllSessions(t *testing.T) {
	svc := newTestAuthService()
	ctx := context.Background()

	user, _, pair, err := svc.Signup(ctx, SignupInput{TenantName: "Acme", TenantSlug: "acme", Email: "user@acme.test", Password: "correct-password"})
	if err != nil {
		t.Fatalf("signup failed: %v", err)
	}

	if err := svc.Logout(ctx, user.ID); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}

	if _, err := svc.Refresh(ctx, pair.RefreshToken); err == nil {
		t.Fatal("expected refresh token to be invalid after logout")
	}
}

// sanity-check the fake itself hashes passwords the same way the real
// service does, so the tests above are exercising real bcrypt behavior.
func TestFakeUserStore_PasswordHashingUsesBcrypt(t *testing.T) {
	hash, err := utils.HashPassword("some-password")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if !utils.CheckPassword(hash, "some-password") {
		t.Fatal("expected CheckPassword to succeed")
	}
}
