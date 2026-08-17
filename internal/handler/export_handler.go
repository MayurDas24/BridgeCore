package handler

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/bridgecore/bridgecore/internal/exports"
	"github.com/bridgecore/bridgecore/internal/middleware"
	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/service"
	"github.com/bridgecore/bridgecore/pkg/apierr"
	"github.com/bridgecore/bridgecore/pkg/response"
)

// ExportHandler exposes the asynchronous usage-export API.
//
// The old synchronous endpoint held a connection open while it paged through
// the entire usage table and streamed CSV into the response. That has three
// problems in production: a large tenant's export outlives the load
// balancer's idle timeout, a retry repeats all of the work, and the request
// pins an ECS task for its whole duration. Here the client gets a job ID
// immediately and polls it, and the result is a private object plus an
// expiring download URL.
type ExportHandler struct {
	exports *service.ExportService
	audit   *service.AuditService

	// local is set only when the object store is the filesystem backend, in
	// which case the API serves signed downloads itself. With S3 the client
	// fetches the presigned URL directly and the bytes never touch the API.
	local *exports.LocalStore
}

func NewExportHandler(svc *service.ExportService, audit *service.AuditService, local *exports.LocalStore) *ExportHandler {
	return &ExportHandler{exports: svc, audit: audit, local: local}
}

type createExportRequest struct {
	Endpoint string `json:"endpoint"`
	Method   string `json:"method"`
	From     string `json:"from"`
	To       string `json:"to"`
}

// Create godoc
// @Summary      Request an asynchronous usage export
// @Description  Requires the "usage.export" feature entitlement (Pro/Enterprise). Returns 202 with a job you can poll.
// @Tags         usage
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        body body createExportRequest false "Optional filters"
// @Success      202 {object} response.Envelope
// @Failure      403 {object} response.Envelope
// @Router       /api/v1/usage/exports [post]
func (h *ExportHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req createExportRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		response.Fail(w, r, badBody(err))
		return
	}

	from, err := parseRFC3339Field("from", req.From)
	if err != nil {
		response.Fail(w, r, err)
		return
	}
	to, err := parseRFC3339Field("to", req.To)
	if err != nil {
		response.Fail(w, r, err)
		return
	}

	scope := middleware.ScopeFromContext(r.Context())

	job, err := h.exports.Request(r.Context(), scope, service.RequestExportInput{
		Endpoint: req.Endpoint,
		Method:   req.Method,
		From:     from,
		To:       to,
	})
	if err != nil {
		response.Fail(w, r, err)
		return
	}

	h.audit.Record(r.Context(), service.RecordInput{
		TenantID:  strPtrOrNil(scope.TenantID),
		ActorID:   strPtrOrNil(scope.UserID),
		Event:     models.EventExportRequested,
		Endpoint:  r.URL.Path,
		IPAddress: r.RemoteAddr,
		UserAgent: r.UserAgent(),
		Metadata:  map[string]any{"export_job_id": job.ID},
	})

	response.Accepted(w, "export queued", map[string]any{
		"job":      job,
		"max_rows": h.exports.MaxRows(),
	})
}

// List godoc
// @Summary      List the current tenant's export jobs
// @Tags         usage
// @Security     BearerAuth
// @Produce      json
// @Success      200 {object} response.Envelope
// @Router       /api/v1/usage/exports [get]
func (h *ExportHandler) List(w http.ResponseWriter, r *http.Request) {
	scope := middleware.ScopeFromContext(r.Context())
	page, pageSize := paginationParams(r)

	jobs, total, err := h.exports.List(r.Context(), scope, page, pageSize)
	if err != nil {
		response.Fail(w, r, err)
		return
	}

	response.OKWithRequest(w, r, "export jobs retrieved", response.ListResponse{
		Items: jobs,
		Meta:  response.NewMeta(page, pageSize, total),
	})
}

// Get godoc
// @Summary      Get one export job, including its download URL when complete
// @Tags         usage
// @Security     BearerAuth
// @Produce      json
// @Param        id path string true "Export job ID"
// @Success      200 {object} response.Envelope
// @Failure      404 {object} response.Envelope
// @Router       /api/v1/usage/exports/{id} [get]
func (h *ExportHandler) Get(w http.ResponseWriter, r *http.Request) {
	scope := middleware.ScopeFromContext(r.Context())

	job, err := h.exports.Get(r.Context(), scope, r.PathValue("id"))
	if err != nil {
		response.Fail(w, r, err)
		return
	}

	payload := map[string]any{"job": job}

	// A completed job carries a freshly minted URL. It is generated per
	// request rather than stored, so it always expires and a replayed old
	// response is not a usable capability.
	if job.Status == models.ExportStatusCompleted {
		if download, err := h.exports.DownloadURL(r.Context(), scope, job.ID); err == nil {
			payload["download"] = download
		}
	}

	response.OKWithRequest(w, r, "export job retrieved", payload)
}

// DownloadURL godoc
// @Summary      Mint a short-lived download URL for a completed export
// @Tags         usage
// @Security     BearerAuth
// @Produce      json
// @Param        id path string true "Export job ID"
// @Success      200 {object} response.Envelope
// @Failure      409 {object} response.Envelope
// @Router       /api/v1/usage/exports/{id}/download [get]
func (h *ExportHandler) DownloadURL(w http.ResponseWriter, r *http.Request) {
	scope := middleware.ScopeFromContext(r.Context())
	id := r.PathValue("id")

	download, err := h.exports.DownloadURL(r.Context(), scope, id)
	if err != nil {
		response.Fail(w, r, err)
		return
	}

	h.audit.Record(r.Context(), service.RecordInput{
		TenantID:  strPtrOrNil(scope.TenantID),
		ActorID:   strPtrOrNil(scope.UserID),
		Event:     models.EventExportDownloaded,
		Endpoint:  r.URL.Path,
		IPAddress: r.RemoteAddr,
		UserAgent: r.UserAgent(),
		Metadata:  map[string]any{"export_job_id": id},
	})

	response.OKWithRequest(w, r, "download URL created", download)
}

// ServeLocalObject serves a signed download from the filesystem backend.
//
// This route deliberately does not require a bearer token: the signature in
// the URL *is* the authorization, exactly as with an S3 presigned URL, which
// is what lets a browser or curl follow the link directly. The signature
// covers both the object key and the expiry, so neither can be edited to
// reach another tenant's export or to extend the window.
func (h *ExportHandler) ServeLocalObject(w http.ResponseWriter, r *http.Request) {
	if h.local == nil {
		response.Fail(w, r, apierr.NotFound("not found"))
		return
	}

	q := r.URL.Query()
	key := q.Get("key")
	signature := q.Get("signature")
	expires, err := strconv.ParseInt(q.Get("expires"), 10, 64)
	if err != nil {
		response.Fail(w, r, apierr.Forbidden("this download link is not valid"))
		return
	}

	if err := h.local.VerifySignature(key, expires, signature); err != nil {
		response.Fail(w, r, apierr.Forbidden("this download link has expired or been altered"))
		return
	}

	body, size, err := h.local.Open(r.Context(), key)
	if errors.Is(err, exports.ErrObjectNotFound) {
		response.Fail(w, r, apierr.NotFound("this export is no longer available"))
		return
	}
	if err != nil {
		response.Fail(w, r, apierr.Internal("failed to read the export").Wrap(err))
		return
	}
	defer body.Close()

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Content-Disposition", `attachment; filename="bridgecore-usage-export.csv"`)
	w.Header().Set("Cache-Control", "private, no-store")
	_, _ = io.Copy(w, body)
}
