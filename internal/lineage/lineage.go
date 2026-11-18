// Package lineage derives column-level lineage from DuckDB's bound logical plan.
//
// molt does not parse SQL. DuckDB binds the model against the shadow catalog
// and serializes the unoptimized plan with json_serialize_plan; this package
// walks that tree. Every output column is traced down through projections,
// joins, aggregates, windows and CTEs to the LOGICAL_GET operators that read
// the upstream tables, giving the upstream (model, column) pairs it comes from.
//
// Sources come in two kinds:
//
//   - direct: the value flows into the column (sum(amount) ← amount).
//   - indirect: the column decides which rows or groups exist, or how a
//     window is partitioned and ordered, without its value flowing through
//     (WHERE status <> 'cancelled', JOIN ON, GROUP BY, PARTITION BY).
//
// Indirect sources that affect every row (filters, join conditions, group
// keys) are reported once per model as Rows; indirect sources specific to one
// column (window partitions and orders, aggregate FILTER clauses) are reported
// on the column.
//
// When the walk meets an operator it does not understand, it does not guess:
// the model is marked Approximate and every output column gets every column
// the plan reads as a source. Coarse, but never missing a dependency.
//
// The serialized plan is not a stable DuckDB API. Lineage is only computed on
// DuckDB versions it has been verified against (see SupportedVersions); a
// DuckDB upgrade has to come with a run of this package's tests.
package lineage

import (
	"fmt"
	"sort"
	"strings"
)

// SupportedVersions lists the DuckDB version prefixes the plan walker is
// verified against. Extend it only after the tests pass on the new version.
var SupportedVersions = []string{"v2.0."}

// CheckVersion returns an error unless version (as returned by DuckDB's
// version()) is one the walker is verified against.
func CheckVersion(version string) error {
	for _, prefix := range SupportedVersions {
		if strings.HasPrefix(version, prefix) {
			return nil
		}
	}
	return fmt.Errorf("column lineage is verified against DuckDB %s, not %s",
		strings.Join(SupportedVersions, "x, ")+"x", version)
}

// Ref is a column of an upstream table, normally a model.
type Ref struct {
	// Table is schema-qualified (shop.orders), prefixed with the catalog when
	// it is not the default in-memory catalog.
	Table  string
	Column string
}

func (r Ref) String() string { return r.Table + "." + r.Column }

// Column is the lineage of one output column.
type Column struct {
	Name string
	// Direct lists the upstream columns whose values flow into this column.
	Direct []Ref
	// Indirect lists upstream columns that shape this column only (window
	// partitions and orders, aggregate filters), beyond the model's Rows.
	Indirect []Ref
	// Fingerprint identifies what the column computes, through every model
	// upstream; see fingerprint.go.
	Fingerprint string
}

// Model is the lineage of one model: one hop, from its output columns to the
// columns of the tables it reads.
type Model struct {
	Columns []Column
	// Rows lists upstream columns that decide which rows the model returns:
	// filters, join conditions, group keys, DISTINCT, UNNEST.
	Rows []Ref
	// Approximate is set when the plan contained something the walker does not
	// understand; it holds the reason. Every column then lists every column the
	// plan reads as Direct.
	Approximate string
	// RowsFingerprint identifies what decides the model's rows.
	RowsFingerprint string
}

// refSet is a set of upstream columns.
type refSet map[Ref]struct{}

func (s refSet) add(other refSet) {
	for r := range other {
		s[r] = struct{}{}
	}
}

func (s refSet) sorted() []Ref {
	out := make([]Ref, 0, len(s))
	for r := range s {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Table != out[j].Table {
			return out[i].Table < out[j].Table
		}
		return out[i].Column < out[j].Column
	})
	return out
}

func union(sets ...refSet) refSet {
	out := refSet{}
	for _, s := range sets {
		out.add(s)
	}
	return out
}
