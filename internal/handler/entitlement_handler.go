package handler

import (
	"net/http"

	"github.com/bridgecore/bridgecore/internal/middleware"
	"github.com/bridgecore/bridgecore/internal/service"
	"github.com/bridgecore/bridgecore/pkg/response"
)

// EntitlementHandler exposes the feature catalog and per-tenant
// entitlement queries/grants.
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
		response.InternalError(w, "failed to list features")
		return
	}
	response.OK(w, "feature catalog retrieved", features)
}

// ListMine godoc
// @Summary      List features enabled for the current tenant
// @Tags         entitlements
// @Security     BearerAuth
// @Produce      json
// @Success      200 {object} response.Envelope
// @Router       /api/v1/features/mine [get]
func (h *EntitlementHandler) ListMine(w http.ResponseWriter, r *http.Request) {
	ac, _ := middleware.AuthFromContext(r.Context())

	features, err := h.entitlements.ListEnabledFeatures(r.Context(), ac.TenantID, ac.TenantPlan)
	if err != nil {
		response.InternalError(w, "failed to resolve entitlements")
		return
	}
	response.OK(w, "tenant entitlements retrieved", map[string]any{
		"tenant_id": ac.TenantID,
		"plan":      ac.TenantPlan,
		"features":  features,
	})
}

type grantFeatureRequest struct {
	TenantID   string `json:"tenant_id"`
	FeatureKey string `json:"feature_key"`
	Enabled    bool   `json:"enabled"`
}

// Grant godoc
// @Summary      Grant or revoke a feature for a tenant (admin operation)
// @Tags         entitlements
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        body body grantFeatureRequest true "Grant payload"
// @Success      200 {object} response.Envelope
// @Router       /api/v1/features/grant [post]
func (h *EntitlementHandler) Grant(w http.ResponseWriter, r *http.Request) {
	var req grantFeatureRequest
	if err := decodeJSON(r, &req); err != nil || req.TenantID == "" || req.FeatureKey == "" {
		response.BadRequest(w, "tenant_id and feature_key are required", nil)
		return
	}

	if err := h.entitlements.GrantFeature(r.Context(), req.TenantID, req.FeatureKey, req.Enabled); err != nil {
		response.InternalError(w, "failed to grant feature: "+err.Error())
		return
	}

	response.OK(w, "feature entitlement updated", req)
}
