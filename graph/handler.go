package graph

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/graphql-go/graphql"
	"go.uber.org/zap"

	"github.com/bridgecore/bridgecore/internal/middleware"
	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/service"
	"github.com/bridgecore/bridgecore/pkg/apierr"
	"github.com/bridgecore/bridgecore/pkg/response"
)

// Handler serves the GraphQL endpoint.
//
// It sits behind exactly the same middleware chain as the REST routes —
// request ID, recovery, logging, authentication, rate limiting, usage
// metering — so a GraphQL request is metered, rate-limited, correlated and
// audited identically to a REST one. The GraphQL-specific work is what
// happens between the middleware and execution: reject the document if it is
// too large, too deep or too expensive, and attach a fresh DataLoader set.
type Handler struct {
	schema  graphql.Schema
	loaders TenantSource
	limits  Limits
	audit   *service.AuditService
	log     *zap.Logger

	playgroundEnabled bool
	path              string
}

// HandlerConfig configures the GraphQL transport.
type HandlerConfig struct {
	Schema            graphql.Schema
	TenantSource      TenantSource
	Limits            Limits
	Audit             *service.AuditService
	Log               *zap.Logger
	PlaygroundEnabled bool
	Path              string
}

func NewHandler(cfg HandlerConfig) *Handler {
	return &Handler{
		schema:            cfg.Schema,
		loaders:           cfg.TenantSource,
		limits:            cfg.Limits,
		audit:             cfg.Audit,
		log:               cfg.Log,
		playgroundEnabled: cfg.PlaygroundEnabled,
		path:              cfg.Path,
	}
}

type graphQLRequest struct {
	Query         string                 `json:"query"`
	OperationName string                 `json:"operationName"`
	Variables     map[string]interface{} `json:"variables"`
}

// maxGraphQLBodyBytes bounds the request body independently of the document
// limit, so an enormous variables blob is rejected too.
const maxGraphQLBodyBytes = 1 << 20

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A GET on the endpoint serves the in-browser IDE in non-production
	// environments. Queries are accepted over POST only: a GraphQL query in a
	// URL ends up in access logs, browser history, and proxy caches, and a
	// mutation over GET is trivially CSRF-able.
	if r.Method == http.MethodGet {
		if !h.playgroundEnabled {
			response.Fail(w, r, apierr.NotFound("not found"))
			return
		}
		h.servePlayground(w)
		return
	}

	if r.Method != http.MethodPost {
		response.Fail(w, r, apierr.BadRequest("GraphQL requests must use POST"))
		return
	}

	var req graphQLRequest
	body := http.MaxBytesReader(w, r.Body, maxGraphQLBodyBytes)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		response.Fail(w, r, apierr.BadRequest("the GraphQL request body could not be parsed"))
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		response.Fail(w, r, apierr.BadRequest("a GraphQL query is required"))
		return
	}

	// Cost analysis happens before execution: this is the only point at which
	// refusing an abusive document is cheap.
	analysis, err := Check(req.Query, h.limits)
	if err != nil {
		h.auditRejection(r, analysis, err)
		h.writeGraphQLError(w, err)
		return
	}

	ctx := WithLoaders(r.Context(), NewLoaders(h.loaders))

	result := graphql.Do(graphql.Params{
		Schema:         h.schema,
		RequestString:  req.Query,
		VariableValues: req.Variables,
		OperationName:  req.OperationName,
		Context:        ctx,
	})

	if len(result.Errors) > 0 {
		// GraphQL reports field-level failures in the errors array with a 200,
		// which is the specified behaviour: a partial result is still a
		// result. The errors are logged here so a 200 does not hide a 500.
		h.log.Warn("graphql execution returned errors",
			zap.String("request_id", middleware.RequestIDFromContext(r.Context())),
			zap.String("operation", req.OperationName),
			zap.Int("error_count", len(result.Errors)),
			zap.Int("complexity", analysis.Cost),
			zap.Any("errors", result.Errors),
		)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-GraphQL-Complexity", itoa(analysis.Cost))
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(result); err != nil {
		h.log.Error("failed to encode the GraphQL response", zap.Error(err))
	}
}

// writeGraphQLError renders a pre-execution rejection in the GraphQL error
// shape, so a client's normal error handling path works even for a document
// that never ran.
func (h *Handler) writeGraphQLError(w http.ResponseWriter, err error) {
	e := apierr.From(err)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": nil,
		"errors": []map[string]any{{
			"message":    e.Message,
			"extensions": e.Extensions(),
		}},
	})
}

func (h *Handler) auditRejection(r *http.Request, analysis Analysis, err error) {
	scope := middleware.ScopeFromContext(r.Context())
	var tenantPtr, actorPtr *string
	if scope.TenantID != "" {
		id := scope.TenantID
		tenantPtr = &id
	}
	if scope.UserID != "" {
		id := scope.UserID
		actorPtr = &id
	}

	h.audit.Record(r.Context(), service.RecordInput{
		TenantID: tenantPtr,
		ActorID:  actorPtr,
		Event:    models.EventGraphQLRejected,
		Endpoint: h.path,
		Metadata: map[string]any{
			"reason":     apierr.From(err).Message,
			"depth":      analysis.Depth,
			"complexity": analysis.Cost,
			"fields":     analysis.Fields,
		},
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// servePlayground renders a minimal GraphiQL page. It is served only when
// GRAPHQL_PLAYGROUND is enabled, which config.Validate forbids in production.
func (h *Handler) servePlayground(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(playgroundHTML(h.path)))
}

func playgroundHTML(path string) string {
	return `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <title>BridgeCore GraphQL</title>
  <link rel="stylesheet" href="https://unpkg.com/graphiql@3/graphiql.min.css" />
  <style>
    html, body, #graphiql { height: 100%; margin: 0; }
    .bc-banner { font: 13px system-ui, sans-serif; background:#1f2937; color:#e5e7eb; padding:8px 12px; }
    .bc-banner code { background:#111827; padding:1px 4px; border-radius:3px; }
  </style>
</head>
<body>
  <div class="bc-banner">
    BridgeCore GraphQL &mdash; send an <code>Authorization: Bearer &lt;access token&gt;</code>
    header (Headers tab) after logging in via <code>POST /api/v1/auth/login</code>.
  </div>
  <div id="graphiql">Loading…</div>
  <script src="https://unpkg.com/react@18/umd/react.production.min.js"></script>
  <script src="https://unpkg.com/react-dom@18/umd/react-dom.production.min.js"></script>
  <script src="https://unpkg.com/graphiql@3/graphiql.min.js"></script>
  <script>
    const fetcher = GraphiQL.createFetcher({ url: '` + path + `' });
    ReactDOM.createRoot(document.getElementById('graphiql'))
      .render(React.createElement(GraphiQL, { fetcher: fetcher }));
  </script>
</body>
</html>`
}
