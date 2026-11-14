// Package compile turns a project into a single SQL script: init.sql, then
// every selected model and test inside one transaction. `molt run` executes
// exactly this script, and `molt compile` prints it, so it can also be run with
// the DuckDB CLI.
package compile

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/vt100io/molt/internal/duckdb"
	"github.com/vt100io/molt/internal/project"
)

// Statement kinds, so a runner can report progress per model and test.
const (
	StmtInit  = "init"
	StmtMeta  = "meta"
	StmtModel = "model"
	StmtTest  = "test"
)

type Statement struct {
	Kind  string
	Label string // model or test name, or a description
	SQL   string
}

type Script struct {
	Statements []Statement
}

// String renders the script as SQL that the DuckDB CLI can run.
func (s *Script) String() string {
	var b strings.Builder
	for _, stmt := range s.Statements {
		fmt.Fprintf(&b, "-- %s: %s\n%s;\n\n", stmt.Kind, stmt.Label, stmt.SQL)
	}
	return b.String()
}

type Options struct {
	// Models to build, in topological order.
	Models []string
	// Vars are set after init.sql, so they override variables it sets.
	Vars map[string]string
	// Existing maps schema.table to "table" or "view" for objects already in the
	// database, so a model that changes kind replaces the old object.
	Existing map[string]string
	// FullRefresh rebuilds models from scratch. table and view models always
	// are; the option exists for incremental models.
	FullRefresh bool
}

// Bookkeeping lives in the _molt schema of the same database and is written in
// the same transaction as the models, so it can never disagree with the data.
const bookkeepingDDL = `CREATE SCHEMA IF NOT EXISTS _molt;
CREATE TABLE IF NOT EXISTS _molt.models (
    name      VARCHAR PRIMARY KEY,
    kind      VARCHAR NOT NULL,
    sql_hash  VARCHAR NOT NULL,
    built_at  TIMESTAMPTZ NOT NULL,
    row_count BIGINT
);
CREATE TABLE IF NOT EXISTS _molt.runs (
    run_at       TIMESTAMPTZ NOT NULL,
    models       VARCHAR[] NOT NULL,
    tests        INTEGER NOT NULL,
    full_refresh BOOLEAN NOT NULL
)`

// Compile builds the script for the selected models. Tests run after the
// models: all tests when every model is selected, otherwise the tests that
// reference a selected model.
func Compile(p *project.Project, opts Options) (*Script, error) {
	s := &Script{}
	add := func(kind, label, sql string) {
		s.Statements = append(s.Statements, Statement{Kind: kind, Label: label, SQL: sql})
	}

	if strings.TrimSpace(p.InitSQL) != "" {
		add(StmtInit, "init.sql", strings.TrimSuffix(strings.TrimSpace(p.InitSQL), ";"))
	}
	for _, stmt := range duckdb.VarStatements(opts.Vars) {
		add(StmtInit, "--var", stmt)
	}

	add(StmtMeta, "begin", "BEGIN TRANSACTION")
	add(StmtMeta, "bookkeeping", bookkeepingDDL)

	schemas := make(map[string]bool)
	for _, name := range opts.Models {
		m, ok := p.Models[name]
		if !ok {
			return nil, fmt.Errorf("model %s not found", name)
		}
		if !schemas[m.Schema] {
			schemas[m.Schema] = true
			add(StmtMeta, "schema "+m.Schema, "CREATE SCHEMA IF NOT EXISTS "+duckdb.QuoteIdent(m.Schema))
		}
		add(StmtModel, m.Name, modelSQL(m, opts.Existing[m.Name]))
	}

	tests := selectTests(p, opts.Models)
	for _, t := range tests {
		add(StmtTest, t.Name, testSQL(t))
	}

	models := make([]string, len(opts.Models))
	for i, name := range opts.Models {
		models[i] = duckdb.QuoteString(name)
	}
	add(StmtMeta, "record run", fmt.Sprintf(
		"INSERT INTO _molt.runs VALUES (current_timestamp, [%s]::VARCHAR[], %d, %t)",
		strings.Join(models, ", "), len(tests), opts.FullRefresh))
	add(StmtMeta, "commit", "COMMIT")

	return s, nil
}

// modelSQL wraps a model's SELECT in the statement for its kind, followed by
// its bookkeeping row. existing is the kind of the object already in the
// database under the model's name, if any.
func modelSQL(m *project.Model, existing string) string {
	target := duckdb.QuoteIdent(m.Schema) + "." + duckdb.QuoteIdent(m.Table)

	var b strings.Builder
	// CREATE OR REPLACE cannot replace a view with a table or the reverse.
	if existing != "" && existing != m.Kind {
		fmt.Fprintf(&b, "DROP %s %s;\n", strings.ToUpper(existing), target)
	}

	rowCount := "NULL"
	switch m.Kind {
	case project.KindTable:
		// The SELECT starts on the same line, so DuckDB error line numbers match the file.
		fmt.Fprintf(&b, "CREATE OR REPLACE TABLE %s AS %s\n;\n", target, m.SQL)
		rowCount = "(SELECT count(*) FROM " + target + ")"
	case project.KindView:
		fmt.Fprintf(&b, "CREATE OR REPLACE VIEW %s AS %s\n;\n", target, m.SQL)
	}

	fmt.Fprintf(&b, "INSERT OR REPLACE INTO _molt.models VALUES (%s, %s, %s, current_timestamp, %s)",
		duckdb.QuoteString(m.Name), duckdb.QuoteString(m.Kind), duckdb.QuoteString(m.Hash), rowCount)
	return b.String()
}

// testSQL raises an error, which aborts the transaction, when the test query returns rows.
func testSQL(t *project.Test) string {
	return fmt.Sprintf("SELECT error(%s || n || ' rows') FROM (SELECT count(*) AS n FROM (%s\n)) WHERE n > 0",
		duckdb.QuoteString("test "+t.Name+" failed: "), t.SQL)
}

func selectTests(p *project.Project, models []string) []*project.Test {
	if len(models) == len(p.Models) {
		return p.Tests
	}
	selected := make(map[string]bool, len(models))
	for _, name := range models {
		selected[name] = true
	}
	var tests []*project.Test
	for _, t := range p.Tests {
		for _, dep := range t.Dependencies {
			if selected[dep] {
				tests = append(tests, t)
				break
			}
		}
	}
	return tests
}

// ExistingObjects returns the tables and views in the current database, keyed
// by schema.table, with value "table" or "view".
func ExistingObjects(db *sql.DB) (map[string]string, error) {
	rows, err := db.Query(`
		SELECT schema_name || '.' || table_name, 'table' FROM duckdb_tables()
		WHERE database_name = current_database()
		UNION ALL
		SELECT schema_name || '.' || view_name, 'view' FROM duckdb_views()
		WHERE database_name = current_database() AND NOT internal`)
	if err != nil {
		return nil, fmt.Errorf("failed to list existing objects: %w", err)
	}
	defer rows.Close()

	existing := make(map[string]string)
	for rows.Next() {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			return nil, err
		}
		existing[name] = kind
	}
	return existing, rows.Err()
}
