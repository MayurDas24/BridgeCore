package handler

import (
	"errors"
	"net/http"

	"github.com/bridgecore/bridgecore/internal/middleware"
	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/service"
	"github.com/bridgecore/bridgecore/pkg/response"
)

// APIKeyHandler exposes generate/rotate/deactivate/list for tenant-scoped
// API keys. All operations are scoped to the caller's own tenant.
type APIKeyHandler struct {
	apiKeys *service.APIKeyService
	audit   *service.AuditService
}

func NewAPIKeyHandler(apiKeys *service.APIKeyService, audit *service.AuditService) *APIKeyHandler {
	return &APIKeyHandler{apiKeys: apiKeys, audit: audit}
}

type generateAPIKeyRequest struct {
	Name string `json:"name"`
}

// Generate godoc
// @Summary      Generate a new API key for the current tenant
// @Tags         apikeys
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        body body generateAPIKeyRequest true "Key metadata"
// @Success      201 {object} response.Envelope
// @Router       /api/v1/apikeys [post]
func (h *APIKeyHandler) Generate(w http.ResponseWriter, r *http.Request) {
	ac, _ := middleware.AuthFromContext(r.Context())

	var req generateAPIKeyRequest
	_ = decodeJSON(r, &req) // name is optional; ignore body-absent error

	plaintext, key, err := h.apiKeys.Generate(r.Context(), ac.TenantID, strPtrOrNil(ac.UserID), req.Name)
	if err != nil {
		response.InternalError(w, "failed to generate API key")
		return
	}

	h.audit.Record(r.Context(), service.RecordInput{
		TenantID:  &ac.TenantID,
		ActorID:   strPtrOrNil(ac.UserID),
		Event:     models.EventAPIKeyGenerated,
		Endpoint:  r.URL.Path,
		IPAddress: r.RemoteAddr,
		UserAgent: r.UserAgent(),
		Metadata:  map[string]any{"api_key_id": key.ID, "name": key.Name},
	})

	response.Created(w, "API key generated — store it now, it will not be shown again", map[string]any{
		"api_key": plaintext,
		"details": key,
	})
}

// List godoc
// @Summary      List API keys for the current tenant
// @Tags         apikeys
// @Security     BearerAuth
// @Produce      json
// @Success      200 {object} response.Envelope
// @Router       /api/v1/apikeys [get]
func (h *APIKeyHandler) List(w http.ResponseWriter, r *http.Request) {
	ac, _ := middleware.AuthFromContext(r.Context())

	keys, err := h.apiKeys.ListByTenant(r.Context(), ac.TenantID)
	if err != nil {
		response.InternalError(w, "failed to list API keys")
		return
	}
	response.OK(w, "API keys retrieved", keys)
}

// Rotate godoc
// @Summary      Rotate an API key (deactivate old, issue new)
// @Tags         apikeys
// @Security     BearerAuth
// @Produce      json
// @Param        id path string true "API Key ID"
// @Success      200 {object} response.Envelope
// @Router       /api/v1/apikeys/{id}/rotate [post]
func (h *APIKeyHandler) Rotate(w http.ResponseWriter, r *http.Request) {
	ac, _ := middleware.AuthFromContext(r.Context())
	id := r.PathValue("id")

	plaintext, key, err := h.apiKeys.Rotate(r.Context(), ac.TenantID, id, strPtrOrNil(ac.UserID))
	if err != nil {
		h.handleError(w, err)
		return
	}

	h.audit.Record(r.Context(), service.RecordInput{
		TenantID:  &ac.TenantID,
		ActorID:   strPtrOrNil(ac.UserID),
		Event:     models.EventAPIKeyRotated,
		Endpoint:  r.URL.Path,
		IPAddress: r.RemoteAddr,
		UserAgent: r.UserAgent(),
		Metadata:  map[string]any{"old_key_id": id, "new_key_id": key.ID},
	})

	response.OK(w, "API key rotated — store the new key now, it will not be shown again", map[string]any{
		"api_key": plaintext,
		"details": key,
	})
}

// Deactivate godoc
// @Summary      Deactivate (revoke) an API key
// @Tags         apikeys
// @Security     BearerAuth
// @Param        id path string true "API Key ID"
// @Success      200 {object} response.Envelope
// @Router       /api/v1/apikeys/{id} [delete]
func (h *APIKeyHandler) Deactivate(w http.ResponseWriter, r *http.Request) {
	ac, _ := middleware.AuthFromContext(r.Context())
	id := r.PathValue("id")

	if err := h.apiKeys.Deactivate(r.Context(), ac.TenantID, id); err != nil {
		h.handleError(w, err)
		return
	}

	h.audit.Record(r.Context(), service.RecordInput{
		TenantID:  &ac.TenantID,
		ActorID:   strPtrOrNil(ac.UserID),
		Event:     models.EventAPIKeyRevoked,
		Endpoint:  r.URL.Path,
		IPAddress: r.RemoteAddr,
		UserAgent: r.UserAgent(),
		Metadata:  map[string]any{"api_key_id": id},
	})

	response.OK(w, "API key deactivated", nil)
}

func (h *APIKeyHandler) handleError(w http.ResponseWriter, err error) {
	if errors.Is(err, service.ErrAPIKeyNotFound) {
		response.NotFound(w, "API key not found")
		return
	}
	response.InternalError(w, "operation failed")
}
