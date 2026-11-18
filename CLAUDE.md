# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Build the binary
go build ./cmd/molt/...

# Run all tests
go test ./...

# Run tests for a specific package
go test ./internal/build/...

# Run a specific test
go test ./internal/duckdb/... -run TestAnalyzeDependencies

# Run molt on the example project (init.sql sets DATA_DIR=example_flow)
go run ./cmd/molt/... run
go run ./cmd/molt/... run orders+
go run ./cmd/molt/... compile
go run ./cmd/molt/... plan
go run ./cmd/molt/... test
go run ./cmd/molt/... check
go run ./cmd/molt/... schema [model]
go run ./cmd/molt/... explain <model>
go run ./cmd/molt/... lineage <model> [column]
go run ./cmd/molt/... graph

# Override a variable from init.sql, e.g. a different data directory
go run ./cmd/molt/... run --var DATA_DIR=/path/to/feeds
```

Go version is managed via `mise` (`mise.toml` pins Go 1.26).

## Architecture

molt builds a folder of SQL files into a DuckDB database, in dependency order and in one transaction. The guiding principle: **molt owns only what DuckDB cannot know (which files exist, and their order). Everything else is DuckDB**: configuration, secrets, variables and macros are SQL in `init.sql`; validation is DuckDB's binder; atomicity is a transaction.

### Project

- `init.sql`: run first in every session (run, check, test). Settings, `CREATE SECRET`, `SET VARIABLE`, `CREATE OR REPLACE MACRO`.
- `models/<schema>/<name>.sql` → `<schema>.<name>`; `models/<name>.sql` → `main.<name>`. Deeper nesting is an error.
- `tests/**/*.sql`: passes when the query returns zero rows.

A model is a `SELECT`. Its kind (`table` default, or `view`) is a directive in the leading comments: `-- @kind: view`. Only `-- @...` lines are directives; unknown directives are an error, ordinary comments are ignored.

### Run pipeline

1. **Project loading** (`internal/project/project.go`): reads init.sql, models and tests, parses headers, hashes each model file, trims trailing semicolons (leading lines are kept so DuckDB error line numbers match the file), and analyzes dependencies.
2. **Dependency analysis** (`internal/duckdb/analyzer.go`): serializes SQL to an AST with `json_serialize_sql` and collects `BASE_TABLE` references as `schema.table`. Unqualified names resolve to `main`, as in DuckDB; only unqualified names can be shadowed by a CTE in scope. Results are sorted.
3. **Selection** (`internal/build/dag.go`): deterministic topological sort; `Select` handles `model` and `model+` (model and everything downstream).
4. **Compilation** (`internal/compile/compile.go`): turns the selection into a `Script` of statements: init.sql, `--var` statements, `BEGIN`, `_molt` DDL, one statement per model (`CREATE OR REPLACE TABLE|VIEW ... AS <sql>`, plus its `_molt.models`, `_molt.columns` and `_molt.column_lineage` rows, from a shadow catalog built first), one per test (`SELECT error(...)` when it returns rows), the `_molt.runs` row, `COMMIT`. When a model changes kind, the existing object (from `ExistingObjects`) is dropped first, since `CREATE OR REPLACE` cannot replace a view with a table or the reverse.
5. **Execution** (`internal/build/execute.go`): runs the statements one by one on a single connection; on the first error it rolls back and returns the error labelled with the model or test.

`molt compile` prints the same script, which runs unchanged in the DuckDB CLI.

### Shadow catalog

`internal/shadow/catalog.go` builds an in-memory DuckDB where every model exists with its real output schema but no data: after init.sql, every model, views included, is created with `CREATE TABLE ... AS SELECT * FROM (<sql>) LIMIT 0`, in topological order. Views are tables here on purpose: the binder would otherwise inline a view into downstream plans and lineage would skip it. Used by `check`, `schema`, `explain` and `lineage`. A model that fails to bind is recorded in `Catalog.Errors`; its downstream models as `upstream model X failed`.

### Column lineage

`internal/lineage` walks DuckDB's bound plan, `json_serialize_plan(sql, optimize := false)` against the shadow catalog (`Catalog.Lineage`). Expressions are mostly `BOUND_REF` (a position in the child's output), so each operator defines how its output columns are composed (join: left ++ right, aggregate: groups ++ aggregates ++ grouping functions, window/unnest: child ++ new columns, CTE_REF: the CTE's output); `BOUND_COLUMN_REF` (table_index, column_index) occurs in table in-out function inputs and is resolved through a table_index map. Lineage is one hop: output column → upstream model columns, `direct` (value flows in) or `indirect` (filters, join conditions, group keys, window partitions). `Model.Rows` holds indirect sources that decide which rows exist; per-column `Indirect` holds the rest (outer-join conditions, window partitions/orders, aggregate FILTER).

Unknown operators make the model `Approximate` (every column depends on every column read) rather than failing. Plans DuckDB cannot serialize (`read_csv` and other table functions) fall back to the model's dependencies, which is exact for source models. The plan JSON is not a stable DuckDB API: `lineage.SupportedVersions` lists verified DuckDB versions and `TestDuckDBVersionIsSupported` fails on an upgrade until the tests are rerun and the list extended.

**Fingerprints** (`internal/lineage/fingerprint.go`): each column's fingerprint hashes its expression with column references replaced by the referenced column's fingerprint; a scanned model column contributes that model's column fingerprint combined with its rows fingerprint. So fingerprints are Merkle-like across models: they change exactly when a column's definition changes here or anywhere upstream, and not for renames, formatting, comments or table indexes. A bare reference passes the fingerprint through unchanged (so `SELECT *` layers and CTEs are neutral). The rows fingerprint covers what decides which rows exist (filters, joins, group keys, DISTINCT, UNNEST, LIMIT with its ORDER BY). Models whose plan cannot be walked get fingerprints from their SQL and all upstream fingerprints.

### Plan

`internal/plan` compares `_molt` (what was built: file hash, column types and fingerprints, rows fingerprint) with a fresh shadow catalog. Model statuses: added, removed, modified (file changed), affected (unchanged file, changed fingerprints upstream), unchanged, failed (does not bind). Breaking = an existing column changes or disappears, or the rows change; only added columns = non-breaking. Affected columns list the changed upstream columns they depend on (`Because`, from lineage). `molt run` with a selection pins unselected models to their recorded fingerprints (`Catalog.Pin`), so a partial build records fingerprints matching the data it was built from.

### Key Design Decisions

- **Rebuild everything, every run, in one transaction.** No skipping. A partial build or a build whose tests fail can never be visible.
- **Bookkeeping in the same database**: `_molt.models` (kind, file hash, built_at, row_count, sql, rows_fingerprint, lineage), `_molt.columns` (type, fingerprint), `_molt.column_lineage` and `_molt.runs`, written in the run's transaction. New columns are added with `ALTER TABLE ... ADD COLUMN IF NOT EXISTS`, so older databases upgrade on the next run. The hash records which SQL produced a table; nothing is skipped based on it. It exists for incremental models, which will need a full refresh when their SQL changes.
- **Materialization is a template around a SELECT.** Adding a kind (e.g. incremental) means a new case in `compile.modelSQL`, not a new pipeline. `--full-refresh` is already plumbed through for it.
- **Variables are session state.** A view using `getvariable()` returns NULL for later readers; use variables in table models. `--var` sets VARCHAR values.
- **No SQL parsing in molt**: dependencies come from DuckDB's own parser, validation from its binder.

### Package Layout

```
cmd/molt/main.go          — CLI (cobra): run, compile, test, check, schema, explain, graph (lineage.go, plan.go: lineage, plan)
cmd/molt/lineage.go       — lineage command
cmd/molt/plan.go          — plan command
internal/
  project/project.go     — Project, Model, Test; loading, headers, Resolve
  duckdb/analyzer.go     — dependency extraction (parser AST), quoting, --var statements
  build/
    dag.go               — topological sort and selection (model, model+)
    execute.go           — runs a compiled script on one connection, rolls back on error
  compile/compile.go     — project → SQL script; _molt bookkeeping; existing objects
  shadow/catalog.go      — shadow catalog for check/schema/explain/lineage
  lineage/               — column lineage and fingerprints from DuckDB's bound plan
  plan/                  — compare _molt with the project: what changes, breaking or not
init.sql                 — example project setup (DATA_DIR)
models/, tests/          — example project
molt.duckdb               — build output (gitignored)
```
