// Package handler implements HTTP handlers for every BridgeCore endpoint.
// Handlers are thin: they decode/validate the request, call into a
// service, and translate the result (or error) into the standard response
// envelope. All business logic lives in internal/service.
package handler

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/bridgecore/bridgecore/internal/middleware"
	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/service"
	"github.com/bridgecore/bridgecore/pkg/response"
)

// AuthHandler exposes signup/login/refresh/logout/me endpoints.
type AuthHandler struct {
	auth  *service.AuthService
	audit *service.AuditService
}

func NewAuthHandler(auth *service.AuthService, audit *service.AuditService) *AuthHandler {
	return &AuthHandler{auth: auth, audit: audit}
}

var slugSanitizer = regexp.MustCompile(`[^a-z0-9-]+`)

func slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = strings.ReplaceAll(s, " ", "-")
	return slugSanitizer.ReplaceAllString(s, "")
}

type signupRequest struct {
	TenantName string `json:"tenant_name"`
	Email      string `json:"email"`
	Password   string `json:"password"`
	FirstName  string `json:"first_name"`
	LastName   string `json:"last_name"`
}

// Signup godoc
// @Summary      Register a new tenant and admin user
// @Description  Creates a brand-new tenant plus its first (admin) user, and returns an access/refresh token pair.
// @Tags         auth
// @Accept       json
// @Produce      json
// @Param        body body signupRequest true "Signup payload"
// @Success      201 {object} response.Envelope
// @Failure      400 {object} response.Envelope
// @Failure      409 {object} response.Envelope
// @Router       /api/v1/auth/signup [post]
func (h *AuthHandler) Signup(w http.ResponseWriter, r *http.Request) {
	var req signupRequest
	if err := decodeJSON(r, &req); err != nil {
		response.BadRequest(w, "invalid request body", err.Error())
		return
	}
	if req.TenantName == "" || req.Email == "" || len(req.Password) < 8 {
		response.BadRequest(w, "tenant_name, email, and a password of at least 8 characters are required", nil)
		return
	}

	slug := slugify(req.TenantName)
	if slug == "" {
		response.BadRequest(w, "tenant_name must contain at least one alphanumeric character", nil)
		return
	}

	user, tenant, pair, err := h.auth.Signup(r.Context(), service.SignupInput{
		TenantName: req.TenantName,
		TenantSlug: slug,
		Email:      strings.ToLower(strings.TrimSpace(req.Email)),
		Password:   req.Password,
		FirstName:  req.FirstName,
		LastName:   req.LastName,
	})
	if err != nil {
		h.handleAuthError(w, err)
		return
	}

	h.audit.Record(r.Context(), service.RecordInput{
		TenantID:  &tenant.ID,
		ActorID:   &user.ID,
		Event:     models.EventTenantCreated,
		Endpoint:  r.URL.Path,
		IPAddress: r.RemoteAddr,
		UserAgent: r.UserAgent(),
		Metadata:  map[string]any{"tenant_slug": tenant.Slug},
	})
	h.audit.Record(r.Context(), service.RecordInput{
		TenantID:  &tenant.ID,
		ActorID:   &user.ID,
		Event:     models.EventUserSignup,
		Endpoint:  r.URL.Path,
		IPAddress: r.RemoteAddr,
		UserAgent: r.UserAgent(),
	})

	response.Created(w, "signup successful", map[string]any{
		"user":   user,
		"tenant": tenant,
		"tokens": tokenPairView(pair),
	})
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Login godoc
// @Summary      Log in with email and password
// @Tags         auth
// @Accept       json
// @Produce      json
// @Param        body body loginRequest true "Login payload"
// @Success      200 {object} response.Envelope
// @Failure      401 {object} response.Envelope
// @Router       /api/v1/auth/login [post]
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		response.BadRequest(w, "invalid request body", err.Error())
		return
	}

	user, pair, err := h.auth.Login(r.Context(), strings.ToLower(strings.TrimSpace(req.Email)), req.Password)
	if err != nil {
		tenantID := ""
		if user != nil {
			tenantID = user.TenantID
		}
		h.audit.Record(r.Context(), service.RecordInput{
			TenantID:  strPtrOrNil(tenantID),
			Event:     models.EventUserLoginFailed,
			Endpoint:  r.URL.Path,
			IPAddress: r.RemoteAddr,
			UserAgent: r.UserAgent(),
			Metadata:  map[string]any{"email": req.Email},
		})
		h.handleAuthError(w, err)
		return
	}

	h.audit.Record(r.Context(), service.RecordInput{
		TenantID:  &user.TenantID,
		ActorID:   &user.ID,
		Event:     models.EventUserLogin,
		Endpoint:  r.URL.Path,
		IPAddress: r.RemoteAddr,
		UserAgent: r.UserAgent(),
	})

	response.OK(w, "login successful", map[string]any{
		"user":   user,
		"tokens": tokenPairView(pair),
	})
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// Refresh godoc
// @Summary      Exchange a refresh token for a new token pair
// @Tags         auth
// @Accept       json
// @Produce      json
// @Param        body body refreshRequest true "Refresh payload"
// @Success      200 {object} response.Envelope
// @Failure      401 {object} response.Envelope
// @Router       /api/v1/auth/refresh [post]
func (h *AuthHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := decodeJSON(r, &req); err != nil || req.RefreshToken == "" {
		response.BadRequest(w, "refresh_token is required", nil)
		return
	}

	pair, err := h.auth.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		response.Unauthorized(w, "invalid or expired refresh token")
		return
	}

	response.OK(w, "token refreshed", map[string]any{"tokens": tokenPairView(pair)})
}

// Logout godoc
// @Summary      Log out (revoke all refresh tokens for the current user)
// @Tags         auth
// @Security     BearerAuth
// @Produce      json
// @Success      200 {object} response.Envelope
// @Router       /api/v1/auth/logout [post]
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	ac, ok := middleware.AuthFromContext(r.Context())
	if !ok || ac.UserID == "" {
		response.Unauthorized(w, "authentication required")
		return
	}

	if err := h.auth.Logout(r.Context(), ac.UserID); err != nil {
		response.InternalError(w, "failed to log out")
		return
	}

	h.audit.Record(r.Context(), service.RecordInput{
		TenantID:  &ac.TenantID,
		ActorID:   &ac.UserID,
		Event:     models.EventUserLogout,
		Endpoint:  r.URL.Path,
		IPAddress: r.RemoteAddr,
		UserAgent: r.UserAgent(),
	})

	response.OK(w, "logged out successfully", nil)
}

// Me godoc
// @Summary      Get the currently authenticated user
// @Tags         auth
// @Security     BearerAuth
// @Produce      json
// @Success      200 {object} response.Envelope
// @Router       /api/v1/auth/me [get]
func (h *AuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	ac, ok := middleware.AuthFromContext(r.Context())
	if !ok || ac.UserID == "" {
		response.Unauthorized(w, "authentication required")
		return
	}

	user, err := h.auth.CurrentUser(r.Context(), ac.UserID)
	if err != nil {
		response.NotFound(w, "user not found")
		return
	}
	response.OK(w, "current user", user)
}

func (h *AuthHandler) handleAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidCredentials):
		response.Unauthorized(w, "invalid email or password")
	case errors.Is(err, service.ErrAccountInactive):
		response.Forbidden(w, "account is inactive")
	case errors.Is(err, service.ErrEmailTaken):
		response.Conflict(w, "email is already registered")
	case errors.Is(err, service.ErrTenantSlugTaken):
		response.Conflict(w, "a tenant with a similar name already exists, please choose a different name")
	default:
		response.InternalError(w, "authentication failed")
	}
}

type tokenPairResponse struct {
	AccessToken           string `json:"access_token"`
	AccessTokenExpiresAt  string `json:"access_token_expires_at"`
	RefreshToken          string `json:"refresh_token"`
	RefreshTokenExpiresAt string `json:"refresh_token_expires_at"`
}

func tokenPairView(p *service.TokenPair) tokenPairResponse {
	return tokenPairResponse{
		AccessToken:           p.AccessToken,
		AccessTokenExpiresAt:  p.AccessTokenExpiresAt.Format("2006-01-02T15:04:05Z07:00"),
		RefreshToken:          p.RefreshToken,
		RefreshTokenExpiresAt: p.RefreshTokenExpiresAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}
