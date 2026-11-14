package duckdb

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	_ "github.com/duckdb/duckdb-go/v2"
)

// VarStatements returns SET VARIABLE statements for vars, sorted by name, so
// getvariable() resolves in model SQL. Values are set as strings.
func VarStatements(vars map[string]string) []string {
	names := make([]string, 0, len(vars))
	for k := range vars {
		names = append(names, k)
	}
	sort.Strings(names)

	stmts := make([]string, 0, len(names))
	for _, k := range names {
		stmts = append(stmts, fmt.Sprintf("SET VARIABLE %s = %s", QuoteIdent(k), QuoteString(vars[k])))
	}
	return stmts
}

// QuoteIdent quotes a SQL identifier.
func QuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// QuoteString quotes a SQL string literal.
func QuoteString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// AnalyzeDependencies returns the models referenced by sqlText, sorted by name.
// Model names are schema-qualified (main.offers); an unqualified table
// reference resolves to the main schema, as in DuckDB.
//
// It uses DuckDB's own parser (json_serialize_sql) rather than the planner, so
// no schemas are needed: column references are never bound, and models with
// arbitrary columns resolve without dummy tables or annotations. Tables that
// are not models (attached sources, files) are ignored.
func AnalyzeDependencies(sqlText string, modelNames []string) ([]string, error) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	var astJSON string
	// Cast to VARCHAR: the driver otherwise decodes the JSON type itself.
	if err := db.QueryRow("SELECT json_serialize_sql(?)::VARCHAR", sqlText).Scan(&astJSON); err != nil {
		return nil, fmt.Errorf("failed to parse SQL: %w", err)
	}

	var ast map[string]any
	if err := json.Unmarshal([]byte(astJSON), &ast); err != nil {
		return nil, fmt.Errorf("failed to unmarshal AST: %w", err)
	}
	if isErr, _ := ast["error"].(bool); isErr {
		return nil, fmt.Errorf("failed to parse SQL: %v", ast["error_message"])
	}

	refs := make(map[string]struct{})
	collectTableRefs(ast, map[string]bool{}, refs)

	// Map back to the canonical model name casing.
	modelMap := make(map[string]string, len(modelNames))
	for _, name := range modelNames {
		modelMap[strings.ToLower(name)] = name
	}

	var result []string
	for ref := range refs {
		if name, ok := modelMap[ref]; ok {
			result = append(result, name)
		}
	}
	// Sorted so the build order, and so the compiled script, is deterministic.
	sort.Strings(result)
	return result, nil
}

// collectTableRefs walks the serialized AST and records the lowercased
// schema.table of every BASE_TABLE reference that is not shadowed by a CTE in scope.
func collectTableRefs(node any, ctes map[string]bool, refs map[string]struct{}) {
	switch n := node.(type) {
	case []any:
		for _, child := range n {
			collectTableRefs(child, ctes, refs)
		}
	case map[string]any:
		// A recursive CTE may reference itself.
		if n["type"] == "RECURSIVE_CTE_NODE" {
			if name, ok := n["cte_name"].(string); ok {
				ctes = withCTE(ctes, name)
			}
		}

		if cteMap, ok := n["cte_map"].(map[string]any); ok {
			entries, _ := cteMap["map"].([]any)
			// Each CTE body sees the CTEs defined before it, but not itself:
			// in `WITH offers AS (SELECT * FROM offers)` the inner reference is the table.
			for _, e := range entries {
				entry, _ := e.(map[string]any)
				collectTableRefs(entry["value"], ctes, refs)
				if name, ok := entry["key"].(string); ok {
					ctes = withCTE(ctes, name)
				}
			}
		}

		if n["type"] == "BASE_TABLE" {
			name, _ := n["table_name"].(string)
			schema, _ := n["schema_name"].(string)
			// Only an unqualified name can refer to a CTE.
			if name != "" && !(schema == "" && ctes[strings.ToLower(name)]) {
				if schema == "" {
					schema = "main"
				}
				refs[strings.ToLower(schema+"."+name)] = struct{}{}
			}
		}

		for key, child := range n {
			if key == "cte_map" {
				continue // already walked with the correct scope
			}
			collectTableRefs(child, ctes, refs)
		}
	}
}

// withCTE returns a copy of ctes with name added, so sibling scopes are unaffected.
func withCTE(ctes map[string]bool, name string) map[string]bool {
	next := make(map[string]bool, len(ctes)+1)
	for k := range ctes {
		next[k] = true
	}
	next[strings.ToLower(name)] = true
	return next
}
