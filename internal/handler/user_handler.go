package handler

import (
	"net/http"

	"github.com/bridgecore/bridgecore/internal/middleware"
	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/service"
	"github.com/bridgecore/bridgecore/internal/tenancy"
	"github.com/bridgecore/bridgecore/pkg/response"
)

// UserHandler exposes user listing and RBAC role management within the
// caller's own tenant.
//
// It depends on *service.UserService rather than on the repository directly.
// That indirection is the point of the refactor: the rules about who may
// change whose role now live in one place that the GraphQL resolvers share,
// so this handler is reduced to decoding, delegating, and auditing.
type UserHandler struct {
	users *service.UserService
	audit *service.AuditService
}

func NewUserHandler(users *service.UserService, audit *service.AuditService) *UserHandler {
	return &UserHandler{users: users, audit: audit}
}

// List godoc
// @Summary      List users in the current tenant
// @Tags         users
// @Security     BearerAuth
// @Produce      json
// @Success      200 {object} response.Envelope
// @Router       /api/v1/users [get]
func (h *UserHandler) List(w http.ResponseWriter, r *http.Request) {
	scope := middleware.ScopeFromContext(r.Context())
	page, pageSize := paginationParams(r)

	users, total, err := h.users.List(r.Context(), scope, page, pageSize)
	if err != nil {
		response.Fail(w, r, err)
		return
	}

	response.OKWithRequest(w, r, "users retrieved", response.ListResponse{
		Items: users,
		Meta:  response.NewMeta(page, pageSize, total),
	})
}

// Get godoc
// @Summary      Get one user in the current tenant
// @Tags         users
// @Security     BearerAuth
// @Produce      json
// @Param        id path string true "User ID"
// @Success      200 {object} response.Envelope
// @Failure      404 {object} response.Envelope
// @Router       /api/v1/users/{id} [get]
func (h *UserHandler) Get(w http.ResponseWriter, r *http.Request) {
	scope := middleware.ScopeFromContext(r.Context())

	user, err := h.users.Get(r.Context(), scope, r.PathValue("id"))
	if err != nil {
		h.auditIfCrossTenant(r, scope, err)
		response.Fail(w, r, err)
		return
	}
	response.OKWithRequest(w, r, "user retrieved", user)
}

// Me godoc
// @Summary      Get the authenticated user
// @Tags         users
// @Security     BearerAuth
// @Produce      json
// @Success      200 {object} response.Envelope
// @Router       /api/v1/users/me [get]
func (h *UserHandler) Me(w http.ResponseWriter, r *http.Request) {
	scope := middleware.ScopeFromContext(r.Context())

	user, err := h.users.Me(r.Context(), scope)
	if err != nil {
		response.Fail(w, r, err)
		return
	}
	response.OKWithRequest(w, r, "current user", user)
}

type updateRoleRequest struct {
	Role models.Role `json:"role"`
}

// UpdateRole godoc
// @Summary      Change a user's RBAC role
// @Tags         users
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        id path string true "User ID"
// @Param        body body updateRoleRequest true "New role"
// @Success      200 {object} response.Envelope
// @Failure      403 {object} response.Envelope
// @Failure      409 {object} response.Envelope
// @Router       /api/v1/users/{id}/role [patch]
func (h *UserHandler) UpdateRole(w http.ResponseWriter, r *http.Request) {
	var req updateRoleRequest
	if err := decodeJSON(r, &req); err != nil {
		response.Fail(w, r, badBody(err))
		return
	}

	scope := middleware.ScopeFromContext(r.Context())
	targetID := r.PathValue("id")

	result, err := h.users.UpdateRole(r.Context(), scope, targetID, req.Role)
	if err != nil {
		h.auditIfCrossTenant(r, scope, err)
		response.Fail(w, r, err)
		return
	}

	h.audit.Record(r.Context(), service.RecordInput{
		TenantID:  strPtrOrNil(scope.TenantID),
		ActorID:   strPtrOrNil(scope.UserID),
		Event:     models.EventRoleChanged,
		Endpoint:  r.URL.Path,
		IPAddress: r.RemoteAddr,
		UserAgent: r.UserAgent(),
		Metadata: map[string]any{
			"target_user_id": targetID,
			"previous_role":  string(result.PreviousRole),
			"new_role":       string(result.NewRole),
		},
	})

	response.OKWithRequest(w, r, "role updated", result.User)
}

type setUserActiveRequest struct {
	IsActive bool `json:"is_active"`
}

// SetActive godoc
// @Summary      Activate or deactivate a user in the current tenant
// @Tags         users
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        id path string true "User ID"
// @Param        body body setUserActiveRequest true "Desired status"
// @Success      200 {object} response.Envelope
// @Router       /api/v1/users/{id}/status [patch]
func (h *UserHandler) SetActive(w http.ResponseWriter, r *http.Request) {
	var req setUserActiveRequest
	if err := decodeJSON(r, &req); err != nil {
		response.Fail(w, r, badBody(err))
		return
	}

	scope := middleware.ScopeFromContext(r.Context())
	targetID := r.PathValue("id")

	user, err := h.users.SetActive(r.Context(), scope, targetID, req.IsActive)
	if err != nil {
		h.auditIfCrossTenant(r, scope, err)
		response.Fail(w, r, err)
		return
	}

	h.audit.Record(r.Context(), service.RecordInput{
		TenantID:  strPtrOrNil(scope.TenantID),
		ActorID:   strPtrOrNil(scope.UserID),
		Event:     models.EventRoleChanged,
		Endpoint:  r.URL.Path,
		IPAddress: r.RemoteAddr,
		UserAgent: r.UserAgent(),
		Metadata: map[string]any{
			"target_user_id": targetID,
			"is_active":      req.IsActive,
		},
	})

	response.OKWithRequest(w, r, "user status updated", user)
}

// auditIfCrossTenant records a blocked cross-tenant access attempt.
//
// The caller only ever sees a 404, so the attempt would otherwise be
// invisible. Recording it is what turns tenant isolation from a silent
// guarantee into something an operator can alarm on: a spike of these from
// one tenant is an enumeration attempt in progress.
func (h *UserHandler) auditIfCrossTenant(r *http.Request, scope tenancy.Scope, err error) {
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
		Metadata:  map[string]any{"resource": "user", "requested_id": r.PathValue("id")},
	})
}
