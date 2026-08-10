package handler

import (
	"errors"
	"net/http"

	"github.com/bridgecore/bridgecore/internal/middleware"
	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/repository"
	"github.com/bridgecore/bridgecore/internal/service"
	"github.com/bridgecore/bridgecore/pkg/response"
)

// UserHandler exposes user listing and RBAC role management within the
// caller's own tenant.
type UserHandler struct {
	users *repository.UserRepository
	audit *service.AuditService
}

func NewUserHandler(users *repository.UserRepository, audit *service.AuditService) *UserHandler {
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
	ac, _ := middleware.AuthFromContext(r.Context())
	page, pageSize := paginationParams(r)

	users, total, err := h.users.ListByTenant(r.Context(), ac.TenantID, page, pageSize)
	if err != nil {
		response.InternalError(w, "failed to list users")
		return
	}

	response.OK(w, "users retrieved", response.ListResponse{
		Items: users,
		Meta:  response.Meta{Page: page, PageSize: pageSize, TotalCount: total, TotalPages: totalPages(total, pageSize)},
	})
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
// @Router       /api/v1/users/{id}/role [patch]
func (h *UserHandler) UpdateRole(w http.ResponseWriter, r *http.Request) {
	targetID := r.PathValue("id")

	var req updateRoleRequest
	if err := decodeJSON(r, &req); err != nil || !req.Role.Valid() {
		response.BadRequest(w, "a valid role (admin, developer, viewer) is required", nil)
		return
	}

	ac, _ := middleware.AuthFromContext(r.Context())

	target, err := h.users.GetByID(r.Context(), targetID)
	if errors.Is(err, repository.ErrNotFound) {
		response.NotFound(w, "user not found")
		return
	}
	if err != nil {
		response.InternalError(w, "failed to look up user")
		return
	}
	if target.TenantID != ac.TenantID {
		response.NotFound(w, "user not found")
		return
	}

	previousRole := target.Role
	if err := h.users.UpdateRole(r.Context(), targetID, req.Role); err != nil {
		response.InternalError(w, "failed to update role")
		return
	}

	h.audit.Record(r.Context(), service.RecordInput{
		TenantID:  &ac.TenantID,
		ActorID:   strPtrOrNil(ac.UserID),
		Event:     models.EventRoleChanged,
		Endpoint:  r.URL.Path,
		IPAddress: r.RemoteAddr,
		UserAgent: r.UserAgent(),
		Metadata: map[string]any{
			"target_user_id": targetID,
			"previous_role":  string(previousRole),
			"new_role":       string(req.Role),
		},
	})

	response.OK(w, "role updated", map[string]any{"user_id": targetID, "role": req.Role})
}
