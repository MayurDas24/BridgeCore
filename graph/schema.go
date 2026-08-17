// Package graph implements BridgeCore's GraphQL API.
//
// The critical design constraint is that GraphQL is a second *transport*, not
// a second application. Resolvers call exactly the same services the REST
// handlers call, and never touch a repository or the database directly:
//
//	REST handler ------+
//	                   |
//	GraphQL resolver --+---> Service ---> Repository ---> PostgreSQL / Redis
//	                   |
//	Export worker -----+
//
// That is what makes the two APIs impossible to drift apart on authorization,
// tenant isolation, or validation: there is only one implementation of each.
//
// The schema is defined programmatically rather than generated from SDL. The
// SDL under graph/schema/ is kept as the human-readable contract and is what
// clients are given; the Go definitions here are what executes. Building the
// schema in code means it is compiled and type-checked with the rest of the
// service — there is no generated file to regenerate, no codegen step in the
// build, and a resolver whose signature stops matching its field is a
// compile error rather than a runtime surprise.
package graph

import (
	"github.com/graphql-go/graphql"

	"github.com/bridgecore/bridgecore/internal/models"
)

// ---------------------------------------------------------------------
// Enums
// ---------------------------------------------------------------------

var roleEnum = graphql.NewEnum(graphql.EnumConfig{
	Name:        "Role",
	Description: "Platform RBAC role. Ordering is admin > developer > viewer.",
	Values: graphql.EnumValueConfigMap{
		"ADMIN":     &graphql.EnumValueConfig{Value: string(models.RoleAdmin)},
		"DEVELOPER": &graphql.EnumValueConfig{Value: string(models.RoleDeveloper)},
		"VIEWER":    &graphql.EnumValueConfig{Value: string(models.RoleViewer)},
	},
})

var planEnum = graphql.NewEnum(graphql.EnumConfig{
	Name:        "Plan",
	Description: "Subscription tier, which determines a tenant's default feature entitlements.",
	Values: graphql.EnumValueConfigMap{
		"FREE":       &graphql.EnumValueConfig{Value: string(models.PlanFree)},
		"PRO":        &graphql.EnumValueConfig{Value: string(models.PlanPro)},
		"ENTERPRISE": &graphql.EnumValueConfig{Value: string(models.PlanEnterprise)},
	},
})

var exportStatusEnum = graphql.NewEnum(graphql.EnumConfig{
	Name:        "ExportStatus",
	Description: "Lifecycle state of an asynchronous usage export.",
	Values: graphql.EnumValueConfigMap{
		"QUEUED":     &graphql.EnumValueConfig{Value: string(models.ExportStatusQueued)},
		"PROCESSING": &graphql.EnumValueConfig{Value: string(models.ExportStatusProcessing)},
		"COMPLETED":  &graphql.EnumValueConfig{Value: string(models.ExportStatusCompleted)},
		"FAILED":     &graphql.EnumValueConfig{Value: string(models.ExportStatusFailed)},
	},
})

// ---------------------------------------------------------------------
// Shared object types
// ---------------------------------------------------------------------

var pageInfoType = graphql.NewObject(graphql.ObjectConfig{
	Name:        "PageInfo",
	Description: "Pagination envelope shared by every connection type.",
	Fields: graphql.Fields{
		"page":       &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"pageSize":   &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"totalPages": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"hasNext":    &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
	},
})

var tenantType = graphql.NewObject(graphql.ObjectConfig{
	Name:        "Tenant",
	Description: "A customer organization. Every other object is scoped to exactly one.",
	Fields: graphql.Fields{
		"id":        &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"name":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"slug":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"plan":      &graphql.Field{Type: graphql.NewNonNull(planEnum)},
		"isActive":  &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		"createdAt": &graphql.Field{Type: graphql.NewNonNull(graphql.DateTime)},
		"updatedAt": &graphql.Field{Type: graphql.NewNonNull(graphql.DateTime)},
	},
})

var featureType = graphql.NewObject(graphql.ObjectConfig{
	Name:        "Feature",
	Description: "A gate-able platform capability, identified by a stable key.",
	Fields: graphql.Fields{
		"id":          &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"key":         &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"name":        &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"description": &graphql.Field{Type: graphql.String},
	},
})

// apiKeyType deliberately has no hash field. See graph/model: the GraphQL
// view of an API key is a separate struct from the database model precisely
// so that key_hash has nowhere to appear.
var apiKeyType = graphql.NewObject(graphql.ObjectConfig{
	Name:        "APIKey",
	Description: "A tenant-scoped machine credential. The secret is never readable after creation.",
	Fields: graphql.Fields{
		"id":         &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"name":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"prefix":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"lastFour":   &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"isActive":   &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		"lastUsedAt": &graphql.Field{Type: graphql.DateTime},
		"expiresAt":  &graphql.Field{Type: graphql.DateTime},
		"revokedAt":  &graphql.Field{Type: graphql.DateTime},
		"createdAt":  &graphql.Field{Type: graphql.NewNonNull(graphql.DateTime)},
	},
})

var generatedAPIKeyType = graphql.NewObject(graphql.ObjectConfig{
	Name: "GeneratedAPIKey",
	Description: "Returned once, at creation or rotation. The plaintext value is not " +
		"stored and cannot be retrieved again.",
	Fields: graphql.Fields{
		"apiKey":    &graphql.Field{Type: graphql.NewNonNull(apiKeyType)},
		"plaintext": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
	},
})

var usageRecordType = graphql.NewObject(graphql.ObjectConfig{
	Name:        "UsageRecord",
	Description: "One metered API request.",
	Fields: graphql.Fields{
		"id":         &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"endpoint":   &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"method":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"statusCode": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"latencyMs":  &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"requestId":  &graphql.Field{Type: graphql.String},
		"createdAt":  &graphql.Field{Type: graphql.NewNonNull(graphql.DateTime)},
	},
})

var usageSummaryType = graphql.NewObject(graphql.ObjectConfig{
	Name:        "UsageSummary",
	Description: "Aggregated usage for one endpoint and method.",
	Fields: graphql.Fields{
		"endpoint":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"method":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"requestCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"errorCount":   &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"avgLatencyMs": &graphql.Field{Type: graphql.NewNonNull(graphql.Float)},
	},
})

var auditEntryType = graphql.NewObject(graphql.ObjectConfig{
	Name:        "AuditEntry",
	Description: "An immutable record of a security- or business-relevant action.",
	Fields: graphql.Fields{
		"id":        &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"event":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"actorId":   &graphql.Field{Type: graphql.ID},
		"endpoint":  &graphql.Field{Type: graphql.String},
		"ipAddress": &graphql.Field{Type: graphql.String},
		"userAgent": &graphql.Field{Type: graphql.String},
		"metadata":  &graphql.Field{Type: graphql.String, Description: "Event metadata as a JSON string."},
		"createdAt": &graphql.Field{Type: graphql.NewNonNull(graphql.DateTime)},
	},
})

var exportJobType = graphql.NewObject(graphql.ObjectConfig{
	Name:        "ExportJob",
	Description: "An asynchronous usage export.",
	Fields: graphql.Fields{
		"id":         &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"status":     &graphql.Field{Type: graphql.NewNonNull(exportStatusEnum)},
		"endpoint":   &graphql.Field{Type: graphql.String},
		"method":     &graphql.Field{Type: graphql.String},
		"rowCount":   &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"sizeBytes":  &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"attempts":   &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"error":      &graphql.Field{Type: graphql.String},
		"startedAt":  &graphql.Field{Type: graphql.DateTime},
		"finishedAt": &graphql.Field{Type: graphql.DateTime},
		"createdAt":  &graphql.Field{Type: graphql.NewNonNull(graphql.DateTime)},
	},
})

var downloadType = graphql.NewObject(graphql.ObjectConfig{
	Name:        "Download",
	Description: "A short-lived, expiring URL for a completed export.",
	Fields: graphql.Fields{
		"url":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"expiresAt": &graphql.Field{Type: graphql.NewNonNull(graphql.DateTime)},
		"rowCount":  &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"sizeBytes": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
	},
})

// ---------------------------------------------------------------------
// Input types
// ---------------------------------------------------------------------

var updateTenantInput = graphql.NewInputObject(graphql.InputObjectConfig{
	Name: "UpdateTenantInput",
	Description: "Fields a tenant may change about itself. Plan is absent by design: " +
		"a tenant that could set its own plan could grant itself any entitlement.",
	Fields: graphql.InputObjectConfigFieldMap{
		"name": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
	},
})

var updateUserRoleInput = graphql.NewInputObject(graphql.InputObjectConfig{
	Name: "UpdateUserRoleInput",
	Fields: graphql.InputObjectConfigFieldMap{
		"userId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		"role":   &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(roleEnum)},
	},
})

var generateAPIKeyInput = graphql.NewInputObject(graphql.InputObjectConfig{
	Name: "GenerateAPIKeyInput",
	Fields: graphql.InputObjectConfigFieldMap{
		"name": &graphql.InputObjectFieldConfig{Type: graphql.String},
	},
})

var requestExportInput = graphql.NewInputObject(graphql.InputObjectConfig{
	Name: "RequestExportInput",
	Fields: graphql.InputObjectConfigFieldMap{
		"endpoint": &graphql.InputObjectFieldConfig{Type: graphql.String},
		"method":   &graphql.InputObjectFieldConfig{Type: graphql.String},
		"from":     &graphql.InputObjectFieldConfig{Type: graphql.DateTime},
		"to":       &graphql.InputObjectFieldConfig{Type: graphql.DateTime},
	},
})

// paginationArgs is the argument set every connection field accepts. Page
// size is clamped by the resolver to the configured maximum, so there is no
// unbounded list anywhere in the schema.
func paginationArgs(extra graphql.FieldConfigArgument) graphql.FieldConfigArgument {
	args := graphql.FieldConfigArgument{
		"page":     &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 1},
		"pageSize": &graphql.ArgumentConfig{Type: graphql.Int},
	}
	for k, v := range extra {
		args[k] = v
	}
	return args
}
