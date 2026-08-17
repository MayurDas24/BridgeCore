package handler

import (
	"net/http"

	"github.com/bridgecore/bridgecore/internal/middleware"
	"github.com/bridgecore/bridgecore/internal/service"
	"github.com/bridgecore/bridgecore/pkg/apierr"
	"github.com/bridgecore/bridgecore/pkg/response"
)

// EntitlementHandler exposes the feature catalog and the caller tenant's own
// entitlements. Granting is a platform operation and lives on
// PlatformHandler.
type EntitlementHandler struct {
	entitlements *service.EntitlementService
}

func NewEntitlementHandler(entitlements *service.EntitlementService) *EntitlementHandler {
	return &EntitlementHandler{entitlements: entitlements}
}

// ListCatalog godoc
// @Summary      List the full feature catalog
// @Tags         entitlements
// @Security     BearerAuth
// @Produce      json
// @Success      200 {object} response.Envelope
// @Router       /api/v1/features [get]
func (h *EntitlementHandler) ListCatalog(w http.ResponseWriter, r *http.Request) {
	features, err := h.entitlements.ListFeatureCatalog(r.Context())
	if err != nil {
		response.Fail(w, r, apierr.Internal("failed to list features").Wrap(err))
		return
	}
	response.OKWithRequest(w, r, "feature catalog retrieved", features)
}

// ListMine godoc
// @Summary      List features enabled for the current tenant
// @Description  The result is the union of the tenant's plan defaults and any explicit per-tenant grants.
// @Tags         entitlements
// @Security     BearerAuth
// @Produce      json
// @Success      200 {object} response.Envelope
// @Router       /api/v1/features/mine [get]
func (h *EntitlementHandler) ListMine(w http.ResponseWriter, r *http.Request) {
	ac, ok := middleware.AuthFromContext(r.Context())
	if !ok {
		response.Fail(w, r, apierr.Unauthenticated("authentication is required"))
		return
	}

	features, err := h.entitlements.ListEnabledFeatures(r.Context(), ac.TenantID, ac.TenantPlan)
	if err != nil {
		response.Fail(w, r, apierr.Internal("failed to resolve entitlements").Wrap(err))
		return
	}

	response.OKWithRequest(w, r, "tenant entitlements retrieved", map[string]any{
		"tenant_id": ac.TenantID,
		"plan":      ac.TenantPlan,
		"features":  features,
	})
}
