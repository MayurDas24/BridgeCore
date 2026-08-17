package handler

import (
	"net/http"

	"github.com/bridgecore/bridgecore/internal/middleware"
	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/service"
	"github.com/bridgecore/bridgecore/internal/tenancy"
	"github.com/bridgecore/bridgecore/pkg/response"
)

// TenantHandler exposes the tenant-scoped tenant endpoints: a caller can
// read and update its own tenant, and nothing else.
//
// Cross-tenant tenant management (provisioning, plan changes, deletion)
// lives on PlatformHandler behind a separate operator credential. Before
// this split, GET/PUT/DELETE /tenants/{id} accepted any tenant ID from any
// authenticated admin, which let one customer read and modify another
// customer's tenant record.
type TenantHandler struct {
	tenants *service.TenantService
	audit   *service.AuditService
}

func NewTenantHandler(tenants *service.TenantService, audit *service.AuditService) *TenantHandler {
	return &TenantHandler{tenants: tenants, audit: audit}
}

// Current godoc
// @Summary      Get the authenticated caller's tenant
// @Tags         tenants
// @Security     BearerAuth
// @Produce      json
// @Success      200 {object} response.Envelope
// @Router       /api/v1/tenant [get]
func (h *TenantHandler) Current(w http.ResponseWriter, r *http.Request) {
	scope := middleware.ScopeFromContext(r.Context())

	tenant, err := h.tenants.Current(r.Context(), scope)
	if err != nil {
		response.Fail(w, r, err)
		return
	}
	response.OKWithRequest(w, r, "tenant retrieved", tenant)
}

// Get godoc
// @Summary      Get a tenant by ID (only the caller's own tenant resolves)
// @Description  Any ID other than the caller's own returns 404, so this endpoint cannot be used to discover which tenant IDs exist.
// @Tags         tenants
// @Security     BearerAuth
// @Produce      json
// @Param        id path string true "Tenant ID"
// @Success      200 {object} response.Envelope
// @Failure      404 {object} response.Envelope
// @Router       /api/v1/tenants/{id} [get]
func (h *TenantHandler) Get(w http.ResponseWriter, r *http.Request) {
	scope := middleware.ScopeFromContext(r.Context())
	id := r.PathValue("id")

	tenant, err := h.tenants.GetForScope(r.Context(), scope, id)
	if err != nil {
		h.auditIfCrossTenant(r, scope, id, err)
		response.Fail(w, r, err)
		return
	}
	response.OKWithRequest(w, r, "tenant retrieved", tenant)
}

// List godoc
// @Summary      List tenants visible to the caller (always exactly its own)
// @Tags         tenants
// @Security     BearerAuth
// @Produce      json
// @Success      200 {object} response.Envelope
// @Router       /api/v1/tenants [get]
func (h *TenantHandler) List(w http.ResponseWriter, r *http.Request) {
	scope := middleware.ScopeFromContext(r.Context())

	tenants, total, err := h.tenants.ListForScope(r.Context(), scope)
	if err != nil {
		response.Fail(w, r, err)
		return
	}

	response.OKWithRequest(w, r, "tenants retrieved", response.ListResponse{
		Items: tenants,
		Meta:  response.NewMeta(1, len(tenants), total),
	})
}

type updateTenantSelfRequest struct {
	Name *string `json:"name"`
}

// Update godoc
// @Summary      Update the caller's own tenant profile
// @Description  Only the display name is mutable here. Plan changes are a billing operation on the platform control plane, because a tenant that could set its own plan could grant itself any feature entitlement.
// @Tags         tenants
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        body body updateTenantSelfRequest true "Fields to update"
// @Success      200 {object} response.Envelope
// @Router       /api/v1/tenant [patch]
func (h *TenantHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req updateTenantSelfRequest
	if err := decodeJSON(r, &req); err != nil {
		response.Fail(w, r, badBody(err))
		return
	}

	scope := middleware.ScopeFromContext(r.Context())

	tenant, err := h.tenants.UpdateForScope(r.Context(), scope, scope.TenantID, service.UpdateTenantSelfInput{
		Name: req.Name,
	})
	if err != nil {
		response.Fail(w, r, err)
		return
	}

	h.audit.Record(r.Context(), service.RecordInput{
		TenantID:  strPtrOrNil(tenant.ID),
		ActorID:   strPtrOrNil(scope.UserID),
		Event:     models.EventTenantUpdated,
		Endpoint:  r.URL.Path,
		IPAddress: r.RemoteAddr,
		UserAgent: r.UserAgent(),
	})

	response.OKWithRequest(w, r, "tenant updated", tenant)
}

func (h *TenantHandler) auditIfCrossTenant(r *http.Request, scope tenancy.Scope, requestedID string, err error) {
	if !tenancy.IsCrossTenant(err) {
		return
	}
	h.audit.Record(r.Context(), service.RecordInput{
		TenantID:  strPtrOrNil(scope.TenantID),
		ActorID:   strPtrOrNil(scope.UserID),
		Event:     models.EventCrossTenantDenied,
		Endpoint:  r.URL.Path,
		IPAddress: r.RemoteAddr,
		UserAgent: r.UserAgent(),
		Metadata:  map[string]any{"resource": "tenant", "requested_id": requestedID},
	})
}
