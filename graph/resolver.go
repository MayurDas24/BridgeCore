package graph

import (
	"encoding/json"
	"time"

	"github.com/graphql-go/graphql"
	"go.uber.org/zap"

	"github.com/bridgecore/bridgecore/graph/model"
	"github.com/bridgecore/bridgecore/internal/middleware"
	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/service"
	"github.com/bridgecore/bridgecore/pkg/apierr"
)

// Resolver holds the services the GraphQL API resolves against.
//
// Note what is absent: there is no database handle and no repository. A
// resolver physically cannot bypass the service layer, so every tenant
// isolation check, RBAC rule, and validation the REST API enforces applies
// identically here — not by convention, but because there is no other code
// path available.
type Resolver struct {
	Users        *service.UserService
	Tenants      *service.TenantService
	Entitlements *service.EntitlementService
	APIKeys      *service.APIKeyService
	Usage        *service.UsageService
	Audit        *service.AuditService
	Exports      *service.ExportService

	// TenantSource backs the DataLoader for the User.tenant field.
	TenantSource TenantSource

	MaxPageSize     int
	DefaultPageSize int

	Log *zap.Logger
}

// NewSchema builds the executable schema.
func NewSchema(r *Resolver) (graphql.Schema, error) {
	// User.tenant is defined after tenantType and userType exist, because the
	// two reference each other. AddFieldConfig is how graphql-go expresses a
	// cyclic type graph.
	userType := graphql.NewObject(graphql.ObjectConfig{
		Name:        "User",
		Description: "A person belonging to exactly one tenant. Password hashes are not part of this type.",
		Fields: graphql.Fields{
			"id":          &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"email":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"firstName":   &graphql.Field{Type: graphql.String},
			"lastName":    &graphql.Field{Type: graphql.String},
			"role":        &graphql.Field{Type: graphql.NewNonNull(roleEnum)},
			"isActive":    &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"lastLoginAt": &graphql.Field{Type: graphql.DateTime},
			"createdAt":   &graphql.Field{Type: graphql.NewNonNull(graphql.DateTime)},
			"updatedAt":   &graphql.Field{Type: graphql.NewNonNull(graphql.DateTime)},
		},
	})

	userType.AddFieldConfig("tenant", &graphql.Field{
		Type:        tenantType,
		Description: "The user's tenant, batch-loaded to avoid an N+1 query.",
		Resolve:     r.userTenant,
	})

	userConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "UserConnection",
		Fields: graphql.Fields{
			"nodes":    &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(userType)))},
			"pageInfo": &graphql.Field{Type: graphql.NewNonNull(pageInfoType)},
		},
	})

	usageConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "UsageConnection",
		Fields: graphql.Fields{
			"nodes":    &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(usageRecordType)))},
			"pageInfo": &graphql.Field{Type: graphql.NewNonNull(pageInfoType)},
		},
	})

	auditConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "AuditConnection",
		Fields: graphql.Fields{
			"nodes":    &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(auditEntryType)))},
			"pageInfo": &graphql.Field{Type: graphql.NewNonNull(pageInfoType)},
		},
	})

	exportConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ExportJobConnection",
		Fields: graphql.Fields{
			"nodes":    &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(exportJobType)))},
			"pageInfo": &graphql.Field{Type: graphql.NewNonNull(pageInfoType)},
		},
	})

	queryType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"me": &graphql.Field{
				Type:        userType,
				Description: "The authenticated user. Null for API-key credentials, which authenticate a tenant rather than a person.",
				Resolve:     r.requireAuth(r.me),
			},
			"tenant": &graphql.Field{
				Type:        graphql.NewNonNull(tenantType),
				Description: "The caller's own tenant. There is no argument to request a different one.",
				Resolve:     r.requireAuth(r.tenant),
			},
			"users": &graphql.Field{
				Type:        graphql.NewNonNull(userConnectionType),
				Args:        paginationArgs(nil),
				Description: "Users in the caller's tenant.",
				Resolve:     r.requireAuth(r.users),
			},
			"user": &graphql.Field{
				Type:        userType,
				Args:        graphql.FieldConfigArgument{"id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)}},
				Description: "One user in the caller's tenant. An ID from another tenant resolves to null.",
				Resolve:     r.requireAuth(r.user),
			},
			"features": &graphql.Field{
				Type:        graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(featureType))),
				Description: "The full feature catalog.",
				Resolve:     r.requireAuth(r.features),
			},
			"myFeatures": &graphql.Field{
				Type:        graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(graphql.String))),
				Description: "Feature keys enabled for the caller's tenant.",
				Resolve:     r.requireAuth(r.myFeatures),
			},
			"apiKeys": &graphql.Field{
				Type:        graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(apiKeyType))),
				Description: "API keys belonging to the caller's tenant.",
				Resolve:     r.requiresRole(models.RoleDeveloper, r.apiKeys),
			},
			"usage": &graphql.Field{
				Type: graphql.NewNonNull(usageConnectionType),
				Args: paginationArgs(graphql.FieldConfigArgument{
					"endpoint": &graphql.ArgumentConfig{Type: graphql.String},
					"method":   &graphql.ArgumentConfig{Type: graphql.String},
					"from":     &graphql.ArgumentConfig{Type: graphql.DateTime},
					"to":       &graphql.ArgumentConfig{Type: graphql.DateTime},
				}),
				Description: "Metered requests for the caller's tenant.",
				Resolve:     r.requireAuth(r.usage),
			},
			"usageSummary": &graphql.Field{
				Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(usageSummaryType))),
				Args: graphql.FieldConfigArgument{
					"from": &graphql.ArgumentConfig{Type: graphql.DateTime},
					"to":   &graphql.ArgumentConfig{Type: graphql.DateTime},
				},
				Resolve: r.requireAuth(r.usageSummary),
			},
			"auditLogs": &graphql.Field{
				Type: graphql.NewNonNull(auditConnectionType),
				Args: paginationArgs(graphql.FieldConfigArgument{
					"event": &graphql.ArgumentConfig{Type: graphql.String},
				}),
				Description: "The caller tenant's audit trail.",
				Resolve:     r.requiresRole(models.RoleDeveloper, r.auditLogs),
			},
			"exportJobs": &graphql.Field{
				Type:    graphql.NewNonNull(exportConnectionType),
				Args:    paginationArgs(nil),
				Resolve: r.requiresFeature("usage.export", r.exportJobs),
			},
			"exportJob": &graphql.Field{
				Type:    exportJobType,
				Args:    graphql.FieldConfigArgument{"id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)}},
				Resolve: r.requiresFeature("usage.export", r.exportJob),
			},
			"exportDownload": &graphql.Field{
				Type:        downloadType,
				Args:        graphql.FieldConfigArgument{"id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)}},
				Description: "A freshly minted, expiring download URL for a completed export.",
				Resolve:     r.requiresFeature("usage.export", r.exportDownload),
			},
		},
	})

	mutationType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Mutation",
		Fields: graphql.Fields{
			"updateTenant": &graphql.Field{
				Type:        graphql.NewNonNull(tenantType),
				Args:        graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(updateTenantInput)}},
				Description: "Update the caller's own tenant profile. Equivalent to @requiresRole(role: ADMIN).",
				Resolve:     r.requiresRole(models.RoleAdmin, r.updateTenant),
			},
			"updateUserRole": &graphql.Field{
				Type:        graphql.NewNonNull(userType),
				Args:        graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(updateUserRoleInput)}},
				Description: "Change a user's role. Equivalent to @requiresRole(role: ADMIN).",
				Resolve:     r.requiresRole(models.RoleAdmin, r.updateUserRole),
			},
			"generateApiKey": &graphql.Field{
				Type:        graphql.NewNonNull(generatedAPIKeyType),
				Args:        graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: generateAPIKeyInput}},
				Description: "Equivalent to @requiresRole(role: DEVELOPER).",
				Resolve:     r.requiresRole(models.RoleDeveloper, r.generateAPIKey),
			},
			"rotateApiKey": &graphql.Field{
				Type:    graphql.NewNonNull(generatedAPIKeyType),
				Args:    graphql.FieldConfigArgument{"id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)}},
				Resolve: r.requiresRole(models.RoleDeveloper, r.rotateAPIKey),
			},
			"deactivateApiKey": &graphql.Field{
				Type:    graphql.NewNonNull(graphql.Boolean),
				Args:    graphql.FieldConfigArgument{"id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)}},
				Resolve: r.requiresRole(models.RoleDeveloper, r.deactivateAPIKey),
			},
			"requestUsageExport": &graphql.Field{
				Type:        graphql.NewNonNull(exportJobType),
				Args:        graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: requestExportInput}},
				Description: "Equivalent to @requiresFeature(feature: \"usage.export\").",
				Resolve:     r.requiresFeature("usage.export", r.requestUsageExport),
			},
		},
	})

	return graphql.NewSchema(graphql.SchemaConfig{
		Query:    queryType,
		Mutation: mutationType,
	})
}

// ---------------------------------------------------------------------
// Query resolvers
// ---------------------------------------------------------------------

func (r *Resolver) me(p graphql.ResolveParams) (interface{}, error) {
	scope := middleware.ScopeFromContext(p.Context)
	if scope.UserID == "" {
		// An API key authenticates a tenant, not a person: null is the
		// truthful answer, not an error.
		return nil, nil
	}
	user, err := r.Users.Me(p.Context, scope)
	if err != nil {
		return nil, err
	}
	return model.NewUser(user), nil
}

func (r *Resolver) tenant(p graphql.ResolveParams) (interface{}, error) {
	tenant, err := r.Tenants.Current(p.Context, middleware.ScopeFromContext(p.Context))
	if err != nil {
		return nil, err
	}
	return model.NewTenant(tenant), nil
}

func (r *Resolver) users(p graphql.ResolveParams) (interface{}, error) {
	scope := middleware.ScopeFromContext(p.Context)
	page, pageSize := r.pagination(p)

	users, total, err := r.Users.List(p.Context, scope, page, pageSize)
	if err != nil {
		return nil, err
	}

	nodes := model.NewUsers(users)

	// Prime the tenant loader with every tenant this page references, so the
	// nested tenant field costs one query for the whole page instead of one
	// per row.
	if loaders := LoadersFrom(p.Context); loaders != nil && len(nodes) > 0 {
		ids := make([]string, 0, len(nodes))
		for _, u := range nodes {
			ids = append(ids, u.TenantID)
		}
		if err := loaders.Tenant.Prime(p.Context, ids); err != nil {
			// Priming is an optimisation; the per-row fallback still returns
			// correct data, so a failure here is logged, not fatal.
			r.Log.Warn("failed to prime the tenant loader", zap.Error(err))
		}
	}

	return &model.UserConnection{
		Nodes:    nodes,
		PageInfo: model.NewPageInfo(page, pageSize, total),
	}, nil
}

func (r *Resolver) user(p graphql.ResolveParams) (interface{}, error) {
	id, _ := p.Args["id"].(string)

	user, err := r.Users.Get(p.Context, middleware.ScopeFromContext(p.Context), id)
	if err != nil {
		// A cross-tenant or unknown ID is null rather than an error, matching
		// the REST 404 and revealing nothing about which IDs exist.
		if apierr.Is(err, apierr.CodeNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return model.NewUser(user), nil
}

// userTenant resolves User.tenant through the DataLoader.
func (r *Resolver) userTenant(p graphql.ResolveParams) (interface{}, error) {
	user, ok := p.Source.(*model.User)
	if !ok || user.TenantID == "" {
		return nil, nil
	}

	loaders := LoadersFrom(p.Context)
	if loaders == nil {
		// No loader on the context (a direct schema execution in a test):
		// fall back to the service so behaviour is still correct.
		tenant, err := r.Tenants.Current(p.Context, middleware.ScopeFromContext(p.Context))
		if err != nil {
			return nil, err
		}
		return model.NewTenant(tenant), nil
	}
	return loaders.Tenant.Load(p.Context, user.TenantID)
}

func (r *Resolver) features(p graphql.ResolveParams) (interface{}, error) {
	features, err := r.Entitlements.ListFeatureCatalog(p.Context)
	if err != nil {
		return nil, apierr.Internal("failed to list features").Wrap(err)
	}
	return model.NewFeatures(features), nil
}

func (r *Resolver) myFeatures(p graphql.ResolveParams) (interface{}, error) {
	ac, _ := middleware.AuthFromContext(p.Context)

	keys, err := r.Entitlements.ListEnabledFeatures(p.Context, ac.TenantID, ac.TenantPlan)
	if err != nil {
		return nil, apierr.Internal("failed to resolve entitlements").Wrap(err)
	}
	if keys == nil {
		keys = []string{}
	}
	return keys, nil
}

func (r *Resolver) apiKeys(p graphql.ResolveParams) (interface{}, error) {
	scope := middleware.ScopeFromContext(p.Context)

	keys, err := r.APIKeys.ListByTenant(p.Context, scope.TenantID)
	if err != nil {
		return nil, apierr.Internal("failed to list API keys").Wrap(err)
	}
	return model.NewAPIKeys(keys), nil
}

func (r *Resolver) usage(p graphql.ResolveParams) (interface{}, error) {
	scope := middleware.ScopeFromContext(p.Context)
	page, pageSize := r.pagination(p)

	endpoint, _ := p.Args["endpoint"].(string)
	method, _ := p.Args["method"].(string)
	from := timeArg(p, "from")
	to := timeArg(p, "to")

	records, total, err := r.Usage.List(p.Context, scope.TenantID, endpoint, method, from, to, page, pageSize)
	if err != nil {
		return nil, apierr.Internal("failed to list usage records").Wrap(err)
	}

	return &model.UsageConnection{
		Nodes:    model.NewUsageRecords(records),
		PageInfo: model.NewPageInfo(page, pageSize, total),
	}, nil
}

func (r *Resolver) usageSummary(p graphql.ResolveParams) (interface{}, error) {
	scope := middleware.ScopeFromContext(p.Context)

	summary, err := r.Usage.Summary(p.Context, scope.TenantID, timeArg(p, "from"), timeArg(p, "to"))
	if err != nil {
		return nil, apierr.Internal("failed to summarize usage").Wrap(err)
	}
	return model.NewUsageSummaries(summary), nil
}

func (r *Resolver) auditLogs(p graphql.ResolveParams) (interface{}, error) {
	scope := middleware.ScopeFromContext(p.Context)
	page, pageSize := r.pagination(p)
	event, _ := p.Args["event"].(string)

	entries, total, err := r.Audit.List(p.Context, scope.TenantID, event, page, pageSize)
	if err != nil {
		return nil, apierr.Internal("failed to list audit logs").Wrap(err)
	}

	nodes := make([]*model.AuditEntry, 0, len(entries))
	for _, e := range entries {
		metadata := "{}"
		if len(e.Metadata) > 0 {
			if raw, err := json.Marshal(e.Metadata); err == nil {
				metadata = string(raw)
			}
		}
		nodes = append(nodes, &model.AuditEntry{
			ID:        e.ID,
			Event:     e.Event,
			ActorID:   e.ActorID,
			Endpoint:  e.Endpoint,
			IPAddress: e.IPAddress,
			UserAgent: e.UserAgent,
			Metadata:  metadata,
			CreatedAt: e.CreatedAt,
		})
	}

	return &model.AuditConnection{
		Nodes:    nodes,
		PageInfo: model.NewPageInfo(page, pageSize, total),
	}, nil
}

func (r *Resolver) exportJobs(p graphql.ResolveParams) (interface{}, error) {
	scope := middleware.ScopeFromContext(p.Context)
	page, pageSize := r.pagination(p)

	jobs, total, err := r.Exports.List(p.Context, scope, page, pageSize)
	if err != nil {
		return nil, err
	}
	return &model.ExportJobConnection{
		Nodes:    model.NewExportJobs(jobs),
		PageInfo: model.NewPageInfo(page, pageSize, total),
	}, nil
}

func (r *Resolver) exportJob(p graphql.ResolveParams) (interface{}, error) {
	id, _ := p.Args["id"].(string)

	job, err := r.Exports.Get(p.Context, middleware.ScopeFromContext(p.Context), id)
	if err != nil {
		if apierr.Is(err, apierr.CodeNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return model.NewExportJob(job), nil
}

func (r *Resolver) exportDownload(p graphql.ResolveParams) (interface{}, error) {
	id, _ := p.Args["id"].(string)

	download, err := r.Exports.DownloadURL(p.Context, middleware.ScopeFromContext(p.Context), id)
	if err != nil {
		return nil, err
	}
	return &model.Download{
		URL:       download.URL,
		ExpiresAt: download.ExpiresAt,
		RowCount:  download.RowCount,
		SizeBytes: download.SizeBytes,
	}, nil
}

// ---------------------------------------------------------------------
// Mutation resolvers
// ---------------------------------------------------------------------

func (r *Resolver) updateTenant(p graphql.ResolveParams) (interface{}, error) {
	scope := middleware.ScopeFromContext(p.Context)
	input, _ := p.Args["input"].(map[string]interface{})

	var namePtr *string
	if name, ok := input["name"].(string); ok {
		namePtr = &name
	}

	tenant, err := r.Tenants.UpdateForScope(p.Context, scope, scope.TenantID, service.UpdateTenantSelfInput{Name: namePtr})
	if err != nil {
		return nil, err
	}

	r.audit(p, models.EventTenantUpdated, map[string]any{"transport": "graphql"})
	return model.NewTenant(tenant), nil
}

func (r *Resolver) updateUserRole(p graphql.ResolveParams) (interface{}, error) {
	scope := middleware.ScopeFromContext(p.Context)
	input, _ := p.Args["input"].(map[string]interface{})

	userID, _ := input["userId"].(string)
	roleRaw, _ := input["role"].(string)

	result, err := r.Users.UpdateRole(p.Context, scope, userID, models.Role(roleRaw))
	if err != nil {
		return nil, err
	}

	r.audit(p, models.EventRoleChanged, map[string]any{
		"transport":      "graphql",
		"target_user_id": userID,
		"previous_role":  string(result.PreviousRole),
		"new_role":       string(result.NewRole),
	})
	return model.NewUser(result.User), nil
}

func (r *Resolver) generateAPIKey(p graphql.ResolveParams) (interface{}, error) {
	scope := middleware.ScopeFromContext(p.Context)

	name := ""
	if input, ok := p.Args["input"].(map[string]interface{}); ok {
		name, _ = input["name"].(string)
	}

	plaintext, key, err := r.APIKeys.Generate(p.Context, scope.TenantID, ptr(scope.UserID), name)
	if err != nil {
		return nil, apierr.Internal("failed to generate the API key").Wrap(err)
	}

	r.audit(p, models.EventAPIKeyGenerated, map[string]any{"transport": "graphql", "api_key_id": key.ID})

	return &model.GeneratedAPIKey{APIKey: model.NewAPIKey(key), Plaintext: plaintext}, nil
}

func (r *Resolver) rotateAPIKey(p graphql.ResolveParams) (interface{}, error) {
	scope := middleware.ScopeFromContext(p.Context)
	id, _ := p.Args["id"].(string)

	plaintext, key, err := r.APIKeys.Rotate(p.Context, scope.TenantID, id, ptr(scope.UserID))
	if err != nil {
		return nil, mapAPIKeyError(err)
	}

	r.audit(p, models.EventAPIKeyRotated, map[string]any{
		"transport":  "graphql",
		"old_key_id": id,
		"new_key_id": key.ID,
	})
	return &model.GeneratedAPIKey{APIKey: model.NewAPIKey(key), Plaintext: plaintext}, nil
}

func (r *Resolver) deactivateAPIKey(p graphql.ResolveParams) (interface{}, error) {
	scope := middleware.ScopeFromContext(p.Context)
	id, _ := p.Args["id"].(string)

	if err := r.APIKeys.Deactivate(p.Context, scope.TenantID, id); err != nil {
		return nil, mapAPIKeyError(err)
	}

	r.audit(p, models.EventAPIKeyRevoked, map[string]any{"transport": "graphql", "api_key_id": id})
	return true, nil
}

func (r *Resolver) requestUsageExport(p graphql.ResolveParams) (interface{}, error) {
	scope := middleware.ScopeFromContext(p.Context)

	in := service.RequestExportInput{}
	if input, ok := p.Args["input"].(map[string]interface{}); ok {
		in.Endpoint, _ = input["endpoint"].(string)
		in.Method, _ = input["method"].(string)
		if from, ok := input["from"].(time.Time); ok {
			in.From = &from
		}
		if to, ok := input["to"].(time.Time); ok {
			in.To = &to
		}
	}

	job, err := r.Exports.Request(p.Context, scope, in)
	if err != nil {
		return nil, err
	}

	r.audit(p, models.EventExportRequested, map[string]any{"transport": "graphql", "export_job_id": job.ID})
	return model.NewExportJob(job), nil
}

// ---------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------

// pagination reads and clamps the page arguments. Clamping here, rather than
// trusting the argument, is what makes the complexity analyzer's worst-case
// assumption true in practice.
func (r *Resolver) pagination(p graphql.ResolveParams) (page, pageSize int) {
	page = 1
	pageSize = r.DefaultPageSize
	if pageSize <= 0 {
		pageSize = 20
	}

	if v, ok := p.Args["page"].(int); ok && v > 0 {
		page = v
	}
	if v, ok := p.Args["pageSize"].(int); ok && v > 0 {
		pageSize = v
	}

	max := r.MaxPageSize
	if max <= 0 {
		max = 100
	}
	if pageSize > max {
		pageSize = max
	}
	return page, pageSize
}

func timeArg(p graphql.ResolveParams, name string) *time.Time {
	if v, ok := p.Args[name].(time.Time); ok {
		return &v
	}
	return nil
}

func ptr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (r *Resolver) audit(p graphql.ResolveParams, event string, metadata map[string]any) {
	scope := middleware.ScopeFromContext(p.Context)
	r.Audit.Record(p.Context, service.RecordInput{
		TenantID: ptr(scope.TenantID),
		ActorID:  ptr(scope.UserID),
		Event:    event,
		Endpoint: "graphql:" + fieldName(p),
		Metadata: metadata,
	})
}

func mapAPIKeyError(err error) error {
	if err == nil {
		return nil
	}
	if apierr.Is(err, apierr.CodeInternal) && errorsIs(err, service.ErrAPIKeyNotFound) {
		return apierr.NotFound("API key not found")
	}
	if errorsIs(err, service.ErrAPIKeyNotFound) {
		return apierr.NotFound("API key not found")
	}
	return apierr.Internal("the API key operation failed").Wrap(err)
}
