# molt

**SQL transformations for people who only need DuckDB.**

molt builds a folder of plain SQL files into a DuckDB database: in dependency order, with tests, in **one transaction**. Either every model and every test succeeds, or the database is exactly as it was.

## Why molt exists

DuckDB made analytics simple: one engine, one file, no server. The tools around it did not. dbt and SQLMesh are built to run on many warehouses, so they put a layer between you and the engine: Jinja or macros, a dialect translator, a project config, state to keep in sync, and a Python environment to install and upgrade. That layer is what you pay for in complexity, and if DuckDB is all you have, it buys you nothing.

molt removes the layer. It assumes DuckDB and nothing else, and lets DuckDB do every job it can already do.

## What makes it different

- **All or nothing.** Every run is one transaction: models, tests and bookkeeping together. A failing model or test rolls everything back, so readers never see half a rebuild or data that failed its tests.
- **DuckDB is the compiler.** Dependencies come from DuckDB's own parser, and `molt check` validates every model with DuckDB's own binder, against the real schemas of its upstream models, without reading any data. Nothing re-implements or rewrites DuckDB's SQL, so new DuckDB syntax and functions work the day you upgrade, and what you write is what runs.
- **Plain SQL in, plain SQL out.** Models are `SELECT`s you can paste into any DuckDB client. `molt compile` prints the whole build as one SQL script that runs without molt. There is nothing to be locked into.
- **Configuration is SQL.** Settings, secrets, variables and macros live in `init.sql`, using DuckDB's own `SET`, `CREATE SECRET`, `SET VARIABLE` and `CREATE MACRO`.
- **No state outside the data.** What was built and when is recorded in the database itself, in the same transaction as the data, so it can never disagree with it.
- **One binary.** No Python runtime, no server, no metadata database.

## Who it is for

Anyone whose analytics runs on DuckDB and who wants structured, tested, repeatable SQL pipelines without a platform around them: on a laptop, a single server, or in a scheduled job. I built it for my own DuckDB projects and share it because sharing costs nothing.

If you need to run the same models on Snowflake, BigQuery or Postgres, a distributed scheduler, or role-based governance, use dbt or SQLMesh. molt is deliberately DuckDB only; that is what lets it stay this small.

---

## The idea

molt owns only what DuckDB cannot know by itself: **which files exist, and the order to run them in.** DuckDB already has everything else:

| You need | DuckDB already has |
|---|---|
| Configuration, secrets | `SET`, `INSTALL`, `LOAD`, `CREATE SECRET` |
| Variables | `SET VARIABLE`, `getvariable()` |
| Reusable logic | `CREATE MACRO` |
| Dependencies | its own parser (`json_serialize_sql`) |
| Validation | its own binder |
| Atomic publishing | transactions |

So a molt project is:

```text
init.sql                     settings, secrets, variables and macros, run first in every session
models/<schema>/<name>.sql   one SELECT per model; models/<name>.sql is schema main
tests/*.sql                  passes when the query returns zero rows
```

The folder is the schema: `models/shop/orders.sql` becomes `shop.orders`. Dependencies are found by DuckDB's parser, so you never declare them.

## How a run works

`molt run` compiles the project into one SQL script and executes it:

```sql
<init.sql>
BEGIN TRANSACTION;
CREATE OR REPLACE TABLE shop.orders AS <orders.sql>;
...                                    -- every model, in dependency order
SELECT error('test ... failed') ...;   -- every test: rows abort the transaction
COMMIT;
```

Every model is rebuilt on every run. DuckDB is fast enough that this is the simplest correct choice: rebuilding is usually bound by reading the sources, not by the transforms.

`molt compile` prints that script. It is plain SQL, so you can read exactly what will run, or run it without molt:

```bash
molt compile > build.sql
duckdb -bail molt.duckdb < build.sql
```

## Models

A model is a `SELECT`. Its kind defaults to `table`; set it with a directive in the leading comments:

```sql
-- One row per order line.
-- @kind: view
SELECT ...
```

| Kind | Statement |
|---|---|
| `table` (default) | `CREATE OR REPLACE TABLE` |
| `view` | `CREATE OR REPLACE VIEW` |

Variables only live in a session, so a view that uses `getvariable()` returns `NULL` for later readers. Use variables in table models; macros are stored in the database and are fine in views.

`--var KEY=VALUE` sets a `VARCHAR` variable after `init.sql`, so it overrides it per environment. Cast it where you need another type: `getvariable('limit')::INTEGER`.

## Bookkeeping

The `_molt` schema in the same database records what was built:

- `_molt.models`: name, kind, a hash of the model file, when it was built and its row count.
- `_molt.runs`: every successful run.

It is written in the run's transaction, so it can never disagree with the data. Nothing is skipped based on it.

## Example

The repository is a molt project: a small, synthetic shop in `models/shop/`, with data in `example_flow/`.

```bash
go run ./cmd/molt run
```

## Commands

```bash
molt run                     # build every model and run every test
molt run orders+             # build orders and everything downstream, with their tests
molt compile [model+]        # print the script that run would execute
molt test                    # run tests against the existing database
molt check                   # bind every model against real schemas, without building
molt schema [model]          # output columns and types
molt explain <model>         # logical plan
molt graph [model]           # dependency tree

--db path                    # database file (default molt.duckdb)
--var KEY=VALUE              # set a variable after init.sql
```

`molt check` uses a *shadow catalog*: every model as an empty table with its real schema, created in dependency order. It reports misspelled columns, unknown tables and type errors with line numbers from the model file.

---

## Roadmap

- [x] Dependencies from DuckDB's parser
- [x] Shadow catalog (`check`, `schema`, `explain`)
- [x] One transaction per run, with tests
- [x] `compile` to a plain SQL script
- [x] Bookkeeping in `_molt`
- [ ] Incremental models (append or merge new rows; `--full-refresh` and the model hash are already in place for them)
- [ ] Column-level lineage on the shadow catalog
- [ ] Publishing marts (e.g. `COPY` to Parquet on S3)
