package main

import (
	"net/http"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bridgecore/bridgecore/graph"
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
	exportHandler      *handler.ExportHandler
	auditHandler       *handler.AuditHandler
	platformHandler    *handler.PlatformHandler
	healthHandler      *handler.HealthHandler
	graphqlHandler     *graph.Handler
}

// registerRoutes wires every BridgeCore endpoint onto mux using Go 1.22's
// stdlib method+pattern routing ("METHOD /path/{param}"), composing the shared
// middleware chains (auth, RBAC, entitlements, rate limiting, usage metering)
// around each handler.
//
// The routing table is the authorization model made legible: reading it tells
// you exactly which credential each endpoint requires, without opening a
// single handler.
func registerRoutes(mux *http.ServeMux, d routerDeps) {
	// ---- Public: health probes ----
	// Never authenticated, never rate limited, never metered: a probe that can
	// be rate limited will eventually be rate limited, and the orchestrator
	// will conclude the service is down.
	mux.HandleFunc("GET /health", d.healthHandler.Health)
	mux.HandleFunc("GET /ready", d.healthHandler.Ready)
	mux.HandleFunc("GET /live", d.healthHandler.Live)

	// ---- Developer tooling (non-production only) ----
	if d.cfg.ExposeDevTools {
		mux.Handle("GET /docs/", http.StripPrefix("/docs/", http.FileServer(http.Dir("./docs"))))
		mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/docs/", http.StatusMovedPermanently)
		})
	}

	// The shared chain for every authenticated route. Order matters:
	// authenticate first (so the rate limiter can key per tenant rather than
	// per IP), then rate limit, then meter.
	authenticated := mw.Chain(
		mw.Auth(d.jwtManager, d.apiKeySvc, d.tenantSvc, d.auditSvc, d.log),
		mw.RateLimit(d.redis, d.cfg.RateLimitRequestsPerMinute),
		mw.UsageMetering(d.usageSvc, d.log),
	)

	requireAdmin := mw.RequireRole(d.auditSvc, models.RoleAdmin)
	requireDeveloper := mw.RequireRole(d.auditSvc, models.RoleDeveloper)
	requireUsageExport := mw.RequireFeature(d.entitlementSvc, d.auditSvc, "usage.export")

	// ---- Public: authentication ----
	mux.HandleFunc("POST /api/v1/auth/signup", d.authHandler.Signup)
	mux.HandleFunc("POST /api/v1/auth/login", d.authHandler.Login)
	mux.HandleFunc("POST /api/v1/auth/refresh", d.authHandler.Refresh)

	// ---- Authenticated: session ----
	mux.Handle("POST /api/v1/auth/logout", authenticated(http.HandlerFunc(d.authHandler.Logout)))
	mux.Handle("GET /api/v1/auth/me", authenticated(http.HandlerFunc(d.authHandler.Me)))

	// ---- GraphQL ----
	// Behind the identical chain as REST, so a GraphQL request is
	// authenticated, rate limited, metered and correlated the same way.
	mux.Handle(d.cfg.GraphQL.Path, authenticated(d.graphqlHandler))

	// ---- Tenant (own tenant only) ----
	mux.Handle("GET /api/v1/tenant", authenticated(http.HandlerFunc(d.tenantHandler.Current)))
	mux.Handle("PATCH /api/v1/tenant", authenticated(requireAdmin(http.HandlerFunc(d.tenantHandler.Update))))
	// Retained for API compatibility. Both are tenant-scoped now: List returns
	// only the caller's tenant, and Get resolves only the caller's own ID.
	mux.Handle("GET /api/v1/tenants", authenticated(http.HandlerFunc(d.tenantHandler.List)))
	mux.Handle("GET /api/v1/tenants/{id}", authenticated(http.HandlerFunc(d.tenantHandler.Get)))

	// ---- Users / RBAC ----
	mux.Handle("GET /api/v1/users", authenticated(http.HandlerFunc(d.userHandler.List)))
	mux.Handle("GET /api/v1/users/me", authenticated(http.HandlerFunc(d.userHandler.Me)))
	mux.Handle("GET /api/v1/users/{id}", authenticated(http.HandlerFunc(d.userHandler.Get)))
	mux.Handle("PATCH /api/v1/users/{id}/role", authenticated(requireAdmin(http.HandlerFunc(d.userHandler.UpdateRole))))
	mux.Handle("PATCH /api/v1/users/{id}/status", authenticated(requireAdmin(http.HandlerFunc(d.userHandler.SetActive))))

	// ---- Feature entitlements (read-only for tenants) ----
	mux.Handle("GET /api/v1/features", authenticated(http.HandlerFunc(d.entitlementHandler.ListCatalog)))
	mux.Handle("GET /api/v1/features/mine", authenticated(http.HandlerFunc(d.entitlementHandler.ListMine)))

	// ---- API keys (developer or above) ----
	mux.Handle("POST /api/v1/apikeys", authenticated(requireDeveloper(http.HandlerFunc(d.apiKeyHandler.Generate))))
	mux.Handle("GET /api/v1/apikeys", authenticated(http.HandlerFunc(d.apiKeyHandler.List)))
	mux.Handle("POST /api/v1/apikeys/{id}/rotate", authenticated(requireDeveloper(http.HandlerFunc(d.apiKeyHandler.Rotate))))
	mux.Handle("DELETE /api/v1/apikeys/{id}", authenticated(requireDeveloper(http.HandlerFunc(d.apiKeyHandler.Deactivate))))

	// ---- Usage metering ----
	mux.Handle("GET /api/v1/usage", authenticated(http.HandlerFunc(d.usageHandler.List)))
	mux.Handle("GET /api/v1/usage/summary", authenticated(http.HandlerFunc(d.usageHandler.Summary)))

	// ---- Asynchronous usage exports (Pro/Enterprise entitlement) ----
	mux.Handle("POST /api/v1/usage/exports", authenticated(requireUsageExport(http.HandlerFunc(d.exportHandler.Create))))
	mux.Handle("GET /api/v1/usage/exports", authenticated(requireUsageExport(http.HandlerFunc(d.exportHandler.List))))
	mux.Handle("GET /api/v1/usage/exports/{id}", authenticated(requireUsageExport(http.HandlerFunc(d.exportHandler.Get))))
	mux.Handle("GET /api/v1/usage/exports/{id}/download", authenticated(requireUsageExport(http.HandlerFunc(d.exportHandler.DownloadURL))))

	// Signed object download. Deliberately unauthenticated: the HMAC signature
	// in the URL is the capability, exactly as with an S3 presigned URL, which
	// is what lets a browser follow the link. The signature covers the object
	// key and the expiry, so neither can be edited.
	mux.HandleFunc("GET "+exportDownloadPath, d.exportHandler.ServeLocalObject)

	// ---- Audit trail ----
	mux.Handle("GET /api/v1/audit", authenticated(http.HandlerFunc(d.auditHandler.List)))
	mux.Handle("GET /api/v1/audit/{id}", authenticated(http.HandlerFunc(d.auditHandler.Get)))

	// ---- Platform control plane ----
	// Cross-tenant operations, authenticated with the operator token instead of
	// any customer credential. Not registered at all when no token is
	// configured, so a deployment that does not need it does not expose it.
	if d.cfg.PlatformAdminToken != "" {
		platform := mw.Chain(
			mw.RequirePlatformToken(d.cfg.PlatformAdminToken, d.auditSvc),
			mw.RateLimit(d.redis, d.cfg.RateLimitRequestsPerMinute),
		)

		mux.Handle("POST /api/v1/platform/tenants", platform(http.HandlerFunc(d.platformHandler.CreateTenant)))
		mux.Handle("GET /api/v1/platform/tenants", platform(http.HandlerFunc(d.platformHandler.ListTenants)))
		mux.Handle("GET /api/v1/platform/tenants/{id}", platform(http.HandlerFunc(d.platformHandler.GetTenant)))
		mux.Handle("PUT /api/v1/platform/tenants/{id}", platform(http.HandlerFunc(d.platformHandler.UpdateTenant)))
		mux.Handle("DELETE /api/v1/platform/tenants/{id}", platform(http.HandlerFunc(d.platformHandler.DeleteTenant)))
		mux.Handle("POST /api/v1/platform/features/grant", platform(http.HandlerFunc(d.platformHandler.GrantFeature)))

		d.log.Info("platform control plane enabled", zap.String("prefix", "/api/v1/platform"))
	}
}
