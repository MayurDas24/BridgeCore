// Package model holds the GraphQL-facing representations of BridgeCore's
// domain objects.
//
// These types exist specifically so that database models are never exposed
// through the API. models.User carries PasswordHash, models.APIKey carries
// KeyHash, models.RefreshToken carries TokenHash: a schema built by
// reflecting over those structs is one careless field addition away from
// publishing a credential. Mapping into a separate, deliberately-chosen
// struct makes exposure an explicit act rather than the default.
//
// The json tags are the GraphQL field names, which is what lets the executor
// resolve most fields without a hand-written resolver per field.
package model

import (
	"time"

	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/repository"
)

// Tenant is the API view of a customer organization.
type Tenant struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	Plan      string    `json:"plan"`
	IsActive  bool      `json:"isActive"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// NewTenant maps a domain tenant into its API view.
func NewTenant(t *models.Tenant) *Tenant {
	if t == nil {
		return nil
	}
	return &Tenant{
		ID:        t.ID,
		Name:      t.Name,
		Slug:      t.Slug,
		Plan:      string(t.Plan),
		IsActive:  t.IsActive,
		CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt,
	}
}

// User is the API view of a person.
//
// PasswordHash is absent by construction, not by omission from a field list
// that someone has to remember to keep pruned.
type User struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	FirstName   string     `json:"firstName"`
	LastName    string     `json:"lastName"`
	Role        string     `json:"role"`
	IsActive    bool       `json:"isActive"`
	LastLoginAt *time.Time `json:"lastLoginAt"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`

	// TenantID is carried for the tenant field's DataLoader and is not
	// itself a schema field.
	TenantID string `json:"-"`
}

func NewUser(u *models.User) *User {
	if u == nil {
		return nil
	}
	return &User{
		ID:          u.ID,
		Email:       u.Email,
		FirstName:   u.FirstName,
		LastName:    u.LastName,
		Role:        string(u.Role),
		IsActive:    u.IsActive,
		LastLoginAt: u.LastLoginAt,
		CreatedAt:   u.CreatedAt,
		UpdatedAt:   u.UpdatedAt,
		TenantID:    u.TenantID,
	}
}

func NewUsers(in []*models.User) []*User {
	out := make([]*User, 0, len(in))
	for _, u := range in {
		out = append(out, NewUser(u))
	}
	return out
}

// Feature is a gate-able platform capability.
type Feature struct {
	ID          string `json:"id"`
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

func NewFeatures(in []*models.Feature) []*Feature {
	out := make([]*Feature, 0, len(in))
	for _, f := range in {
		out = append(out, &Feature{ID: f.ID, Key: f.Key, Name: f.Name, Description: f.Description})
	}
	return out
}

// APIKey is the API view of a machine credential. KeyHash is never mapped;
// the plaintext key appears only in GeneratedAPIKey, only once.
type APIKey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	LastFour   string     `json:"lastFour"`
	IsActive   bool       `json:"isActive"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
	ExpiresAt  *time.Time `json:"expiresAt"`
	CreatedAt  time.Time  `json:"createdAt"`
	RevokedAt  *time.Time `json:"revokedAt"`
}

func NewAPIKey(k *models.APIKey) *APIKey {
	if k == nil {
		return nil
	}
	return &APIKey{
		ID:         k.ID,
		Name:       k.Name,
		Prefix:     k.Prefix,
		LastFour:   k.LastFour,
		IsActive:   k.IsActive,
		LastUsedAt: k.LastUsedAt,
		ExpiresAt:  k.ExpiresAt,
		CreatedAt:  k.CreatedAt,
		RevokedAt:  k.RevokedAt,
	}
}

func NewAPIKeys(in []*models.APIKey) []*APIKey {
	out := make([]*APIKey, 0, len(in))
	for _, k := range in {
		out = append(out, NewAPIKey(k))
	}
	return out
}

// GeneratedAPIKey carries the one and only exposure of a plaintext key.
type GeneratedAPIKey struct {
	APIKey    *APIKey `json:"apiKey"`
	Plaintext string  `json:"plaintext"`
}

// UsageRecord is one metered request.
type UsageRecord struct {
	ID         string    `json:"id"`
	Endpoint   string    `json:"endpoint"`
	Method     string    `json:"method"`
	StatusCode int       `json:"statusCode"`
	LatencyMS  int       `json:"latencyMs"`
	RequestID  string    `json:"requestId"`
	CreatedAt  time.Time `json:"createdAt"`
}

func NewUsageRecords(in []*models.UsageLog) []*UsageRecord {
	out := make([]*UsageRecord, 0, len(in))
	for _, u := range in {
		out = append(out, &UsageRecord{
			ID:         u.ID,
			Endpoint:   u.Endpoint,
			Method:     u.Method,
			StatusCode: u.StatusCode,
			LatencyMS:  u.LatencyMS,
			RequestID:  u.RequestID,
			CreatedAt:  u.CreatedAt,
		})
	}
	return out
}

// UsageSummary aggregates usage per endpoint.
type UsageSummary struct {
	Endpoint     string  `json:"endpoint"`
	Method       string  `json:"method"`
	RequestCount int64   `json:"requestCount"`
	ErrorCount   int64   `json:"errorCount"`
	AvgLatencyMS float64 `json:"avgLatencyMs"`
}

func NewUsageSummaries(in []repository.EndpointSummary) []*UsageSummary {
	out := make([]*UsageSummary, 0, len(in))
	for _, s := range in {
		out = append(out, &UsageSummary{
			Endpoint:     s.Endpoint,
			Method:       s.Method,
			RequestCount: s.RequestCount,
			ErrorCount:   s.ErrorCount,
			AvgLatencyMS: s.AvgLatencyMS,
		})
	}
	return out
}

// AuditEntry is one audit record.
type AuditEntry struct {
	ID        string    `json:"id"`
	Event     string    `json:"event"`
	ActorID   *string   `json:"actorId"`
	Endpoint  string    `json:"endpoint"`
	IPAddress string    `json:"ipAddress"`
	UserAgent string    `json:"userAgent"`
	Metadata  string    `json:"metadata"`
	CreatedAt time.Time `json:"createdAt"`
}

// ExportJob is the API view of an asynchronous export.
type ExportJob struct {
	ID         string     `json:"id"`
	Status     string     `json:"status"`
	Endpoint   string     `json:"endpoint"`
	Method     string     `json:"method"`
	RowCount   int        `json:"rowCount"`
	SizeBytes  int64      `json:"sizeBytes"`
	Attempts   int        `json:"attempts"`
	Error      string     `json:"error"`
	StartedAt  *time.Time `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
	CreatedAt  time.Time  `json:"createdAt"`
}

func NewExportJob(j *models.ExportJob) *ExportJob {
	if j == nil {
		return nil
	}
	return &ExportJob{
		ID:         j.ID,
		Status:     string(j.Status),
		Endpoint:   j.Endpoint,
		Method:     j.Method,
		RowCount:   j.RowCount,
		SizeBytes:  j.SizeBytes,
		Attempts:   j.Attempts,
		Error:      j.Error,
		StartedAt:  j.StartedAt,
		FinishedAt: j.FinishedAt,
		CreatedAt:  j.CreatedAt,
	}
}

func NewExportJobs(in []*models.ExportJob) []*ExportJob {
	out := make([]*ExportJob, 0, len(in))
	for _, j := range in {
		out = append(out, NewExportJob(j))
	}
	return out
}

// PageInfo is the pagination envelope shared by every connection type, so
// GraphQL and REST report pagination identically.
type PageInfo struct {
	Page       int   `json:"page"`
	PageSize   int   `json:"pageSize"`
	TotalCount int64 `json:"totalCount"`
	TotalPages int   `json:"totalPages"`
	HasNext    bool  `json:"hasNext"`
}

// NewPageInfo derives the page envelope from a page/size/total triple.
func NewPageInfo(page, pageSize int, total int64) *PageInfo {
	pages := 0
	if pageSize > 0 {
		pages = int(total) / pageSize
		if int(total)%pageSize != 0 {
			pages++
		}
	}
	return &PageInfo{
		Page:       page,
		PageSize:   pageSize,
		TotalCount: total,
		TotalPages: pages,
		HasNext:    page < pages,
	}
}

// Connection types. Each is a plain struct rather than a generic so the
// schema builder can bind a concrete Go type per GraphQL type.

type UserConnection struct {
	Nodes    []*User   `json:"nodes"`
	PageInfo *PageInfo `json:"pageInfo"`
}

type UsageConnection struct {
	Nodes    []*UsageRecord `json:"nodes"`
	PageInfo *PageInfo      `json:"pageInfo"`
}

type AuditConnection struct {
	Nodes    []*AuditEntry `json:"nodes"`
	PageInfo *PageInfo     `json:"pageInfo"`
}

type ExportJobConnection struct {
	Nodes    []*ExportJob `json:"nodes"`
	PageInfo *PageInfo    `json:"pageInfo"`
}

// Download is a completed export's short-lived download URL.
type Download struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
	RowCount  int       `json:"rowCount"`
	SizeBytes int64     `json:"sizeBytes"`
}
