// Command seed populates BridgeCore with baseline demo data: one tenant per
// subscription plan (free/pro/enterprise), one user per RBAC role, and the
// default feature catalog. It is idempotent — safe to run multiple times
// (on conflict, it upserts/skips rather than erroring) — so it can run
// automatically on every `docker compose up`.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/bridgecore/bridgecore/internal/config"
	"github.com/bridgecore/bridgecore/internal/database"
	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/repository"
	"github.com/bridgecore/bridgecore/pkg/utils"
)

type seedTenant struct {
	name string
	slug string
	plan models.Plan
}

type seedUser struct {
	email     string
	password  string
	firstName string
	lastName  string
	role      models.Role
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("seed: failed to load config: %v", err)
	}

	db, err := database.NewPostgres(cfg.DB)
	if err != nil {
		log.Fatalf("seed: failed to connect to postgres: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(); err != nil {
		log.Fatalf("seed: failed to run migrations: %v", err)
	}

	ctx := context.Background()
	tenantRepo := repository.NewTenantRepository(db)
	userRepo := repository.NewUserRepository(db)
	entitlementRepo := repository.NewEntitlementRepository(db)

	// ---- Feature catalog ----
	catalog := []models.Feature{
		{Key: "usage.basic_dashboard", Name: "Basic Usage Dashboard", Description: "View basic request volume charts"},
		{Key: "usage.export", Name: "Usage Export", Description: "Export raw usage logs as CSV"},
		{Key: "apikeys.multiple", Name: "Multiple API Keys", Description: "Issue more than one active API key per tenant"},
		{Key: "audit.retention_30d", Name: "30-Day Audit Retention", Description: "Audit logs retained for 30 days"},
		{Key: "audit.retention_90d", Name: "90-Day Audit Retention", Description: "Audit logs retained for 90 days"},
		{Key: "audit.export", Name: "Audit Export", Description: "Export the audit trail for compliance reporting"},
		{Key: "sso.saml", Name: "SAML SSO", Description: "Single sign-on via SAML 2.0"},
		{Key: "support.priority", Name: "Priority Support", Description: "Priority-queue customer support"},
	}
	for _, f := range catalog {
		feature := f
		if err := entitlementRepo.CreateFeature(ctx, &feature); err != nil {
			log.Fatalf("seed: failed to create feature %s: %v", f.Key, err)
		}
		fmt.Printf("✓ feature ready: %s\n", f.Key)
	}

	// ---- Tenants (one per plan) ----
	tenantSeeds := []seedTenant{
		{name: "Freebird Labs", slug: "freebird-labs", plan: models.PlanFree},
		{name: "Proline Systems", slug: "proline-systems", plan: models.PlanPro},
		{name: "Enterprigo Holdings", slug: "enterprigo-holdings", plan: models.PlanEnterprise},
	}

	tenantsBySlug := map[string]*models.Tenant{}
	for _, ts := range tenantSeeds {
		existing, err := tenantRepo.GetBySlug(ctx, ts.slug)
		if err == nil {
			tenantsBySlug[ts.slug] = existing
			fmt.Printf("✓ tenant already exists: %s (%s)\n", ts.name, ts.plan)
			continue
		}
		tenant := &models.Tenant{Name: ts.name, Slug: ts.slug, Plan: ts.plan, IsActive: true}
		if err := tenantRepo.Create(ctx, tenant); err != nil {
			log.Fatalf("seed: failed to create tenant %s: %v", ts.slug, err)
		}
		tenantsBySlug[ts.slug] = tenant
		fmt.Printf("✓ tenant created: %s (%s) [%s]\n", ts.name, ts.plan, tenant.ID)
	}

	// ---- Users (admin/developer/viewer), all under the Pro tenant for a
	// realistic RBAC demo; each seed user has a unique email so re-running
	// is idempotent via the email uniqueness check below. ----
	proTenant := tenantsBySlug["proline-systems"]
	userSeeds := []seedUser{
		{email: "admin@bridgecore.dev", password: "AdminPass123!", firstName: "Ada", lastName: "Admin", role: models.RoleAdmin},
		{email: "developer@bridgecore.dev", password: "DevPass123!", firstName: "Dev", lastName: "Eloper", role: models.RoleDeveloper},
		{email: "viewer@bridgecore.dev", password: "ViewerPass123!", firstName: "Vic", lastName: "Viewer", role: models.RoleViewer},
	}

	for _, us := range userSeeds {
		if _, err := userRepo.GetByEmail(ctx, us.email); err == nil {
			fmt.Printf("✓ user already exists: %s (%s)\n", us.email, us.role)
			continue
		}

		hash, err := utils.HashPassword(us.password)
		if err != nil {
			log.Fatalf("seed: failed to hash password for %s: %v", us.email, err)
		}

		user := &models.User{
			TenantID:     proTenant.ID,
			Email:        us.email,
			PasswordHash: hash,
			FirstName:    us.firstName,
			LastName:     us.lastName,
			Role:         us.role,
			IsActive:     true,
		}
		if err := userRepo.Create(ctx, user); err != nil {
			log.Fatalf("seed: failed to create user %s: %v", us.email, err)
		}
		fmt.Printf("✓ user created: %s (%s) password=%s\n", us.email, us.role, us.password)
	}

	fmt.Println("\nSeed complete. Example login:")
	fmt.Println("  POST /api/v1/auth/login")
	fmt.Println(`  {"email": "admin@bridgecore.dev", "password": "AdminPass123!"}`)
}
