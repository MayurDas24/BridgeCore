package handler

import (
	"encoding/csv"
	"fmt"
	"net/http"

	"github.com/bridgecore/bridgecore/internal/middleware"
	"github.com/bridgecore/bridgecore/internal/service"
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
// @Param        page_size query int false "Page size"
// @Success      200 {object} response.Envelope
// @Router       /api/v1/usage [get]
func (h *UsageHandler) List(w http.ResponseWriter, r *http.Request) {
	ac, _ := middleware.AuthFromContext(r.Context())
	page, pageSize := paginationParams(r)
	endpoint := r.URL.Query().Get("endpoint")
	method := r.URL.Query().Get("method")
	from := parseTimeParam(r, "from")
	to := parseTimeParam(r, "to")

	logs, total, err := h.usage.List(r.Context(), ac.TenantID, endpoint, method, from, to, page, pageSize)
	if err != nil {
		response.InternalError(w, "failed to list usage records")
		return
	}

	response.OK(w, "usage records retrieved", response.ListResponse{
		Items: logs,
		Meta:  response.Meta{Page: page, PageSize: pageSize, TotalCount: total, TotalPages: totalPages(total, pageSize)},
	})
}

// Summary godoc
// @Summary      Get aggregated usage summary per endpoint for the current tenant
// @Tags         usage
// @Security     BearerAuth
// @Produce      json
// @Param        from query string false "RFC3339 start time"
// @Param        to query string false "RFC3339 end time"
// @Success      200 {object} response.Envelope
// @Router       /api/v1/usage/summary [get]
func (h *UsageHandler) Summary(w http.ResponseWriter, r *http.Request) {
	ac, _ := middleware.AuthFromContext(r.Context())
	from := parseTimeParam(r, "from")
	to := parseTimeParam(r, "to")

	summary, err := h.usage.Summary(r.Context(), ac.TenantID, from, to)
	if err != nil {
		response.InternalError(w, "failed to summarize usage")
		return
	}

	response.OK(w, "usage summary retrieved", summary)
}

// Export godoc
// @Summary      Export usage records as CSV (requires the "usage.export" feature entitlement — Pro/Enterprise plans)
// @Description  This endpoint is gated by the RequireFeature middleware, which checks tenant entitlement BEFORE the handler runs. Free-plan tenants receive a 403.
// @Tags         usage
// @Security     BearerAuth
// @Produce      text/csv
// @Success      200 {string} string "CSV file"
// @Failure      403 {object} response.Envelope
// @Router       /api/v1/usage/export [get]
func (h *UsageHandler) Export(w http.ResponseWriter, r *http.Request) {
	ac, _ := middleware.AuthFromContext(r.Context())
	from := parseTimeParam(r, "from")
	to := parseTimeParam(r, "to")

	logs, _, err := h.usage.List(r.Context(), ac.TenantID, "", "", from, to, 1, 10000)
	if err != nil {
		response.InternalError(w, "failed to export usage records")
		return
	}

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="usage-export.csv"`)
	writer := csv.NewWriter(w)
	_ = writer.Write([]string{"id", "endpoint", "method", "status_code", "latency_ms", "request_id", "created_at"})
	for _, l := range logs {
		_ = writer.Write([]string{
			l.ID, l.Endpoint, l.Method,
			fmt.Sprintf("%d", l.StatusCode), fmt.Sprintf("%d", l.LatencyMS),
			l.RequestID, l.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	writer.Flush()
}
