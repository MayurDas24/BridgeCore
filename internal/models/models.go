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

// ExportStatus is the lifecycle state of an asynchronous usage export job.
//
// The state machine is deliberately explicit rather than a boolean "done"
// flag, because an async pipeline has more than two outcomes: a job can be
// in flight, it can fail transiently and be retried, and it can fail
// permanently and need to be visible to an operator.
//
//	queued -> processing -> completed
//	                 |
//	                 +----> queued   (transient failure, attempts remaining)
//	                 |
//	                 +----> failed   (attempts exhausted)
type ExportStatus string

const (
	ExportStatusQueued     ExportStatus = "queued"
	ExportStatusProcessing ExportStatus = "processing"
	ExportStatusCompleted  ExportStatus = "completed"
	ExportStatusFailed     ExportStatus = "failed"
)

// Valid reports whether s is a known export status.
func (s ExportStatus) Valid() bool {
	switch s {
	case ExportStatusQueued, ExportStatusProcessing, ExportStatusCompleted, ExportStatusFailed:
		return true
	}
	return false
}

// Terminal reports whether no further transition is possible.
func (s ExportStatus) Terminal() bool {
	return s == ExportStatusCompleted || s == ExportStatusFailed
}

// ExportJob is one asynchronous usage-export request.
//
// The job row is the durable record of intent: the API creates it and
// returns immediately, and a worker (in-process locally, a dedicated ECS
// service or a Lambda consumer in production) claims and fulfils it. ObjectKey
// points at the generated CSV in the object store; the download URL is
// always minted on demand and short-lived, so the row never contains a
// long-lived credential.
type ExportJob struct {
	ID          string       `json:"id"`
	TenantID    string       `json:"tenant_id"`
	RequestedBy *string      `json:"requested_by,omitempty"`
	Status      ExportStatus `json:"status"`

	// Filters captured at request time, so a replayed job produces the
	// same output even if the caller's session is long gone.
	Endpoint string     `json:"endpoint,omitempty"`
	Method   string     `json:"method,omitempty"`
	From     *time.Time `json:"from,omitempty"`
	To       *time.Time `json:"to,omitempty"`

	ObjectKey  string     `json:"object_key,omitempty"`
	RowCount   int        `json:"row_count"`
	SizeBytes  int64      `json:"size_bytes"`
	Attempts   int        `json:"attempts"`
	Error      string     `json:"error,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// Additional audit events introduced by the async export pipeline and by
// explicit cross-tenant denial tracking.
const (
	EventExportRequested   = "usage_export.requested"
	EventExportCompleted   = "usage_export.completed"
	EventExportFailed      = "usage_export.failed"
	EventExportDownloaded  = "usage_export.downloaded"
	EventFeatureGranted    = "feature.granted"
	EventFeatureRevoked    = "feature.revoked"
	EventCrossTenantDenied = "security.cross_tenant_denied"
	EventGraphQLRejected   = "graphql.query_rejected"
)
