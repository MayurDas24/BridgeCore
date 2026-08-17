package graph

import (
	"testing"

	"github.com/bridgecore/bridgecore/pkg/apierr"
)

func TestAnalyze_MeasuresDepth(t *testing.T) {
	doc := `
	query {
		users(pageSize: 10) {
			nodes {
				tenant {
					name
				}
			}
		}
	}`

	got := Analyze(doc, 100)
	// query{ -> 1, users{ -> 2, nodes{ -> 3, tenant{ -> 4
	if got.Depth != 4 {
		t.Errorf("Depth = %d, want 4", got.Depth)
	}
}

func TestAnalyze_MultipliesCostByPageSize(t *testing.T) {
	small := Analyze(`query { users(pageSize: 2) { nodes { id email } } }`, 100)
	large := Analyze(`query { users(pageSize: 50) { nodes { id email } } }`, 100)

	if large.Cost <= small.Cost {
		t.Fatalf("a larger page must cost more: pageSize 2 = %d, pageSize 50 = %d", small.Cost, large.Cost)
	}
}

func TestAnalyze_TreatsVariablePageSizeAsWorstCase(t *testing.T) {
	// The value is unknown until execution, so the analyzer must assume the
	// maximum rather than the best case.
	withVar := Analyze(`query ($n: Int) { users(pageSize: $n) { nodes { id } } }`, 100)
	withMax := Analyze(`query { users(pageSize: 100) { nodes { id } } }`, 100)

	if withVar.Cost < withMax.Cost {
		t.Errorf("a variable page size must be costed at the maximum: got %d, want at least %d",
			withVar.Cost, withMax.Cost)
	}
}

func TestAnalyze_ClampsPageSizeToMaximum(t *testing.T) {
	// Asking for 10,000 cannot cost more than the ceiling allows, because
	// the resolver will clamp it too.
	huge := Analyze(`query { users(pageSize: 10000) { nodes { id } } }`, 100)
	atMax := Analyze(`query { users(pageSize: 100) { nodes { id } } }`, 100)

	if huge.Cost != atMax.Cost {
		t.Errorf("page size above the ceiling should cost the same as the ceiling: %d vs %d",
			huge.Cost, atMax.Cost)
	}
}

func TestAnalyze_IgnoresCommentsAndStrings(t *testing.T) {
	doc := `
	query {
		# users { nodes { id } } this is a comment, not a selection
		tenant { name }
		auditLogs(event: "user { login }") { pageInfo { page } }
	}`

	got := Analyze(doc, 100)
	if got.Depth > 3 {
		t.Errorf("braces inside comments and strings must not add depth, got %d", got.Depth)
	}
}

func TestAnalyze_DetectsIntrospection(t *testing.T) {
	if !Analyze(`{ __schema { types { name } } }`, 100).UsesIntrospection {
		t.Error("expected __schema to be detected as introspection")
	}
	if !Analyze(`{ __type(name: "User") { name } }`, 100).UsesIntrospection {
		t.Error("expected __type to be detected as introspection")
	}
	if Analyze(`{ tenant { name } }`, 100).UsesIntrospection {
		t.Error("a normal query must not be flagged as introspection")
	}
}

func TestAnalyze_DoesNotCountAliasesOrArgumentNamesAsFields(t *testing.T) {
	plain := Analyze(`{ users { nodes { id } } }`, 100)
	aliased := Analyze(`{ everyone: users { nodes { id } } }`, 100)

	if aliased.Fields != plain.Fields {
		t.Errorf("an alias must not add a field: %d vs %d", aliased.Fields, plain.Fields)
	}
}

func TestCheck_RejectsExcessiveDepth(t *testing.T) {
	doc := `{ a { b { c { d { e { f } } } } } }`

	_, err := Check(doc, Limits{MaxDepth: 3, MaxComplexity: 10000, MaxPageSize: 100, AllowIntrospection: true})
	if !apierr.Is(err, apierr.CodeQueryRejected) {
		t.Fatalf("expected QUERY_REJECTED for an over-deep document, got %v", err)
	}
}

func TestCheck_RejectsExcessiveComplexity(t *testing.T) {
	doc := `{ users(pageSize: 100) { nodes { id email firstName lastName role isActive createdAt } } }`

	_, err := Check(doc, Limits{MaxDepth: 20, MaxComplexity: 50, MaxPageSize: 100, AllowIntrospection: true})
	if !apierr.Is(err, apierr.CodeQueryRejected) {
		t.Fatalf("expected QUERY_REJECTED for an over-costly document, got %v", err)
	}
}

func TestCheck_RejectsOversizedDocument(t *testing.T) {
	doc := "{ tenant { name } }"

	_, err := Check(doc, Limits{MaxBytes: 5, MaxDepth: 10, MaxComplexity: 100, MaxPageSize: 100})
	if !apierr.Is(err, apierr.CodeQueryRejected) {
		t.Fatalf("expected an oversized document to be rejected before parsing, got %v", err)
	}
}

func TestCheck_RejectsIntrospectionWhenDisabled(t *testing.T) {
	_, err := Check(`{ __schema { types { name } } }`, Limits{
		MaxDepth: 10, MaxComplexity: 1000, MaxPageSize: 100, AllowIntrospection: false,
	})
	if !apierr.Is(err, apierr.CodeQueryRejected) {
		t.Fatalf("expected introspection to be refused when disabled, got %v", err)
	}
}

func TestCheck_AcceptsAReasonableDocument(t *testing.T) {
	doc := `
	query Dashboard {
		me { id email role }
		tenant { name plan }
		usageSummary { endpoint requestCount }
	}`

	analysis, err := Check(doc, Limits{
		MaxDepth: 10, MaxComplexity: 500, MaxBytes: 16384, MaxPageSize: 100, AllowIntrospection: true,
	})
	if err != nil {
		t.Fatalf("expected a normal dashboard query to be accepted, got %v", err)
	}
	if analysis.Fields == 0 {
		t.Error("expected the analyzer to count the selected fields")
	}
}
