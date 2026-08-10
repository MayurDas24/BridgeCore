package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/repository"
	"github.com/bridgecore/bridgecore/pkg/utils"
)

var (
	ErrAPIKeyNotFound = errors.New("service: api key not found")
	ErrAPIKeyInvalid  = errors.New("service: api key invalid or revoked")
)

// APIKeyService implements generation, rotation, deactivation, and
// authentication of tenant-scoped API keys.
type APIKeyService struct {
	repo   *repository.APIKeyRepository
	prefix string
}

func NewAPIKeyService(repo *repository.APIKeyRepository, prefix string) *APIKeyService {
	return &APIKeyService{repo: repo, prefix: prefix}
}

// Generate creates a brand-new API key for a tenant. The full plaintext key
// is returned exactly once — callers must persist it themselves; BridgeCore
// only ever stores the bcrypt hash from this point forward.
func (s *APIKeyService) Generate(ctx context.Context, tenantID string, createdBy *string, name string) (plaintext string, key *models.APIKey, err error) {
	fullKey, lastFour, err := utils.GenerateAPIKey(s.prefix)
	if err != nil {
		return "", nil, fmt.Errorf("apikey: generate secret: %w", err)
	}

	hash, err := utils.HashToken(fullKey)
	if err != nil {
		return "", nil, fmt.Errorf("apikey: hash key: %w", err)
	}

	if name == "" {
		name = "default"
	}

	record := &models.APIKey{
		TenantID:  tenantID,
		CreatedBy: createdBy,
		Name:      name,
		Prefix:    s.prefix,
		KeyHash:   hash,
		LastFour:  lastFour,
		IsActive:  true,
	}
	if err := s.repo.Create(ctx, record); err != nil {
		return "", nil, fmt.Errorf("apikey: persist key: %w", err)
	}

	return fullKey, record, nil
}

// Rotate deactivates an existing key and issues a brand-new one in its
// place for the same tenant, in one logical operation.
func (s *APIKeyService) Rotate(ctx context.Context, tenantID, keyID string, createdBy *string) (plaintext string, key *models.APIKey, err error) {
	existing, err := s.repo.GetByID(ctx, keyID)
	if errors.Is(err, repository.ErrNotFound) {
		return "", nil, ErrAPIKeyNotFound
	}
	if err != nil {
		return "", nil, err
	}
	if existing.TenantID != tenantID {
		return "", nil, ErrAPIKeyNotFound
	}

	if err := s.repo.Deactivate(ctx, keyID); err != nil && !errors.Is(err, repository.ErrNotFound) {
		return "", nil, fmt.Errorf("apikey: deactivate old key: %w", err)
	}

	return s.Generate(ctx, tenantID, createdBy, existing.Name)
}

// Deactivate revokes an API key so it can no longer authenticate requests.
func (s *APIKeyService) Deactivate(ctx context.Context, tenantID, keyID string) error {
	existing, err := s.repo.GetByID(ctx, keyID)
	if errors.Is(err, repository.ErrNotFound) {
		return ErrAPIKeyNotFound
	}
	if err != nil {
		return err
	}
	if existing.TenantID != tenantID {
		return ErrAPIKeyNotFound
	}
	return s.repo.Deactivate(ctx, keyID)
}

func (s *APIKeyService) ListByTenant(ctx context.Context, tenantID string) ([]*models.APIKey, error) {
	return s.repo.ListByTenant(ctx, tenantID)
}

// Authenticate validates a presented plaintext API key and returns the
// matching record. It bcrypt-compares against every active key sharing the
// configured prefix; in production, prefixes could be sharded further
// (e.g. per-tenant prefixes) to bound this set, but for BridgeCore's scale
// this linear scan over active keys is fine.
func (s *APIKeyService) Authenticate(ctx context.Context, plaintext string) (*models.APIKey, error) {
	if len(plaintext) <= len(s.prefix) || plaintext[:len(s.prefix)] != s.prefix {
		return nil, ErrAPIKeyInvalid
	}

	candidates, err := s.repo.ListActiveByPrefix(ctx, s.prefix)
	if err != nil {
		return nil, fmt.Errorf("apikey: list candidates: %w", err)
	}

	for _, c := range candidates {
		if utils.CheckToken(c.KeyHash, plaintext) {
			_ = s.repo.TouchLastUsed(ctx, c.ID)
			return c, nil
		}
	}

	return nil, ErrAPIKeyInvalid
}
