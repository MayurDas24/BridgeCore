package handler

import (
	"net/http"

	"github.com/bridgecore/bridgecore/internal/middleware"
	"github.com/bridgecore/bridgecore/internal/service"
	"github.com/bridgecore/bridgecore/pkg/apierr"
	"github.com/bridgecore/bridgecore/pkg/response"
)

// UsageHandler exposes raw usage log queries and aggregated summaries,
// always scoped to the caller's own tenant.
type UsageHandler struct {
	usage *service.UsageService
}

func NewUsageHandler(usage *service.UsageService) *UsageHandler {
	return &UsageHandler{usage: usage}
}

// List godoc
// @Summary      List raw usage records for the current tenant
// @Tags         usage
// @Security     BearerAuth
// @Produce      json
// @Param        endpoint query string false "Filter by endpoint substring"
// @Param        method query string false "Filter by HTTP method"
// @Param        from query string false "RFC3339 start time"
// @Param        to query string false "RFC3339 end time"
// @Param        page query int false "Page number"
// @Param        page_size query int false "Page size (clamped to the configured maximum)"
// @Success      200 {object} response.Envelope
// @Router       /api/v1/usage [get]
func (h *UsageHandler) List(w http.ResponseWriter, r *http.Request) {
	scope := middleware.ScopeFromContext(r.Context())
	if !scope.Valid() {
		response.Fail(w, r, apierr.Unauthenticated("authentication is required"))
		return
	}

	page, pageSize := paginationParams(r)
	from, to, err := timeWindow(r)
	if err != nil {
		response.Fail(w, r, err)
		return
	}

	logs, total, err := h.usage.List(r.Context(), scope.TenantID,
		r.URL.Query().Get("endpoint"), r.URL.Query().Get("method"), from, to, page, pageSize)
	if err != nil {
		response.Fail(w, r, apierr.Internal("failed to list usage records").Wrap(err))
		return
	}

	response.OKWithRequest(w, r, "usage records retrieved", response.ListResponse{
		Items: logs,
		Meta:  response.NewMeta(page, pageSize, total),
	})
}

// Summary godoc
// @Summary      Aggregated usage per endpoint for the current tenant
// @Tags         usage
// @Security     BearerAuth
// @Produce      json
// @Param        from query string false "RFC3339 start time"
// @Param        to query string false "RFC3339 end time"
// @Success      200 {object} response.Envelope
// @Router       /api/v1/usage/summary [get]
func (h *UsageHandler) Summary(w http.ResponseWriter, r *http.Request) {
	scope := middleware.ScopeFromContext(r.Context())
	if !scope.Valid() {
		response.Fail(w, r, apierr.Unauthenticated("authentication is required"))
		return
	}

	from, to, err := timeWindow(r)
	if err != nil {
		response.Fail(w, r, err)
		return
	}

	summary, err := h.usage.Summary(r.Context(), scope.TenantID, from, to)
	if err != nil {
		response.Fail(w, r, apierr.Internal("failed to summarize usage").Wrap(err))
		return
	}

	response.OKWithRequest(w, r, "usage summary retrieved", summary)
}
