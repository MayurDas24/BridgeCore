package handler

import (
	"errors"
	"net/http"

	"github.com/bridgecore/bridgecore/internal/middleware"
	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/service"
	"github.com/bridgecore/bridgecore/pkg/response"
)

// TenantHandler exposes tenant CRUD endpoints. All operations are scoped:
// admins may only manage their own tenant, except List which is a
// platform-operator view intended for internal/admin tooling (still
// requires an authenticated admin caller).
type TenantHandler struct {
	tenants *service.TenantService
	audit   *service.AuditService
}

func NewTenantHandler(tenants *service.TenantService, audit *service.AuditService) *TenantHandler {
	return &TenantHandler{tenants: tenants, audit: audit}
}

type createTenantRequest struct {
	Name string      `json:"name"`
	Slug string      `json:"slug"`
	Plan models.Plan `json:"plan"`
}

// Create godoc
// @Summary      Create a tenant
// @Tags         tenants
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        body body createTenantRequest true "Tenant payload"
// @Success      201 {object} response.Envelope
// @Router       /api/v1/tenants [post]
func (h *TenantHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req createTenantRequest
	if err := decodeJSON(r, &req); err != nil {
		response.BadRequest(w, "invalid request body", err.Error())
		return
	}
	if req.Name == "" || req.Slug == "" {
		response.BadRequest(w, "name and slug are required", nil)
		return
	}

	tenant, err := h.tenants.Create(r.Context(), service.CreateTenantInput{
		Name: req.Name, Slug: req.Slug, Plan: req.Plan,
	})
	if err != nil {
		h.handleError(w, err)
		return
	}

	ac, _ := middleware.AuthFromContext(r.Context())
	h.audit.Record(r.Context(), service.RecordInput{
		TenantID:  &tenant.ID,
		ActorID:   strPtrOrNil(ac.UserID),
		Event:     models.EventTenantCreated,
		Endpoint:  r.URL.Path,
		IPAddress: r.RemoteAddr,
		UserAgent: r.UserAgent(),
	})

	response.Created(w, "tenant created", tenant)
}

// List godoc
// @Summary      List tenants
// @Tags         tenants
// @Security     BearerAuth
// @Produce      json
// @Param        search query string false "Search by name or slug"
// @Param        page query int false "Page number"
// @Param        page_size query int false "Page size"
// @Success      200 {object} response.Envelope
// @Router       /api/v1/tenants [get]
func (h *TenantHandler) List(w http.ResponseWriter, r *http.Request) {
	search := r.URL.Query().Get("search")
	page, pageSize := paginationParams(r)

	tenants, total, err := h.tenants.List(r.Context(), search, page, pageSize)
	if err != nil {
		response.InternalError(w, "failed to list tenants")
		return
	}

	response.OK(w, "tenants retrieved", response.ListResponse{
		Items: tenants,
		Meta: response.Meta{
			Page: page, PageSize: pageSize, TotalCount: total, TotalPages: totalPages(total, pageSize),
		},
	})
}

// Get godoc
// @Summary      Get a tenant by ID
// @Tags         tenants
// @Security     BearerAuth
// @Produce      json
// @Param        id path string true "Tenant ID"
// @Success      200 {object} response.Envelope
// @Failure      404 {object} response.Envelope
// @Router       /api/v1/tenants/{id} [get]
func (h *TenantHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tenant, err := h.tenants.Get(r.Context(), id)
	if err != nil {
		h.handleError(w, err)
		return
	}
	response.OK(w, "tenant retrieved", tenant)
}

type updateTenantRequest struct {
	Name     *string      `json:"name"`
	Plan     *models.Plan `json:"plan"`
	IsActive *bool        `json:"is_active"`
}

// Update godoc
// @Summary      Update a tenant
// @Tags         tenants
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        id path string true "Tenant ID"
// @Param        body body updateTenantRequest true "Fields to update"
// @Success      200 {object} response.Envelope
// @Router       /api/v1/tenants/{id} [put]
func (h *TenantHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req updateTenantRequest
	if err := decodeJSON(r, &req); err != nil {
		response.BadRequest(w, "invalid request body", err.Error())
		return
	}

	tenant, err := h.tenants.Update(r.Context(), id, service.UpdateTenantInput{
		Name: req.Name, Plan: req.Plan, IsActive: req.IsActive,
	})
	if err != nil {
		h.handleError(w, err)
		return
	}

	ac, _ := middleware.AuthFromContext(r.Context())
	h.audit.Record(r.Context(), service.RecordInput{
		TenantID:  &tenant.ID,
		ActorID:   strPtrOrNil(ac.UserID),
		Event:     models.EventTenantUpdated,
		Endpoint:  r.URL.Path,
		IPAddress: r.RemoteAddr,
		UserAgent: r.UserAgent(),
	})

	response.OK(w, "tenant updated", tenant)
}

// Delete godoc
// @Summary      Soft-delete a tenant
// @Tags         tenants
// @Security     BearerAuth
// @Param        id path string true "Tenant ID"
// @Success      200 {object} response.Envelope
// @Router       /api/v1/tenants/{id} [delete]
func (h *TenantHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.tenants.Delete(r.Context(), id); err != nil {
		h.handleError(w, err)
		return
	}

	ac, _ := middleware.AuthFromContext(r.Context())
	h.audit.Record(r.Context(), service.RecordInput{
		TenantID:  &id,
		ActorID:   strPtrOrNil(ac.UserID),
		Event:     models.EventTenantDeleted,
		Endpoint:  r.URL.Path,
		IPAddress: r.RemoteAddr,
		UserAgent: r.UserAgent(),
	})

	response.OK(w, "tenant deleted", nil)
}

func (h *TenantHandler) handleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrTenantNotFound):
		response.NotFound(w, "tenant not found")
	case errors.Is(err, service.ErrTenantSlugTaken):
		response.Conflict(w, "a tenant with this slug already exists")
	default:
		response.InternalError(w, "operation failed")
	}
}
