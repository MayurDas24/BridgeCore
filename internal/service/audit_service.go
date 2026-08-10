package service

import (
	"context"

	"go.uber.org/zap"

	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/repository"
)

// AuditService records and queries the platform's audit trail. It is
// deliberately dependency-light and non-blocking-safe: callers fire audit
// events inline in request handlers/middleware, and failures to write an
// audit record are logged rather than propagated, since a broken audit
// pipeline must never take down the primary request path.
type AuditService struct {
	repo *repository.AuditRepository
	log  *zap.Logger
}

func NewAuditService(repo *repository.AuditRepository, log *zap.Logger) *AuditService {
	return &AuditService{repo: repo, log: log}
}

// RecordInput describes one audit event to persist.
type RecordInput struct {
	TenantID  *string
	ActorID   *string
	Event     string
	Metadata  map[string]any
	Endpoint  string
	IPAddress string
	UserAgent string
}

// Record persists an audit event. Errors are logged, not returned, by
// design — see the package doc comment above.
func (s *AuditService) Record(ctx context.Context, in RecordInput) {
	metadata := in.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}

	entry := &models.AuditLog{
		TenantID:  in.TenantID,
		ActorID:   in.ActorID,
		Event:     in.Event,
		Metadata:  metadata,
		Endpoint:  in.Endpoint,
		IPAddress: in.IPAddress,
		UserAgent: in.UserAgent,
	}

	if err := s.repo.Create(ctx, entry); err != nil {
		s.log.Error("failed to write audit log", zap.Error(err), zap.String("event", in.Event))
	}
}

func (s *AuditService) Get(ctx context.Context, id string) (*models.AuditLog, error) {
	return s.repo.GetByID(ctx, id)
}

func (s *AuditService) List(ctx context.Context, tenantID, eventFilter string, page, pageSize int) ([]*models.AuditLog, int64, error) {
	return s.repo.ListByTenant(ctx, tenantID, eventFilter, page, pageSize)
}
