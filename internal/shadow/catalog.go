// Package shadow builds a shadow catalog: an in-memory DuckDB where every model
// exists with its real output schema but no data.
//
// After running init.sql, models are created in dependency order: table models
// with CREATE TABLE ... AS ... LIMIT 0, view models as views. Each one is bound
// against the real columns of its upstream models without processing any data.
// This validates column references before a run and gives the planner real
// schemas to work with (explain, and later lineage).
package shadow

import (
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/vt100io/molt/internal/build"
	"github.com/vt100io/molt/internal/duckdb"
	"github.com/vt100io/molt/internal/project"
)

type Column struct {
	Name string
	Type string
}

type Catalog struct {
	db      *sql.DB
	project *project.Project

	// Order lists all models in dependency order.
	Order []string
	// Schemas holds the output columns of every model that bound successfully.
	Schemas map[string][]Column
	// Errors holds the bind error of every model that failed, including models
	// skipped because an upstream model failed.
	Errors map[string]error
}

// Build creates the shadow catalog for a project. A model that fails to bind
// is recorded in Errors rather than aborting, so all problems are reported at
// once. An error in init.sql aborts, since nothing can bind without it.
func Build(p *project.Project, vars map[string]string) (*Catalog, error) {
	order, err := build.TopoSort(p)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("failed to open duckdb: %w", err)
	}
	// One connection: the catalog lives in a single in-memory database.
	db.SetMaxOpenConns(1)

	setup := duckdb.VarStatements(vars)
	if strings.TrimSpace(p.InitSQL) != "" {
		setup = append([]string{p.InitSQL}, setup...)
	}
	for _, schema := range p.Schemas() {
		setup = append(setup, "CREATE SCHEMA IF NOT EXISTS "+duckdb.QuoteIdent(schema))
	}
	for _, stmt := range setup {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("init failed: %w", err)
		}
	}

	c := &Catalog{
		db:      db,
		project: p,
		Order:   order,
		Schemas: make(map[string][]Column),
		Errors:  make(map[string]error),
	}

	for _, name := range order {
		m := p.Models[name]
		if failed := c.failedUpstream(m); failed != "" {
			c.Errors[name] = fmt.Errorf("upstream model %s failed", failed)
			continue
		}

		// The model SQL starts on line 1 so binder errors report the line numbers of the
		// model file. The closing parenthesis goes on its own line in case of a trailing comment.
		target := duckdb.QuoteIdent(m.Schema) + "." + duckdb.QuoteIdent(m.Table)
		createSQL := fmt.Sprintf("CREATE TABLE %s AS SELECT * FROM (%s\n) LIMIT 0", target, m.SQL)
		if m.Kind == project.KindView {
			createSQL = fmt.Sprintf("CREATE VIEW %s AS %s\n", target, m.SQL)
		}
		if _, err := db.Exec(createSQL); err != nil {
			c.Errors[name] = err
			continue
		}

		cols, err := c.describe(m)
		if err != nil {
			db.Close()
			return nil, err
		}
		c.Schemas[name] = cols
	}

	return c, nil
}

func (c *Catalog) Close() error {
	return c.db.Close()
}

// Explain returns the logical plan for a model, bound against the real schemas
// of its upstream models.
func (c *Catalog) Explain(modelName string) (string, error) {
	m, ok := c.project.Models[modelName]
	if !ok {
		return "", fmt.Errorf("model %s not found", modelName)
	}
	if failed := c.failedUpstream(m); failed != "" {
		return "", fmt.Errorf("cannot explain %s: upstream model %s failed: %w", modelName, failed, c.Errors[failed])
	}

	if _, err := c.db.Exec("PRAGMA disable_optimizer"); err != nil {
		return "", fmt.Errorf("failed to disable optimizer: %w", err)
	}
	defer c.db.Exec("PRAGMA enable_optimizer")

	var key, plan string
	if err := c.db.QueryRow("EXPLAIN "+m.SQL).Scan(&key, &plan); err != nil {
		return "", fmt.Errorf("failed to explain SQL: %w", err)
	}
	return plan, nil
}

// failedUpstream returns the first dependency of m that failed to bind, or "".
func (c *Catalog) failedUpstream(m *project.Model) string {
	for _, dep := range m.Dependencies {
		if _, failed := c.Errors[dep]; failed {
			return dep
		}
	}
	return ""
}

func (c *Catalog) describe(m *project.Model) ([]Column, error) {
	rows, err := c.db.Query(`
		SELECT column_name, data_type
		FROM information_schema.columns
		WHERE table_schema = ? AND table_name = ?
		ORDER BY ordinal_position`, m.Schema, m.Table)
	if err != nil {
		return nil, fmt.Errorf("failed to describe %s: %w", m.Name, err)
	}
	defer rows.Close()

	var cols []Column
	for rows.Next() {
		var col Column
		if err := rows.Scan(&col.Name, &col.Type); err != nil {
			return nil, fmt.Errorf("failed to describe %s: %w", m.Name, err)
		}
		cols = append(cols, col)
	}
	return cols, rows.Err()
}
