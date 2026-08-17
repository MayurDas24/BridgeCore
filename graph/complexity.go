package graph

import (
	"errors"
	"strconv"
	"strings"

	"github.com/bridgecore/bridgecore/pkg/apierr"
)

// Analysis is the static cost profile of a GraphQL document.
type Analysis struct {
	// Depth is the deepest selection-set nesting.
	Depth int
	// Cost approximates how many resolver invocations the document can
	// trigger, accounting for pagination multipliers.
	Cost int
	// Fields is the raw field count, useful for logging.
	Fields int
	// UsesIntrospection reports whether the document queries __schema or
	// __type.
	UsesIntrospection bool
}

// Limits are the ceilings a document must satisfy.
type Limits struct {
	MaxDepth      int
	MaxComplexity int
	MaxBytes      int
	MaxPageSize   int
	// AllowIntrospection is false in production, where the schema should not
	// publish itself to unauthenticated scanners.
	AllowIntrospection bool
}

// Analyze computes the cost profile of a GraphQL document.
//
// Why this exists at all: REST endpoints have a fixed cost per route, so
// pagination alone bounds them. A single GraphQL document can nest
// selections and ask for a page of users, each with their tenant, each with
// its users again — a shape that is trivially small to send and enormous to
// serve. Rejecting expensive documents *before* execution is the only place
// the check is cheap.
//
// The analysis is done on the raw document with a small scanner rather than
// after parsing, so a hostile document is rejected before anything larger
// than the request body has been allocated. Cost is computed as the sum, over
// every field, of the product of the pagination multipliers of its enclosing
// selection sets: a page of 100 users costs 100, and each field selected
// inside those users costs another 100.
func Analyze(document string, maxPageSize int) Analysis {
	if maxPageSize <= 0 {
		maxPageSize = 100
	}

	var (
		analysis Analysis
		depth    int
		// multipliers[i] is the cost multiplier in force at nesting level i.
		multipliers = []int{1}
		// pendingMultiplier is the multiplier implied by the arguments of
		// the field whose selection set is about to open.
		pendingMultiplier = 1
	)

	runes := []rune(document)
	i := 0
	for i < len(runes) {
		c := runes[i]

		switch {
		case c == '#':
			// Comment to end of line.
			for i < len(runes) && runes[i] != '\n' {
				i++
			}

		case c == '"':
			i = skipString(runes, i)

		case c == '(':
			// Argument list: extract the pagination hint, then skip it.
			end := matchParen(runes, i)
			pendingMultiplier = pageMultiplier(string(runes[i+1:end]), maxPageSize)
			i = end + 1

		case c == '{':
			depth++
			if depth > analysis.Depth {
				analysis.Depth = depth
			}
			parent := multipliers[len(multipliers)-1]
			multipliers = append(multipliers, parent*pendingMultiplier)
			pendingMultiplier = 1
			i++

		case c == '}':
			depth--
			if len(multipliers) > 1 {
				multipliers = multipliers[:len(multipliers)-1]
			}
			pendingMultiplier = 1
			i++

		case isNameStart(c):
			start := i
			for i < len(runes) && isNameChar(runes[i]) {
				i++
			}
			name := string(runes[start:i])

			// Skip keywords and alias/argument-name positions: a name
			// followed by ':' is an alias or an argument key, not a field.
			rest := skipSpace(runes, i)
			if rest < len(runes) && runes[rest] == ':' {
				i = rest + 1
				continue
			}
			if isReservedWord(name) {
				continue
			}

			if strings.HasPrefix(name, "__") {
				analysis.UsesIntrospection = true
			}

			// Only selections inside an operation body have a cost.
			if depth >= 1 {
				analysis.Fields++
				analysis.Cost += multipliers[len(multipliers)-1]
			}

		default:
			i++
		}
	}

	return analysis
}

// Check validates a document against the configured limits, returning a
// caller-safe error that names the limit and the measured value so a client
// can actually fix its query.
func Check(document string, limits Limits) (Analysis, error) {
	if limits.MaxBytes > 0 && len(document) > limits.MaxBytes {
		return Analysis{}, apierr.QueryRejected("the GraphQL document is too large").
			WithDetails(map[string]any{"bytes": len(document), "max_bytes": limits.MaxBytes})
	}

	analysis := Analyze(document, limits.MaxPageSize)

	if analysis.UsesIntrospection && !limits.AllowIntrospection {
		return analysis, apierr.QueryRejected("schema introspection is disabled in this environment")
	}
	if limits.MaxDepth > 0 && analysis.Depth > limits.MaxDepth {
		return analysis, apierr.QueryRejected("the GraphQL document is nested too deeply").
			WithDetails(map[string]any{"depth": analysis.Depth, "max_depth": limits.MaxDepth})
	}
	if limits.MaxComplexity > 0 && analysis.Cost > limits.MaxComplexity {
		return analysis, apierr.QueryRejected("the GraphQL document is too expensive to execute").
			WithDetails(map[string]any{
				"complexity":     analysis.Cost,
				"max_complexity": limits.MaxComplexity,
				"hint":           "request a smaller pageSize or select fewer nested fields",
			})
	}
	return analysis, nil
}

// pageMultiplier reads a pagination argument out of an argument list.
//
// A literal value is used as given. A variable ($pageSize) is treated as the
// maximum page size, because the actual value is not known until execution
// and a limiter that assumes the best case is not a limiter.
func pageMultiplier(args string, maxPageSize int) int {
	for _, key := range []string{"pageSize", "page_size", "first", "last", "limit"} {
		idx := strings.Index(args, key)
		if idx < 0 {
			continue
		}
		rest := strings.TrimSpace(args[idx+len(key):])
		if !strings.HasPrefix(rest, ":") {
			continue
		}
		rest = strings.TrimSpace(rest[1:])

		if strings.HasPrefix(rest, "$") {
			return maxPageSize
		}

		end := 0
		for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
			end++
		}
		if end == 0 {
			continue
		}
		n, err := strconv.Atoi(rest[:end])
		if err != nil || n <= 0 {
			continue
		}
		if n > maxPageSize {
			n = maxPageSize
		}
		return n
	}
	return 1
}

func skipString(runes []rune, i int) int {
	// Block string.
	if i+2 < len(runes) && runes[i+1] == '"' && runes[i+2] == '"' {
		j := i + 3
		for j+2 < len(runes) {
			if runes[j] == '"' && runes[j+1] == '"' && runes[j+2] == '"' {
				return j + 3
			}
			j++
		}
		return len(runes)
	}
	// Regular string, honouring escapes.
	j := i + 1
	for j < len(runes) {
		if runes[j] == '\\' {
			j += 2
			continue
		}
		if runes[j] == '"' {
			return j + 1
		}
		j++
	}
	return len(runes)
}

// matchParen returns the index of the ')' closing the '(' at i, tolerating
// nesting and quoted strings inside the argument list.
func matchParen(runes []rune, i int) int {
	depth := 0
	for j := i; j < len(runes); j++ {
		switch runes[j] {
		case '"':
			j = skipString(runes, j) - 1
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return len(runes) - 1
}

func skipSpace(runes []rune, i int) int {
	for i < len(runes) {
		switch runes[i] {
		case ' ', '\t', '\n', '\r', ',':
			i++
		default:
			return i
		}
	}
	return i
}

func isNameStart(c rune) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isNameChar(c rune) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}

func isReservedWord(name string) bool {
	switch name {
	case "query", "mutation", "subscription", "fragment", "on", "true", "false", "null":
		return true
	}
	return false
}

// errorsIs is a local alias so the resolver error mapping reads cleanly.
func errorsIs(err, target error) bool { return errors.Is(err, target) }
