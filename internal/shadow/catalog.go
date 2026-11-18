// Package shadow builds a shadow catalog: an in-memory DuckDB where every model
// exists with its real output schema but no data.
//
// After running init.sql, models are created in dependency order with
// CREATE TABLE ... AS SELECT * FROM (...) LIMIT 0. Each one is bound against the
// real columns of its upstream models without processing any data. This
// validates column references before a run and gives the planner real schemas
// to work with (explain, lineage).
//
// View models are created as empty tables too. DuckDB's binder expands a view
// into the plan of whatever reads it, so a downstream model's plan would skip
// over the view and its lineage would jump straight to the view's own sources.
// As a table, every model is a boundary and lineage goes one model at a time.
package shadow

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/vt100io/molt/internal/build"
	"github.com/vt100io/molt/internal/duckdb"
	"github.com/vt100io/molt/internal/lineage"
	"github.com/vt100io/molt/internal/project"
)

// Column is an output column of a model.
type Column = project.Column

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

	version    string // DuckDB version, read on first use by Lineage
	versionErr error
	lineage    map[string]*lineage.Model
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

// Lineage returns the column lineage of a model: for each output column, the
// columns of upstream models it comes from, and its fingerprint. See package
// lineage. Upstream models are traced first, since fingerprints build on theirs.
func (c *Catalog) Lineage(modelName string) (*lineage.Model, error) {
	if l, ok := c.lineage[modelName]; ok {
		return l, nil
	}
	m, ok := c.project.Models[modelName]
	if !ok {
		return nil, fmt.Errorf("model %s not found", modelName)
	}
	if err, failed := c.Errors[modelName]; failed {
		return nil, fmt.Errorf("cannot trace %s: %w", modelName, err)
	}

	if c.versionErr == nil && c.version == "" {
		if err := c.db.QueryRow("SELECT version()").Scan(&c.version); err != nil {
			return nil, fmt.Errorf("failed to read DuckDB version: %w", err)
		}
		c.versionErr = lineage.CheckVersion(c.version)
	}
	if c.versionErr != nil {
		return nil, c.versionErr
	}

	upstream := map[string]*lineage.Model{}
	for _, dep := range m.Dependencies {
		l, err := c.Lineage(dep)
		if err != nil {
			return nil, err
		}
		upstream[dep] = l
	}

	// Unoptimized, so the plan follows the SQL as written. Cast to VARCHAR: the
	// driver otherwise decodes the JSON type itself.
	var plan string
	if err := c.db.QueryRow("SELECT json_serialize_plan(?, optimize := false)::VARCHAR", m.SQL).Scan(&plan); err != nil {
		return nil, fmt.Errorf("failed to plan %s: %w", modelName, err)
	}

	names := make([]string, len(c.Schemas[modelName]))
	for i, col := range c.Schemas[modelName] {
		names[i] = col.Name
	}
	l, err := lineage.Analyze([]byte(plan), names, fingerprintsOf(upstream))
	var planErr *lineage.PlanError
	switch {
	case errors.As(err, &planErr):
		// DuckDB cannot serialize every plan; table functions such as
		// read_csv have no serializer. Fall back to the model's dependencies.
		l = c.lineageFromDependencies(m, names, planErr.Message)
		opaqueFingerprints(l, m, upstream)
	case err != nil:
		return nil, fmt.Errorf("lineage of %s: %w", modelName, err)
	case l.Approximate != "":
		opaqueFingerprints(l, m, upstream)
	}

	if c.lineage == nil {
		c.lineage = map[string]*lineage.Model{}
	}
	c.lineage[modelName] = l
	return l, nil
}

// Pin sets the lineage of a model instead of tracing it. molt run pins the
// models it does not rebuild to the fingerprints recorded when they were
// built, so the models it does rebuild record fingerprints that match the
// data they were built from.
func (c *Catalog) Pin(modelName string, l *lineage.Model) {
	if c.lineage == nil {
		c.lineage = map[string]*lineage.Model{}
	}
	c.lineage[modelName] = l
}

// fingerprintsOf gives the fingerprints of upstream model columns. A column's
// values depend on the model's rows too, so both go into what a downstream
// plan sees. Tables that are not models are identified by name.
func fingerprintsOf(models map[string]*lineage.Model) lineage.Upstream {
	return func(table, column string) string {
		l, ok := models[table]
		if !ok {
			return lineage.Hash("table", table, column)
		}
		if column == "" {
			return l.RowsFingerprint
		}
		for _, c := range l.Columns {
			if c.Name == column {
				return lineage.Hash(c.Fingerprint, l.RowsFingerprint)
			}
		}
		return lineage.Hash("missing", table, column)
	}
}

// opaqueFingerprints fingerprints a model whose plan could not be walked: from
// its SQL and everything upstream, so any change to either changes them all.
func opaqueFingerprints(l *lineage.Model, m *project.Model, upstream map[string]*lineage.Model) {
	parts := []string{"opaque", m.SQL}
	for _, dep := range m.Dependencies {
		u := upstream[dep]
		parts = append(parts, dep, u.RowsFingerprint)
		for _, c := range u.Columns {
			parts = append(parts, c.Name, c.Fingerprint)
		}
	}
	base := lineage.Hash(parts...)
	l.RowsFingerprint = lineage.Hash("rows", base)
	for i := range l.Columns {
		l.Columns[i].Fingerprint = lineage.Hash(base, l.Columns[i].Name)
	}
}

// lineageFromDependencies is the fallback when DuckDB cannot serialize a
// model's plan. A model without upstream models has no upstream columns, which
// is exact. Otherwise every column gets every column of every upstream model:
// coarse, but never missing a dependency.
func (c *Catalog) lineageFromDependencies(m *project.Model, names []string, reason string) *lineage.Model {
	var all []lineage.Ref
	for _, dep := range m.Dependencies {
		for _, col := range c.Schemas[dep] {
			all = append(all, lineage.Ref{Table: dep, Column: col.Name})
		}
	}
	l := &lineage.Model{}
	if len(all) > 0 {
		l.Approximate = "DuckDB cannot serialize the plan: " + reason
	}
	for _, name := range names {
		l.Columns = append(l.Columns, lineage.Column{Name: name, Direct: all})
	}
	return l
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
