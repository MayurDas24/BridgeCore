// Package models defines the domain entities persisted by BridgeCore.
// These are plain structs mapped by hand in the repository layer (no ORM
// magic), which keeps the SQL <-> Go mapping explicit and easy to reason
// about under load.
package models

import "time"

// Role is a platform-level RBAC role. Roles are intentionally coarse
// (admin/developer/viewer) — fine-grained permissions are expressed via
// feature entitlements instead.
type Role string

const (
	RoleAdmin     Role = "admin"
	RoleDeveloper Role = "developer"
	RoleViewer    Role = "viewer"
)

// Valid reports whether r is one of the known roles.
func (r Role) Valid() bool {
	switch r {
	case RoleAdmin, RoleDeveloper, RoleViewer:
		return true
	}
	return false
}

// Plan is a tenant's subscription tier, which drives feature entitlements.
type Plan string

const (
	PlanFree       Plan = "free"
	PlanPro        Plan = "pro"
	PlanEnterprise Plan = "enterprise"
)

func (p Plan) Valid() bool {
	switch p {
	case PlanFree, PlanPro, PlanEnterprise:
		return true
	}
	return false
}

// Tenant represents a single customer organization. Every user, API key,
// usage record, and audit record is scoped to exactly one tenant.
type Tenant struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Slug      string     `json:"slug"`
	Plan      Plan       `json:"plan"`
	IsActive  bool       `json:"is_active"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// User represents a human actor belonging to exactly one tenant.
type User struct {
	ID           string     `json:"id"`
	TenantID     string     `json:"tenant_id"`
	Email        string     `json:"email"`
	PasswordHash string     `json:"-"`
	FirstName    string     `json:"first_name"`
	LastName     string     `json:"last_name"`
	Role         Role       `json:"role"`
	IsActive     bool       `json:"is_active"`
	LastLoginAt  *time.Time `json:"last_login_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	DeletedAt    *time.Time `json:"deleted_at,omitempty"`
}

// Feature is a gate-able platform capability, identified by a stable key
// (e.g. "usage.export", "audit.retention_90d").
type Feature struct {
	ID          string    `json:"id"`
	Key         string    `json:"key"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// TenantFeature is the join between a tenant and a feature it is entitled to.
type TenantFeature struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	FeatureID string    `json:"feature_id"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// APIKey represents a machine credential scoped to a tenant. The plaintext
// key is only ever returned once, at creation/rotation time; thereafter
// only key_hash (bcrypt) and a display-safe last_four are stored.
type APIKey struct {
	ID         string     `json:"id"`
	TenantID   string     `json:"tenant_id"`
	CreatedBy  *string    `json:"created_by,omitempty"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	KeyHash    string     `json:"-"`
	LastFour   string     `json:"last_four"`
	IsActive   bool       `json:"is_active"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// RefreshToken represents a long-lived, rotatable credential used to mint
// new access tokens without re-authenticating.
type RefreshToken struct {
	ID        string     `json:"id"`
	UserID    string     `json:"user_id"`
	TokenHash string     `json:"-"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// UsageLog is one recorded API request, captured automatically by the
// usage-metering middleware.
type UsageLog struct {
	ID         string    `json:"id"`
	TenantID   *string   `json:"tenant_id,omitempty"`
	Endpoint   string    `json:"endpoint"`
	Method     string    `json:"method"`
	StatusCode int       `json:"status_code"`
	LatencyMS  int       `json:"latency_ms"`
	RequestID  string    `json:"request_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// AuditLog is an immutable record of a security- or business-relevant
// action taken on the platform.
type AuditLog struct {
	ID        string         `json:"id"`
	TenantID  *string        `json:"tenant_id,omitempty"`
	ActorID   *string        `json:"actor_id,omitempty"`
	Event     string         `json:"event"`
	Metadata  map[string]any `json:"metadata"`
	Endpoint  string         `json:"endpoint"`
	IPAddress string         `json:"ip_address"`
	UserAgent string         `json:"user_agent"`
	CreatedAt time.Time      `json:"created_at"`
}

// Audit event name constants, kept centralized so handlers/services never
// hand-roll event strings (and risk typos fragmenting the audit trail).
const (
	EventUserSignup          = "user.signup"
	EventUserLogin           = "user.login"
	EventUserLoginFailed     = "user.login_failed"
	EventUserLogout          = "user.logout"
	EventTenantCreated       = "tenant.created"
	EventTenantUpdated       = "tenant.updated"
	EventTenantDeleted       = "tenant.deleted"
	EventRoleChanged         = "user.role_changed"
	EventAPIKeyGenerated     = "apikey.generated"
	EventAPIKeyRotated       = "apikey.rotated"
	EventAPIKeyRevoked       = "apikey.revoked"
	EventFeatureAccessDenied = "feature.access_denied"
	EventUnauthorizedRequest = "request.unauthorized"
)
