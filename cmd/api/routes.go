package main

import (
	"net/http"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bridgecore/bridgecore/internal/config"
	"github.com/bridgecore/bridgecore/internal/handler"
	mw "github.com/bridgecore/bridgecore/internal/middleware"
	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/service"
	"github.com/bridgecore/bridgecore/pkg/jwt"
)

// routerDeps bundles everything registerRoutes needs to build handlers and
// middleware chains, kept as a single struct so main() stays readable.
type routerDeps struct {
	cfg            *config.Config
	log            *zap.Logger
	jwtManager     *jwt.Manager
	apiKeySvc      *service.APIKeyService
	tenantSvc      *service.TenantService
	entitlementSvc *service.EntitlementService
	auditSvc       *service.AuditService
	usageSvc       *service.UsageService
	redis          *redis.Client

	authHandler        *handler.AuthHandler
	tenantHandler      *handler.TenantHandler
	userHandler        *handler.UserHandler
	entitlementHandler *handler.EntitlementHandler
	apiKeyHandler      *handler.APIKeyHandler
	usageHandler       *handler.UsageHandler
	auditHandler       *handler.AuditHandler
	healthHandler      *handler.HealthHandler
}

// registerRoutes wires every BridgeCore endpoint onto mux using Go 1.22's
// stdlib method+pattern routing ("METHOD /path/{param}"), composing the
// shared middleware chains (auth, RBAC, entitlements, rate limiting, usage
// metering) around each handler.
func registerRoutes(mux *http.ServeMux, d routerDeps) {
	// ---- Public: health checks ----
	mux.HandleFunc("GET /health", d.healthHandler.Health)
	mux.HandleFunc("GET /ready", d.healthHandler.Ready)
	mux.HandleFunc("GET /live", d.healthHandler.Live)

	// ---- Public: API docs ----
	mux.Handle("GET /docs/", http.StripPrefix("/docs/", http.FileServer(http.Dir("./docs"))))
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/docs/", http.StatusMovedPermanently)
	})

	authenticated := mw.Chain(
		mw.Auth(d.jwtManager, d.apiKeySvc, d.tenantSvc, d.auditSvc, d.log),
		mw.RateLimit(d.redis, d.cfg.RateLimitRequestsPerMinute),
		mw.UsageMetering(d.usageSvc, d.log),
	)

	requireAdmin := mw.RequireRole(d.auditSvc, models.RoleAdmin)
	requireDeveloper := mw.RequireRole(d.auditSvc, models.RoleDeveloper)

	// ---- Public: auth ----
	mux.HandleFunc("POST /api/v1/auth/signup", d.authHandler.Signup)
	mux.HandleFunc("POST /api/v1/auth/login", d.authHandler.Login)
	mux.HandleFunc("POST /api/v1/auth/refresh", d.authHandler.Refresh)

	// ---- Authenticated: auth ----
	mux.Handle("POST /api/v1/auth/logout", authenticated(http.HandlerFunc(d.authHandler.Logout)))
	mux.Handle("GET /api/v1/auth/me", authenticated(http.HandlerFunc(d.authHandler.Me)))

	// ---- Tenants (admin only) ----
	mux.Handle("POST /api/v1/tenants", authenticated(requireAdmin(http.HandlerFunc(d.tenantHandler.Create))))
	mux.Handle("GET /api/v1/tenants", authenticated(requireAdmin(http.HandlerFunc(d.tenantHandler.List))))
	mux.Handle("GET /api/v1/tenants/{id}", authenticated(http.HandlerFunc(d.tenantHandler.Get)))
	mux.Handle("PUT /api/v1/tenants/{id}", authenticated(requireAdmin(http.HandlerFunc(d.tenantHandler.Update))))
	mux.Handle("DELETE /api/v1/tenants/{id}", authenticated(requireAdmin(http.HandlerFunc(d.tenantHandler.Delete))))

	// ---- Users / RBAC (admin only for role changes) ----
	mux.Handle("GET /api/v1/users", authenticated(http.HandlerFunc(d.userHandler.List)))
	mux.Handle("PATCH /api/v1/users/{id}/role", authenticated(requireAdmin(http.HandlerFunc(d.userHandler.UpdateRole))))

	// ---- Feature entitlements ----
	mux.Handle("GET /api/v1/features", authenticated(http.HandlerFunc(d.entitlementHandler.ListCatalog)))
	mux.Handle("GET /api/v1/features/mine", authenticated(http.HandlerFunc(d.entitlementHandler.ListMine)))
	mux.Handle("POST /api/v1/features/grant", authenticated(requireAdmin(http.HandlerFunc(d.entitlementHandler.Grant))))

	// ---- API keys (developer or above) ----
	mux.Handle("POST /api/v1/apikeys", authenticated(requireDeveloper(http.HandlerFunc(d.apiKeyHandler.Generate))))
	mux.Handle("GET /api/v1/apikeys", authenticated(http.HandlerFunc(d.apiKeyHandler.List)))
	mux.Handle("POST /api/v1/apikeys/{id}/rotate", authenticated(requireDeveloper(http.HandlerFunc(d.apiKeyHandler.Rotate))))
	mux.Handle("DELETE /api/v1/apikeys/{id}", authenticated(requireDeveloper(http.HandlerFunc(d.apiKeyHandler.Deactivate))))

	// ---- Usage metering ----
	mux.Handle("GET /api/v1/usage", authenticated(http.HandlerFunc(d.usageHandler.List)))
	mux.Handle("GET /api/v1/usage/summary", authenticated(http.HandlerFunc(d.usageHandler.Summary)))
	// CSV export is gated behind the "usage.export" feature entitlement
	// (Pro/Enterprise only) — the RequireFeature middleware runs BEFORE
	// the handler and rejects Free-plan tenants with 403.
	requireUsageExport := mw.RequireFeature(d.entitlementSvc, d.auditSvc, "usage.export")
	mux.Handle("GET /api/v1/usage/export", authenticated(requireUsageExport(http.HandlerFunc(d.usageHandler.Export))))

	// ---- Audit logs ----
	mux.Handle("GET /api/v1/audit", authenticated(http.HandlerFunc(d.auditHandler.List)))
	mux.Handle("GET /api/v1/audit/{id}", authenticated(http.HandlerFunc(d.auditHandler.Get)))
}
