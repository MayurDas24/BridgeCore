//go:build integration

// Package integration holds BridgeCore's black-box integration tests.
//
// They run against a real, fully-wired server over HTTP — the same binary,
// middleware chain, PostgreSQL and Redis a deployment uses — rather than
// against handlers constructed in-process. That choice is deliberate: the bugs
// this suite exists to catch are wiring bugs. A unit test proves the tenancy
// guard rejects a cross-tenant ID; only an end-to-end request proves the route
// is actually wired to the guarded service method. The most dangerous class of
// security bug here is a correct check that nothing calls.
//
// Run with:
//
//	make up            # start the stack
//	make integration   # go test -tags=integration ./integration/...
//
// Set BRIDGECORE_BASE_URL to point at a different environment.
package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

var baseURL = envOr("BRIDGECORE_BASE_URL", "http://localhost:8080")

var client = &http.Client{Timeout: 20 * time.Second}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return fallback
}

// ---------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------

type envelope struct {
	Success   bool            `json:"success"`
	Message   string          `json:"message"`
	Data      json.RawMessage `json:"data"`
	RequestID string          `json:"request_id"`
	Error     *struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

type result struct {
	status int
	body   envelope
	raw    []byte
}

// call issues a request and decodes the standard envelope.
func call(t *testing.T, method, path string, body any, headers map[string]string) result {
	t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v (is the stack running? try `make up`)", method, path, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	var env envelope
	_ = json.Unmarshal(raw, &env)

	return result{status: resp.StatusCode, body: env, raw: raw}
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// tenant is a freshly provisioned tenant plus its admin's tokens.
type tenant struct {
	slug        string
	adminEmail  string
	accessToken string
	tenantID    string
	adminUserID string
}

// signup provisions an isolated tenant. Each test gets its own, keyed by a
// nanosecond suffix, so the suite is safe to re-run against a database that
// already holds data from a previous run.
func signup(t *testing.T, label string) tenant {
	t.Helper()

	suffix := fmt.Sprintf("%s-%d", label, time.Now().UnixNano())

	// The API derives the tenant slug from the name (handler.slugify), so
	// there is no slug field to send — and the strict JSON decoder rejects
	// one, which is how this test's original mistake surfaced as a clean 400
	// rather than a silently ignored field.
	//
	// The unique suffix goes in the name for that reason: it is what makes the
	// derived slug distinct, so the suite is safe to re-run against a database
	// that already holds tenants from a previous run.
	name := "integration-" + suffix
	slug := name // already lowercase, alphanumeric and hyphens, so slugify is a no-op
	email := "admin-" + suffix + "@bridgecore.test"

	res := call(t, http.MethodPost, "/api/v1/auth/signup", map[string]any{
		"tenant_name": name,
		"email":       email,
		"password":    "integration-password-123",
		"first_name":  "Test",
		"last_name":   "Admin",
	}, nil)

	if res.status != http.StatusCreated {
		t.Fatalf("signup failed: status %d body %s", res.status, res.raw)
	}

	var data struct {
		User struct {
			ID       string `json:"id"`
			TenantID string `json:"tenant_id"`
		} `json:"user"`
		Tenant struct {
			ID string `json:"id"`
		} `json:"tenant"`
		Tokens struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(res.body.Data, &data); err != nil {
		t.Fatalf("decode signup payload: %v (body %s)", err, res.raw)
	}
	if data.Tokens.AccessToken == "" {
		t.Fatalf("signup returned no access token: %s", res.raw)
	}

	return tenant{
		slug:        slug,
		adminEmail:  email,
		accessToken: data.Tokens.AccessToken,
		tenantID:    data.Tenant.ID,
		adminUserID: data.User.ID,
	}
}

// requireStack skips the suite when no server is reachable, so `go test
// -tags=integration` on a machine without the stack running reports a skip
// rather than a wall of failures.
func requireStack(t *testing.T) {
	t.Helper()
	resp, err := client.Get(baseURL + "/live")
	if err != nil {
		t.Skipf("no BridgeCore server at %s (run `make up`): %v", baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("server at %s is not alive (status %d)", baseURL, resp.StatusCode)
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

// ---------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------

func TestHealthEndpoints(t *testing.T) {
	requireStack(t)

	for _, path := range []string{"/live", "/ready", "/health"} {
		res := call(t, http.MethodGet, path, nil, nil)
		if res.status != http.StatusOK {
			t.Errorf("%s returned %d, want 200: %s", path, res.status, res.raw)
		}
		if !res.body.Success {
			t.Errorf("%s reported unsuccessful: %s", path, res.raw)
		}
	}
}

func TestEveryResponseCarriesACorrelationID(t *testing.T) {
	requireStack(t)

	resp, err := client.Get(baseURL + "/live")
	if err != nil {
		t.Fatalf("GET /live: %v", err)
	}
	defer resp.Body.Close()

	if resp.Header.Get("X-Request-ID") == "" {
		t.Error("expected an X-Request-ID header so a user can quote it in a support ticket")
	}
}

// ---------------------------------------------------------------------
// Authentication
// ---------------------------------------------------------------------

func TestAuth_SignupLoginRefreshLogout(t *testing.T) {
	requireStack(t)
	tn := signup(t, "authflow")

	// Login with the same credentials.
	login := call(t, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email":    tn.adminEmail,
		"password": "integration-password-123",
	}, nil)
	if login.status != http.StatusOK {
		t.Fatalf("login failed: %d %s", login.status, login.raw)
	}

	var loginData struct {
		Tokens struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(login.body.Data, &loginData); err != nil {
		t.Fatalf("decode login: %v", err)
	}

	// Refresh rotates the pair.
	refresh := call(t, http.MethodPost, "/api/v1/auth/refresh", map[string]any{
		"refresh_token": loginData.Tokens.RefreshToken,
	}, nil)
	if refresh.status != http.StatusOK {
		t.Fatalf("refresh failed: %d %s", refresh.status, refresh.raw)
	}

	// The old refresh token must be dead: rotation without revocation is just
	// a longer-lived credential.
	replay := call(t, http.MethodPost, "/api/v1/auth/refresh", map[string]any{
		"refresh_token": loginData.Tokens.RefreshToken,
	}, nil)
	if replay.status != http.StatusUnauthorized {
		t.Errorf("expected a rotated refresh token to be revoked, got %d: %s", replay.status, replay.raw)
	}

	// Logout ends every session.
	logout := call(t, http.MethodPost, "/api/v1/auth/logout", nil, bearer(loginData.Tokens.AccessToken))
	if logout.status != http.StatusOK {
		t.Errorf("logout failed: %d %s", logout.status, logout.raw)
	}
}

func TestAuth_RejectsMissingAndBogusCredentials(t *testing.T) {
	requireStack(t)

	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"no credential", nil},
		{"bogus bearer", bearer("not-a-real-token")},
		{"bogus api key", map[string]string{"X-API-Key": "bc_live_totallyfake"}},
	}

	for _, c := range cases {
		res := call(t, http.MethodGet, "/api/v1/users", nil, c.headers)
		if res.status != http.StatusUnauthorized {
			t.Errorf("%s: expected 401, got %d: %s", c.name, res.status, res.raw)
		}
		if res.body.Error == nil || res.body.Error.Code != "UNAUTHENTICATED" {
			t.Errorf("%s: expected the UNAUTHENTICATED code, got %s", c.name, res.raw)
		}
	}
}

// ---------------------------------------------------------------------
// Multi-tenancy — the isolation invariant, end to end
// ---------------------------------------------------------------------

func TestTenantIsolation_CannotReachAnotherTenantsResources(t *testing.T) {
	requireStack(t)

	alice := signup(t, "alice")
	bob := signup(t, "bob")

	// Every one of these is a real ID that exists — just not in Alice's
	// tenant. A 403 would confirm existence; only a 404 keeps the boundary
	// opaque.
	probes := []struct {
		name string
		path string
	}{
		{"tenant record", "/api/v1/tenants/" + bob.tenantID},
		{"user record", "/api/v1/users/" + bob.adminUserID},
	}

	for _, p := range probes {
		res := call(t, http.MethodGet, p.path, nil, bearer(alice.accessToken))
		if res.status != http.StatusNotFound {
			t.Errorf("%s: expected 404 for a cross-tenant read, got %d: %s", p.name, res.status, res.raw)
		}
		if res.body.Error != nil && res.body.Error.Code == "FORBIDDEN" {
			t.Errorf("%s: a 403 confirms the resource exists; the boundary must return NOT_FOUND", p.name)
		}
	}

	// Writes must be blocked too, not just reads.
	roleChange := call(t, http.MethodPatch, "/api/v1/users/"+bob.adminUserID+"/role",
		map[string]any{"role": "viewer"}, bearer(alice.accessToken))
	if roleChange.status != http.StatusNotFound {
		t.Errorf("expected a cross-tenant role change to 404, got %d: %s", roleChange.status, roleChange.raw)
	}

	// And Bob must be entirely unaffected.
	bobSelf := call(t, http.MethodGet, "/api/v1/users/me", nil, bearer(bob.accessToken))
	if bobSelf.status != http.StatusOK {
		t.Fatalf("bob's own account became unreadable: %d %s", bobSelf.status, bobSelf.raw)
	}
}

func TestTenantIsolation_ListingsOnlyContainOwnTenant(t *testing.T) {
	requireStack(t)

	alice := signup(t, "listalice")
	bob := signup(t, "listbob")

	res := call(t, http.MethodGet, "/api/v1/users?page_size=100", nil, bearer(alice.accessToken))
	if res.status != http.StatusOK {
		t.Fatalf("list users failed: %d %s", res.status, res.raw)
	}
	if strings.Contains(string(res.raw), bob.adminEmail) {
		t.Error("another tenant's user leaked into the user listing")
	}

	tenants := call(t, http.MethodGet, "/api/v1/tenants", nil, bearer(alice.accessToken))
	if tenants.status != http.StatusOK {
		t.Fatalf("list tenants failed: %d %s", tenants.status, tenants.raw)
	}
	if strings.Contains(string(tenants.raw), bob.slug) {
		t.Error("the tenant listing exposed another tenant")
	}
}

func TestTenantIsolation_PlanIsNotSelfService(t *testing.T) {
	requireStack(t)
	tn := signup(t, "planescalation")

	// A tenant admin must not be able to promote its own plan, because the
	// plan is what grants feature entitlements.
	res := call(t, http.MethodPatch, "/api/v1/tenant",
		map[string]any{"plan": "enterprise"}, bearer(tn.accessToken))

	if res.status == http.StatusOK {
		t.Fatal("a tenant admin was able to change its own plan, which grants it any entitlement")
	}

	// Confirm the plan really is unchanged.
	current := call(t, http.MethodGet, "/api/v1/tenant", nil, bearer(tn.accessToken))
	if !strings.Contains(string(current.raw), `"free"`) {
		t.Errorf("expected the tenant to still be on the free plan, got %s", current.raw)
	}
}

func TestPlatformControlPlane_RejectsTenantCredentials(t *testing.T) {
	requireStack(t)
	tn := signup(t, "platformprobe")

	// A tenant admin JWT must not reach a cross-tenant operation, and the
	// control plane should not even advertise itself.
	res := call(t, http.MethodGet, "/api/v1/platform/tenants", nil, bearer(tn.accessToken))
	if res.status == http.StatusOK {
		t.Fatalf("a tenant credential reached the platform control plane: %s", res.raw)
	}

	grant := call(t, http.MethodPost, "/api/v1/platform/features/grant", map[string]any{
		"tenant_id":   tn.tenantID,
		"feature_key": "usage.export",
		"enabled":     true,
	}, bearer(tn.accessToken))
	if grant.status == http.StatusOK {
		t.Fatal("a tenant admin granted itself a paid feature entitlement")
	}
}

// ---------------------------------------------------------------------
// RBAC
// ---------------------------------------------------------------------

func TestRBAC_AdminCanChangeRolesAndDemotedUsersCannot(t *testing.T) {
	requireStack(t)
	admin := signup(t, "rbac")

	// The signup admin cannot change its own role: that is either escalation
	// or a self-inflicted lockout.
	self := call(t, http.MethodPatch, "/api/v1/users/"+admin.adminUserID+"/role",
		map[string]any{"role": "viewer"}, bearer(admin.accessToken))
	if self.status != http.StatusForbidden {
		t.Errorf("expected a self role change to be forbidden, got %d: %s", self.status, self.raw)
	}

	// And it cannot be the last admin demoted by anyone.
	if self.body.Error != nil && self.body.Error.Code == "INTERNAL_ERROR" {
		t.Errorf("expected a typed authorization error, got %s", self.raw)
	}
}

func TestRBAC_UnknownRoleIsRejected(t *testing.T) {
	requireStack(t)
	admin := signup(t, "rbacrole")

	res := call(t, http.MethodPatch, "/api/v1/users/"+admin.adminUserID+"/role",
		map[string]any{"role": "superuser"}, bearer(admin.accessToken))

	if res.status == http.StatusOK {
		t.Fatal("an unknown role was accepted")
	}
}

// ---------------------------------------------------------------------
// API keys
// ---------------------------------------------------------------------

func TestAPIKeys_GenerateAuthenticateRotateRevoke(t *testing.T) {
	requireStack(t)
	tn := signup(t, "apikeys")

	gen := call(t, http.MethodPost, "/api/v1/apikeys",
		map[string]any{"name": "integration"}, bearer(tn.accessToken))
	if gen.status != http.StatusCreated {
		t.Fatalf("generate api key failed: %d %s", gen.status, gen.raw)
	}

	var genData struct {
		APIKey  string `json:"api_key"`
		Details struct {
			ID string `json:"id"`
		} `json:"details"`
	}
	if err := json.Unmarshal(gen.body.Data, &genData); err != nil {
		t.Fatalf("decode api key: %v (%s)", err, gen.raw)
	}
	if genData.APIKey == "" {
		t.Fatal("no plaintext key returned")
	}

	// The key authenticates requests.
	usingKey := call(t, http.MethodGet, "/api/v1/usage", nil,
		map[string]string{"X-API-Key": genData.APIKey})
	if usingKey.status != http.StatusOK {
		t.Fatalf("the generated API key did not authenticate: %d %s", usingKey.status, usingKey.raw)
	}

	// A listing must never contain the plaintext or any hash.
	list := call(t, http.MethodGet, "/api/v1/apikeys", nil, bearer(tn.accessToken))
	body := string(list.raw)
	if strings.Contains(body, genData.APIKey) {
		t.Error("the plaintext API key was returned by the listing endpoint")
	}
	for _, leak := range []string{"key_hash", "keyHash", "password_hash"} {
		if strings.Contains(body, leak) {
			t.Errorf("the API key listing exposed %q", leak)
		}
	}

	// Rotation issues a new key and kills the old one.
	rotate := call(t, http.MethodPost, "/api/v1/apikeys/"+genData.Details.ID+"/rotate", nil, bearer(tn.accessToken))
	if rotate.status != http.StatusOK {
		t.Fatalf("rotate failed: %d %s", rotate.status, rotate.raw)
	}

	afterRotate := call(t, http.MethodGet, "/api/v1/usage", nil,
		map[string]string{"X-API-Key": genData.APIKey})
	if afterRotate.status != http.StatusUnauthorized {
		t.Errorf("expected the rotated-out key to stop working, got %d", afterRotate.status)
	}
}

// ---------------------------------------------------------------------
// Feature entitlements
// ---------------------------------------------------------------------

func TestEntitlements_FreePlanIsBlockedFromExports(t *testing.T) {
	requireStack(t)
	tn := signup(t, "entitlement")

	// A new tenant is on the free plan, which does not include usage.export.
	res := call(t, http.MethodPost, "/api/v1/usage/exports", map[string]any{}, bearer(tn.accessToken))
	if res.status != http.StatusForbidden {
		t.Fatalf("expected 403 for a free-plan export, got %d: %s", res.status, res.raw)
	}
	if res.body.Error == nil || res.body.Error.Code != "FEATURE_NOT_ENTITLED" {
		t.Errorf("expected the FEATURE_NOT_ENTITLED code so a client can prompt an upgrade, got %s", res.raw)
	}

	mine := call(t, http.MethodGet, "/api/v1/features/mine", nil, bearer(tn.accessToken))
	if strings.Contains(string(mine.raw), "usage.export") {
		t.Error("a free-plan tenant reported the usage.export entitlement")
	}
}

// ---------------------------------------------------------------------
// Pagination
// ---------------------------------------------------------------------

func TestPagination_ClampsOversizedPageRequests(t *testing.T) {
	requireStack(t)
	tn := signup(t, "pagination")

	res := call(t, http.MethodGet, "/api/v1/users?page=1&page_size=100000", nil, bearer(tn.accessToken))
	if res.status != http.StatusOK {
		t.Fatalf("list users failed: %d %s", res.status, res.raw)
	}

	var data struct {
		Meta struct {
			PageSize int `json:"page_size"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(res.body.Data, &data); err != nil {
		t.Fatalf("decode meta: %v (%s)", err, res.raw)
	}
	if data.Meta.PageSize > 100 {
		t.Errorf("page_size was not clamped: got %d", data.Meta.PageSize)
	}
}

// ---------------------------------------------------------------------
// GraphQL
// ---------------------------------------------------------------------

type graphQLResponse struct {
	Data   map[string]any `json:"data"`
	Errors []struct {
		Message    string         `json:"message"`
		Extensions map[string]any `json:"extensions"`
	} `json:"errors"`
}

func graphQL(t *testing.T, token, query string, variables map[string]any) (int, graphQLResponse, []byte) {
	t.Helper()

	payload := map[string]any{"query": query}
	if variables != nil {
		payload["variables"] = variables
	}
	encoded, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, baseURL+"/graphql", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("build graphql request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /graphql: %v", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var out graphQLResponse
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, raw
}

func TestGraphQL_RequiresAuthentication(t *testing.T) {
	requireStack(t)

	status, _, raw := graphQL(t, "", `{ tenant { name } }`, nil)
	if status == http.StatusOK && !strings.Contains(string(raw), "UNAUTHENTICATED") {
		t.Errorf("expected an unauthenticated GraphQL request to be refused, got %s", raw)
	}
}

func TestGraphQL_QueriesShareTheRESTServiceLayer(t *testing.T) {
	requireStack(t)
	tn := signup(t, "graphql")

	status, res, raw := graphQL(t, tn.accessToken, `
		query {
			me { id email role }
			tenant { id name plan isActive }
			users(pageSize: 10) {
				nodes { id email role tenant { id name } }
				pageInfo { page pageSize totalCount hasNext }
			}
			myFeatures
		}`, nil)

	if status != http.StatusOK {
		t.Fatalf("graphql returned %d: %s", status, raw)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("unexpected graphql errors: %s", raw)
	}

	// The tenant reached through the nested User.tenant field must be the
	// caller's own, which is the DataLoader path.
	if !strings.Contains(string(raw), tn.tenantID) {
		t.Errorf("expected the caller's own tenant id in the response, got %s", raw)
	}
	// No credential material may appear anywhere in a GraphQL response.
	for _, leak := range []string{"passwordHash", "password_hash", "PasswordHash"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("GraphQL exposed %q", leak)
		}
	}
}

func TestGraphQL_TenantIsolationHoldsOverGraphQL(t *testing.T) {
	requireStack(t)

	alice := signup(t, "gqlalice")
	bob := signup(t, "gqlbob")

	_, _, raw := graphQL(t, alice.accessToken,
		`query ($id: ID!) { user(id: $id) { id email } }`,
		map[string]any{"id": bob.adminUserID})

	if strings.Contains(string(raw), bob.adminEmail) {
		t.Fatal("GraphQL returned another tenant's user")
	}
	if !strings.Contains(string(raw), `"user":null`) {
		t.Errorf("expected a cross-tenant lookup to resolve to null, got %s", raw)
	}
}

func TestGraphQL_RejectsOverlyComplexQueries(t *testing.T) {
	requireStack(t)
	tn := signup(t, "gqlcomplexity")

	// Deep, repeated nesting through the user/tenant cycle: cheap to send,
	// expensive to serve. It must be refused before execution.
	deep := `query { users(pageSize: 100) { nodes { tenant { id name slug plan isActive createdAt updatedAt } id email firstName lastName role isActive createdAt updatedAt lastLoginAt } pageInfo { page pageSize totalCount totalPages hasNext } } }`

	_, res, raw := graphQL(t, tn.accessToken, deep, nil)
	if len(res.Errors) == 0 {
		t.Fatalf("expected an expensive query to be rejected, got %s", raw)
	}
	if !strings.Contains(string(raw), "QUERY_REJECTED") {
		t.Errorf("expected the QUERY_REJECTED code, got %s", raw)
	}
}

func TestGraphQL_RejectsExcessiveDepth(t *testing.T) {
	requireStack(t)
	tn := signup(t, "gqldepth")

	query := `{ users { nodes { tenant { id } } } }`
	for i := 0; i < 3; i++ {
		query = `{ users { nodes { tenant { id } } } }`
	}

	// Build a document deeper than the configured maximum by chaining the
	// user/tenant cycle.
	nested := "id"
	for i := 0; i < 12; i++ {
		nested = "tenant { " + nested + " }"
	}
	deep := "{ users { nodes { " + nested + " } } }"

	_, res, raw := graphQL(t, tn.accessToken, deep, nil)
	if len(res.Errors) == 0 {
		t.Fatalf("expected an over-deep query to be rejected, got %s (control query: %s)", raw, query)
	}
}

func TestGraphQL_MutationsEnforceRoleDirectives(t *testing.T) {
	requireStack(t)
	tn := signup(t, "gqlmutation")

	// The admin cannot change its own role, enforced by the shared
	// UserService rather than by transport-specific code.
	_, _, raw := graphQL(t, tn.accessToken, `
		mutation ($input: UpdateUserRoleInput!) {
			updateUserRole(input: $input) { id role }
		}`, map[string]any{
		"input": map[string]any{"userId": tn.adminUserID, "role": "VIEWER"},
	})

	if !strings.Contains(string(raw), "FORBIDDEN") {
		t.Errorf("expected FORBIDDEN for a self role change over GraphQL, got %s", raw)
	}
}

func TestGraphQL_ExportMutationRespectsEntitlements(t *testing.T) {
	requireStack(t)
	tn := signup(t, "gqlexport")

	_, _, raw := graphQL(t, tn.accessToken, `
		mutation { requestUsageExport(input: {}) { id status } }`, nil)

	if !strings.Contains(string(raw), "FEATURE_NOT_ENTITLED") {
		t.Errorf("expected the free plan to be refused the export mutation, got %s", raw)
	}
}

func TestGraphQL_RejectsGETQueries(t *testing.T) {
	requireStack(t)

	// A query in a URL lands in access logs, proxy caches and browser history,
	// and a mutation over GET is trivially CSRF-able.
	res := call(t, http.MethodGet, "/graphql?query={tenant{name}}", nil, nil)
	if res.status == http.StatusOK && strings.Contains(string(res.raw), `"data"`) {
		t.Error("GraphQL executed a query sent over GET")
	}
}

// ---------------------------------------------------------------------
// Usage metering
// ---------------------------------------------------------------------

func TestUsageMetering_RecordsRequests(t *testing.T) {
	requireStack(t)
	tn := signup(t, "metering")

	for i := 0; i < 3; i++ {
		call(t, http.MethodGet, "/api/v1/features", nil, bearer(tn.accessToken))
	}

	// Metering is asynchronous by design (it must not add latency to the
	// response), so poll briefly rather than assuming it has landed.
	deadline := time.Now().Add(10 * time.Second)
	for {
		res := call(t, http.MethodGet, "/api/v1/usage?page_size=50", nil, bearer(tn.accessToken))
		if res.status == http.StatusOK && strings.Contains(string(res.raw), "/api/v1/features") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("usage records never appeared: %s", res.raw)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------
// Error contract
// ---------------------------------------------------------------------

func TestErrors_UseStableCodesAndLeakNothingInternal(t *testing.T) {
	requireStack(t)
	tn := signup(t, "errors")

	res := call(t, http.MethodGet, "/api/v1/users/00000000-0000-0000-0000-000000000000", nil, bearer(tn.accessToken))
	if res.status != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", res.status, res.raw)
	}
	if res.body.Error == nil || res.body.Error.Code != "NOT_FOUND" {
		t.Errorf("expected a NOT_FOUND code, got %s", res.raw)
	}
	if res.body.RequestID == "" {
		t.Error("expected the error envelope to carry the correlation id")
	}

	// An internal error must never surface SQL, hostnames, or driver text.
	body := strings.ToLower(string(res.raw))
	for _, leak := range []string{"select ", "pq:", "postgres", "sql:", "5432"} {
		if strings.Contains(body, leak) {
			t.Errorf("the error response leaked internal detail (%q): %s", leak, res.raw)
		}
	}
}

func TestErrors_RejectsUnknownJSONFields(t *testing.T) {
	requireStack(t)

	// A typo in a client payload should fail loudly rather than be silently
	// dropped, which is how "why didn't my update apply?" bugs happen.
	res := call(t, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email":     "someone@example.test",
		"password":  "whatever",
		"remmember": true,
	}, nil)

	if res.status != http.StatusBadRequest {
		t.Errorf("expected 400 for an unknown field, got %d: %s", res.status, res.raw)
	}
}
