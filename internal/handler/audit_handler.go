package handler

import (
	"errors"
	"net/http"

	"github.com/bridgecore/bridgecore/internal/middleware"
	"github.com/bridgecore/bridgecore/internal/repository"
	"github.com/bridgecore/bridgecore/internal/service"
	"github.com/bridgecore/bridgecore/pkg/apierr"
	"github.com/bridgecore/bridgecore/pkg/response"
)

// AuditHandler exposes read access to the tenant's audit trail. There is no
// write endpoint: audit records are only ever produced as a side effect of
// the action they describe, and the table itself rejects UPDATE and DELETE.
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
// @Param        page_size query int false "Page size (clamped to the configured maximum)"
// @Success      200 {object} response.Envelope
// @Router       /api/v1/audit [get]
func (h *AuditHandler) List(w http.ResponseWriter, r *http.Request) {
	scope := middleware.ScopeFromContext(r.Context())
	if !scope.Valid() {
		response.Fail(w, r, apierr.Unauthenticated("authentication is required"))
		return
	}

	page, pageSize := paginationParams(r)
	event := r.URL.Query().Get("event")

	logs, total, err := h.audit.List(r.Context(), scope.TenantID, event, page, pageSize)
	if err != nil {
		response.Fail(w, r, apierr.Internal("failed to list audit logs").Wrap(err))
		return
	}

	response.OKWithRequest(w, r, "audit logs retrieved", response.ListResponse{
		Items: logs,
		Meta:  response.NewMeta(page, pageSize, total),
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
	scope := middleware.ScopeFromContext(r.Context())
	if !scope.Valid() {
		response.Fail(w, r, apierr.Unauthenticated("authentication is required"))
		return
	}

	// Tenant isolation is in the query, not in a comparison afterwards: the
	// row is never loaded unless it belongs to the caller.
	entry, err := h.audit.GetForTenant(r.Context(), scope.TenantID, r.PathValue("id"))
	if errors.Is(err, repository.ErrNotFound) {
		response.Fail(w, r, apierr.NotFound("audit log entry not found"))
		return
	}
	if err != nil {
		response.Fail(w, r, apierr.Internal("failed to retrieve the audit log entry").Wrap(err))
		return
	}

	response.OKWithRequest(w, r, "audit log entry retrieved", entry)
}
