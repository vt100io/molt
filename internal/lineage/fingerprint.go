package lineage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

// Fingerprints identify what a column computes, not what it is called.
//
// A column's fingerprint hashes its expression with every column reference
// replaced by the fingerprint of the column it refers to, down to the tables
// the plan reads. A table column's fingerprint is supplied by the caller: for
// a model, it is the fingerprint of that model's column combined with the
// model's rows fingerprint. So a fingerprint changes exactly when the
// definition of the column changes, in this model or in any model upstream,
// and stays the same for renames, reformatting, comments and table indexes.
//
// A stream's rows fingerprint hashes everything that decides which rows exist:
// scans, filters, joins and their conditions, group keys, DISTINCT, UNNEST,
// LIMIT. When it changes, every column of the model is considered changed.

// Upstream returns the fingerprint of a column of a table the plan reads, or
// of the table's rows when column is "".
type Upstream func(table, column string) string

// Hash combines strings into a fingerprint.
func Hash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// volatileKeys differ between plans of equivalent SQL: names chosen by the
// user or positions chosen by the binder.
var volatileKeys = map[string]bool{
	"alias": true, "query_location": true, "query_location_length": true,
	"table_index": true, "group_index": true, "aggregate_index": true, "groupings_index": true,
	"window_index": true, "unnest_index": true, "mark_index": true, "cte_index": true,
}

// canon renders an expression canonically, with column references replaced by
// the fingerprints of the columns they refer to.
func (w *walker) canon(v any, in []col) string {
	var b strings.Builder
	w.writeCanon(&b, v, in, true)
	return b.String()
}

func (w *walker) writeCanon(b *strings.Builder, v any, in []col, subst bool) {
	switch v := v.(type) {
	case []any:
		b.WriteByte('[')
		for i, x := range v {
			if i > 0 {
				b.WriteByte(',')
			}
			w.writeCanon(b, x, in, subst)
		}
		b.WriteByte(']')
	case obj:
		if subst {
			switch str(v, "type") {
			case "BOUND_REF":
				idx, _ := num(v, "index")
				b.WriteString("ref:")
				if idx < uint64(len(in)) {
					b.WriteString(in[idx].fp)
				}
				return
			case "BOUND_COLUMN_REF":
				bd := asObj(v["binding"])
				ti, _ := num(bd, "table_index")
				ci, _ := num(bd, "column_index")
				b.WriteString("ref:")
				if cols, ok := w.bindings[ti]; ok && ci < uint64(len(cols)) {
					b.WriteString(cols[ci].fp)
				}
				return
			case "LAMBDA":
				// References in a lambda body are to its parameters.
				subst = false
			}
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			if !volatileKeys[k] {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for _, k := range keys {
			b.WriteString(k)
			b.WriteByte(':')
			// function_data holds bind data, including copies of lambda bodies.
			w.writeCanon(b, v[k], in, subst && k != "function_data")
			b.WriteByte(';')
		}
		b.WriteByte('}')
	default:
		raw, _ := json.Marshal(v)
		b.Write(raw)
	}
}

// params renders an operator's own fields canonically, without its children
// and expressions, which are fingerprinted separately.
func (w *walker) params(n obj) string {
	rest := obj{}
	for k, v := range n {
		if k != "children" && k != "expressions" {
			rest[k] = v
		}
	}
	return w.canon(rest, nil)
}

func colFPs(cols []col) string {
	fps := make([]string, len(cols))
	for i, c := range cols {
		fps[i] = c.fp
	}
	return strings.Join(fps, ",")
}
