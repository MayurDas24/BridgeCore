package handler

import (
	"net/http"

	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/service"
	"github.com/bridgecore/bridgecore/pkg/apierr"
	"github.com/bridgecore/bridgecore/pkg/response"
)

// PlatformHandler exposes the cross-tenant control plane: provisioning
// tenants, changing plans, and granting entitlements.
//
// Every route here is authenticated with the platform operator token, never
// with a customer's JWT. That is what makes the tenant-scoped API safe to
// reason about: there is no request a customer credential can make that
// reaches any of these methods, so tenant isolation does not depend on
// remembering a role check on each one.
type PlatformHandler struct {
	tenants      *service.TenantService
	entitlements *service.EntitlementService
	audit        *service.AuditService
}

func NewPlatformHandler(
	tenants *service.TenantService,
	entitlements *service.EntitlementService,
	audit *service.AuditService,
) *PlatformHandler {
	return &PlatformHandler{tenants: tenants, entitlements: entitlements, audit: audit}
}

type createTenantRequest struct {
	Name string      `json:"name"`
	Slug string      `json:"slug"`
	Plan models.Plan `json:"plan"`
}

// CreateTenant godoc
// @Summary      Provision a tenant (platform operators only)
// @Tags         platform
// @Accept       json
// @Produce      json
// @Param        X-Platform-Token header string true "Platform operator token"
// @Param        body body createTenantRequest true "Tenant payload"
// @Success      201 {object} response.Envelope
// @Router       /api/v1/platform/tenants [post]
func (h *PlatformHandler) CreateTenant(w http.ResponseWriter, r *http.Request) {
	var req createTenantRequest
	if err := decodeJSON(r, &req); err != nil {
		response.Fail(w, r, badBody(err))
		return
	}

	tenant, err := h.tenants.Create(r.Context(), service.CreateTenantInput{
		Name: req.Name, Slug: req.Slug, Plan: req.Plan,
	})
	if err != nil {
		response.Fail(w, r, mapTenantError(err))
		return
	}

	h.audit.Record(r.Context(), service.RecordInput{
		TenantID:  strPtrOrNil(tenant.ID),
		Event:     models.EventTenantCreated,
		Endpoint:  r.URL.Path,
		IPAddress: r.RemoteAddr,
		UserAgent: r.UserAgent(),
		Metadata:  map[string]any{"actor": "platform_operator", "plan": string(tenant.Plan)},
	})

	response.Created(w, "tenant created", tenant)
}

// ListTenants godoc
// @Summary      List every tenant on the platform (platform operators only)
// @Tags         platform
// @Produce      json
// @Param        X-Platform-Token header string true "Platform operator token"
// @Param        search query string false "Search by name or slug"
// @Success      200 {object} response.Envelope
// @Router       /api/v1/platform/tenants [get]
func (h *PlatformHandler) ListTenants(w http.ResponseWriter, r *http.Request) {
	search := r.URL.Query().Get("search")
	page, pageSize := paginationParams(r)

	tenants, total, err := h.tenants.List(r.Context(), search, page, pageSize)
	if err != nil {
		response.Fail(w, r, apierr.Internal("failed to list tenants").Wrap(err))
		return
	}

	response.OKWithRequest(w, r, "tenants retrieved", response.ListResponse{
		Items: tenants,
		Meta:  response.NewMeta(page, pageSize, total),
	})
}

// GetTenant godoc
// @Summary      Get any tenant by ID (platform operators only)
// @Tags         platform
// @Produce      json
// @Param        X-Platform-Token header string true "Platform operator token"
// @Param        id path string true "Tenant ID"
// @Success      200 {object} response.Envelope
// @Router       /api/v1/platform/tenants/{id} [get]
func (h *PlatformHandler) GetTenant(w http.ResponseWriter, r *http.Request) {
	tenant, err := h.tenants.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		response.Fail(w, r, mapTenantError(err))
		return
	}
	response.OKWithRequest(w, r, "tenant retrieved", tenant)
}

type updateTenantRequest struct {
	Name     *string      `json:"name"`
	Plan     *models.Plan `json:"plan"`
	IsActive *bool        `json:"is_active"`
}

// UpdateTenant godoc
// @Summary      Update any tenant, including its plan (platform operators only)
// @Tags         platform
// @Accept       json
// @Produce      json
// @Param        X-Platform-Token header string true "Platform operator token"
// @Param        id path string true "Tenant ID"
// @Param        body body updateTenantRequest true "Fields to update"
// @Success      200 {object} response.Envelope
// @Router       /api/v1/platform/tenants/{id} [put]
func (h *PlatformHandler) UpdateTenant(w http.ResponseWriter, r *http.Request) {
	var req updateTenantRequest
	if err := decodeJSON(r, &req); err != nil {
		response.Fail(w, r, badBody(err))
		return
	}

	id := r.PathValue("id")
	tenant, err := h.tenants.Update(r.Context(), id, service.UpdateTenantInput{
		Name: req.Name, Plan: req.Plan, IsActive: req.IsActive,
	})
	if err != nil {
		response.Fail(w, r, mapTenantError(err))
		return
	}

	h.audit.Record(r.Context(), service.RecordInput{
		TenantID:  strPtrOrNil(tenant.ID),
		Event:     models.EventTenantUpdated,
		Endpoint:  r.URL.Path,
		IPAddress: r.RemoteAddr,
		UserAgent: r.UserAgent(),
		Metadata:  map[string]any{"actor": "platform_operator", "plan": string(tenant.Plan)},
	})

	response.OKWithRequest(w, r, "tenant updated", tenant)
}

// DeleteTenant godoc
// @Summary      Soft-delete any tenant (platform operators only)
// @Tags         platform
// @Produce      json
// @Param        X-Platform-Token header string true "Platform operator token"
// @Param        id path string true "Tenant ID"
// @Success      200 {object} response.Envelope
// @Router       /api/v1/platform/tenants/{id} [delete]
func (h *PlatformHandler) DeleteTenant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if err := h.tenants.Delete(r.Context(), id); err != nil {
		response.Fail(w, r, mapTenantError(err))
		return
	}

	h.audit.Record(r.Context(), service.RecordInput{
		TenantID:  strPtrOrNil(id),
		Event:     models.EventTenantDeleted,
		Endpoint:  r.URL.Path,
		IPAddress: r.RemoteAddr,
		UserAgent: r.UserAgent(),
		Metadata:  map[string]any{"actor": "platform_operator"},
	})

	response.OKWithRequest(w, r, "tenant deleted", nil)
}

type grantFeatureRequest struct {
	TenantID   string `json:"tenant_id"`
	FeatureKey string `json:"feature_key"`
	Enabled    bool   `json:"enabled"`
}

// GrantFeature godoc
// @Summary      Grant or revoke a feature for any tenant (platform operators only)
// @Description  This is the endpoint that used to accept a tenant_id from any tenant admin, which allowed a tenant to grant itself Enterprise features. It now requires the platform operator token.
// @Tags         platform
// @Accept       json
// @Produce      json
// @Param        X-Platform-Token header string true "Platform operator token"
// @Param        body body grantFeatureRequest true "Grant payload"
// @Success      200 {object} response.Envelope
// @Router       /api/v1/platform/features/grant [post]
func (h *PlatformHandler) GrantFeature(w http.ResponseWriter, r *http.Request) {
	var req grantFeatureRequest
	if err := decodeJSON(r, &req); err != nil {
		response.Fail(w, r, badBody(err))
		return
	}
	if req.TenantID == "" || req.FeatureKey == "" {
		response.Fail(w, r, apierr.Validation("tenant_id and feature_key are required"))
		return
	}

	if err := h.entitlements.GrantFeature(r.Context(), req.TenantID, req.FeatureKey, req.Enabled); err != nil {
		response.Fail(w, r, err)
		return
	}

	event := models.EventFeatureGranted
	if !req.Enabled {
		event = models.EventFeatureRevoked
	}
	h.audit.Record(r.Context(), service.RecordInput{
		TenantID:  strPtrOrNil(req.TenantID),
		Event:     event,
		Endpoint:  r.URL.Path,
		IPAddress: r.RemoteAddr,
		UserAgent: r.UserAgent(),
		Metadata: map[string]any{
			"actor":       "platform_operator",
			"feature_key": req.FeatureKey,
			"enabled":     req.Enabled,
		},
	})

	response.OKWithRequest(w, r, "feature entitlement updated", req)
}

// mapTenantError converts the tenant service's sentinel errors into typed
// API errors. The sentinels are kept because the existing unit tests assert
// on them; the mapping lives here so the transport still speaks one error
// vocabulary.
func mapTenantError(err error) error {
	switch {
	case err == nil:
		return nil
	case errorsIs(err, service.ErrTenantNotFound):
		return apierr.NotFound("tenant not found")
	case errorsIs(err, service.ErrTenantSlugTaken):
		return apierr.Conflict("a tenant with this slug already exists")
	default:
		return err
	}
}
