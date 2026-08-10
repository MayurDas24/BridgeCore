package service

import (
	"context"

	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/repository"
)

// PlanFeatureDefaults maps each subscription plan to the feature keys it
// grants by default. Tenants can additionally be granted features
// individually via tenant_features regardless of plan (e.g. a manually
// granted beta feature on a Free tenant).
var PlanFeatureDefaults = map[models.Plan][]string{
	models.PlanFree: {
		"usage.basic_dashboard",
	},
	models.PlanPro: {
		"usage.basic_dashboard",
		"usage.export",
		"apikeys.multiple",
		"audit.retention_30d",
	},
	models.PlanEnterprise: {
		"usage.basic_dashboard",
		"usage.export",
		"apikeys.multiple",
		"audit.retention_90d",
		"audit.export",
		"sso.saml",
		"support.priority",
	},
}

// EntitlementService resolves whether a tenant may use a given feature,
// combining its subscription plan's default feature set with any
// individually-granted overrides.
type EntitlementService struct {
	repo *repository.EntitlementRepository
}

func NewEntitlementService(repo *repository.EntitlementRepository) *EntitlementService {
	return &EntitlementService{repo: repo}
}

// HasFeature reports whether tenantPlan+tenantID together entitle the
// tenant to featureKey. An explicit tenant_features row (granted via the
// entitlement API) always takes precedence over the plan default table,
// which lets support/ops grant one-off features without changing plan.
func (s *EntitlementService) HasFeature(ctx context.Context, tenantID string, tenantPlan models.Plan, featureKey string) (bool, error) {
	granted, err := s.repo.TenantHasFeature(ctx, tenantID, featureKey)
	if err != nil {
		return false, err
	}
	if granted {
		return true, nil
	}

	for _, k := range PlanFeatureDefaults[tenantPlan] {
		if k == featureKey {
			return true, nil
		}
	}
	return false, nil
}

// ListEnabledFeatures returns the union of plan-default and explicitly
// granted feature keys for a tenant.
func (s *EntitlementService) ListEnabledFeatures(ctx context.Context, tenantID string, tenantPlan models.Plan) ([]string, error) {
	granted, err := s.repo.ListTenantFeatures(ctx, tenantID)
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var result []string
	for _, k := range PlanFeatureDefaults[tenantPlan] {
		if !seen[k] {
			seen[k] = true
			result = append(result, k)
		}
	}
	for _, k := range granted {
		if !seen[k] {
			seen[k] = true
			result = append(result, k)
		}
	}
	return result, nil
}

func (s *EntitlementService) ListFeatureCatalog(ctx context.Context) ([]*models.Feature, error) {
	return s.repo.ListFeatures(ctx)
}

func (s *EntitlementService) GrantFeature(ctx context.Context, tenantID, featureKey string, enabled bool) error {
	feature, err := s.repo.GetFeatureByKey(ctx, featureKey)
	if err != nil {
		return err
	}
	return s.repo.GrantFeature(ctx, tenantID, feature.ID, enabled)
}
