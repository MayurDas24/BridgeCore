package handler

import (
	"errors"
	"net/http"

	"github.com/bridgecore/bridgecore/internal/middleware"
	"github.com/bridgecore/bridgecore/internal/repository"
	"github.com/bridgecore/bridgecore/internal/service"
	"github.com/bridgecore/bridgecore/pkg/response"
)

// AuditHandler exposes read access to the tenant's audit trail.
type AuditHandler struct {
	audit *service.AuditService
}

func NewAuditHandler(audit *service.AuditService) *AuditHandler {
	return &AuditHandler{audit: audit}
}

// List godoc
// @Summary      List audit log entries for the current tenant
// @Tags         audit
// @Security     BearerAuth
// @Produce      json
// @Param        event query string false "Filter by exact event name"
// @Param        page query int false "Page number"
// @Param        page_size query int false "Page size"
// @Success      200 {object} response.Envelope
// @Router       /api/v1/audit [get]
func (h *AuditHandler) List(w http.ResponseWriter, r *http.Request) {
	ac, _ := middleware.AuthFromContext(r.Context())
	page, pageSize := paginationParams(r)
	event := r.URL.Query().Get("event")

	logs, total, err := h.audit.List(r.Context(), ac.TenantID, event, page, pageSize)
	if err != nil {
		response.InternalError(w, "failed to list audit logs")
		return
	}

	response.OK(w, "audit logs retrieved", response.ListResponse{
		Items: logs,
		Meta:  response.Meta{Page: page, PageSize: pageSize, TotalCount: total, TotalPages: totalPages(total, pageSize)},
	})
}

// Get godoc
// @Summary      Get a single audit log entry by ID
// @Tags         audit
// @Security     BearerAuth
// @Produce      json
// @Param        id path string true "Audit Log ID"
// @Success      200 {object} response.Envelope
// @Failure      404 {object} response.Envelope
// @Router       /api/v1/audit/{id} [get]
func (h *AuditHandler) Get(w http.ResponseWriter, r *http.Request) {
	ac, _ := middleware.AuthFromContext(r.Context())
	id := r.PathValue("id")

	entry, err := h.audit.Get(r.Context(), id)
	if errors.Is(err, repository.ErrNotFound) {
		response.NotFound(w, "audit log entry not found")
		return
	}
	if err != nil {
		response.InternalError(w, "failed to retrieve audit log entry")
		return
	}

	// Tenant isolation: never let one tenant read another tenant's audit trail.
	if entry.TenantID == nil || *entry.TenantID != ac.TenantID {
		response.NotFound(w, "audit log entry not found")
		return
	}

	response.OK(w, "audit log entry retrieved", entry)
}
