package lineage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// col is the lineage of one column of an operator's output.
type col struct {
	direct   refSet
	indirect refSet
	fp       string // fingerprint, see fingerprint.go
}

func newCol(fp string) col { return col{refSet{}, refSet{}, fp} }

// stream is the output of an operator: its columns, and the upstream columns
// that decide which rows it contains.
type stream struct {
	cols   []col
	rows   refSet
	rowsFP string
	// order is the fingerprint of an ORDER BY directly below, which matters
	// to a LIMIT above it but not otherwise.
	order string
}

// node and expressions are decoded generically: the plan has many operator
// shapes and only a few fields matter for lineage.
type obj = map[string]any

type walker struct {
	// bindings maps an operator's table index to its output columns, for
	// BOUND_COLUMN_REF expressions, which DuckDB leaves unresolved in some places
	// (the input of table in-out functions such as UNNEST in FROM).
	bindings map[uint64][]col
	// ctes maps a CTE index to the CTE's output.
	ctes map[uint64]stream
	// upstream gives the fingerprints of the columns of scanned tables.
	upstream Upstream
}

// PlanError is returned when DuckDB could not bind or serialize the plan.
// Some operators have no serializer (table functions such as read_csv).
type PlanError struct{ Message string }

func (e *PlanError) Error() string { return "failed to plan: " + e.Message }

// unsupportedError marks a plan the walker cannot follow precisely.
type unsupportedError struct{ reason string }

func (e *unsupportedError) Error() string { return e.reason }

func unsupported(format string, args ...any) error {
	return &unsupportedError{fmt.Sprintf(format, args...)}
}

// Analyze computes the lineage of a model from the output of
// json_serialize_plan(sql, optimize := false). names are the model's output
// column names, from the shadow catalog. upstream gives the fingerprints of
// the columns of the tables the plan reads; nil hashes table and column name.
//
// An approximate Model has no fingerprints: the caller has to derive them from
// something it trusts, such as the model's SQL.
func Analyze(planJSON []byte, names []string, upstream Upstream) (*Model, error) {
	if upstream == nil {
		upstream = func(table, column string) string { return Hash("table", table, column) }
	}
	dec := json.NewDecoder(bytes.NewReader(planJSON))
	dec.UseNumber() // table indexes use the full uint64 range
	var doc obj
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("failed to decode plan: %w", err)
	}
	if isErr, _ := doc["error"].(bool); isErr {
		return nil, &PlanError{Message: fmt.Sprint(doc["error_message"])}
	}
	plans := arr(doc, "plans")
	if len(plans) != 1 {
		return nil, fmt.Errorf("expected one plan, got %d", len(plans))
	}
	root, _ := plans[0].(obj)

	w := &walker{bindings: map[uint64][]col{}, ctes: map[uint64]stream{}, upstream: upstream}
	out, err := w.node(root)
	if err == nil && len(out.cols) != len(names) {
		err = unsupported("plan returns %d columns, model has %d", len(out.cols), len(names))
	}
	if err != nil {
		var u *unsupportedError
		if !asUnsupported(err, &u) {
			return nil, err
		}
		return approximate(root, names, u.reason), nil
	}

	m := &Model{Rows: out.rows.sorted(), RowsFingerprint: out.rowsFP}
	for i, c := range out.cols {
		indirect := refSet{}
		for r := range c.indirect {
			if _, ok := out.rows[r]; !ok {
				indirect[r] = struct{}{}
			}
		}
		m.Columns = append(m.Columns, Column{
			Name: names[i], Direct: c.direct.sorted(), Indirect: indirect.sorted(), Fingerprint: c.fp,
		})
	}
	return m, nil
}

func asUnsupported(err error, target **unsupportedError) bool {
	u, ok := err.(*unsupportedError)
	if ok {
		*target = u
	}
	return ok
}

// approximate gives every column every table column the plan reads.
func approximate(root obj, names []string, reason string) *Model {
	all := refSet{}
	var collect func(v any)
	collect = func(v any) {
		switch v := v.(type) {
		case obj:
			if str(v, "type") == "LOGICAL_GET" {
				if table := tableName(v); table != "" {
					tableNames := strs(v, "names")
					for _, ci := range arr(v, "column_indexes") {
						if idx, ok := num(ci.(obj), "index"); ok && idx < uint64(len(tableNames)) {
							all[Ref{table, tableNames[idx]}] = struct{}{}
						}
					}
				}
			}
			for _, child := range v {
				collect(child)
			}
		case []any:
			for _, child := range v {
				collect(child)
			}
		}
	}
	collect(root)

	m := &Model{Approximate: reason}
	for _, name := range names {
		m.Columns = append(m.Columns, Column{Name: name, Direct: all.sorted()})
	}
	return m
}

func (w *walker) node(n obj) (stream, error) {
	if n == nil {
		return stream{}, unsupported("missing operator")
	}
	typ := str(n, "type")

	var children []stream
	// CTEs and recursive CTEs need their first child registered before the
	// second is walked.
	if typ != "LOGICAL_MATERIALIZED_CTE" && typ != "LOGICAL_RECURSIVE_CTE" {
		for _, c := range arr(n, "children") {
			s, err := w.node(asObj(c))
			if err != nil {
				return stream{}, err
			}
			children = append(children, s)
		}
	}

	out, err := w.operator(typ, n, children)
	if err != nil {
		return stream{}, err
	}
	if idx, ok := num(n, "table_index"); ok && typ != "LOGICAL_GET" {
		w.bindings[idx] = out.cols
	}
	return out, nil
}

func (w *walker) operator(typ string, n obj, children []stream) (stream, error) {
	child := func(i int) stream {
		if i < len(children) {
			return children[i]
		}
		return stream{rows: refSet{}, rowsFP: Hash("none")}
	}

	switch typ {
	case "LOGICAL_GET":
		return w.get(n, children)

	case "LOGICAL_DUMMY_SCAN":
		return stream{rows: refSet{}, rowsFP: Hash("dummy")}, nil

	case "LOGICAL_PROJECTION":
		in := child(0)
		out := stream{rows: union(in.rows), rowsFP: in.rowsFP, order: in.order}
		for _, e := range arr(n, "expressions") {
			c, err := w.expr(e, in.cols)
			if err != nil {
				return stream{}, err
			}
			out.cols = append(out.cols, c)
		}
		return out, nil

	case "LOGICAL_FILTER":
		in := child(0)
		out := stream{cols: in.cols, rows: union(in.rows)}
		for _, e := range arr(n, "expressions") {
			c, err := w.expr(e, in.cols)
			if err != nil {
				return stream{}, err
			}
			out.rows.add(c.direct)
			out.rows.add(c.indirect)
		}
		out.rowsFP = Hash("filter", in.rowsFP, w.canon(n["expressions"], in.cols))
		return project(out, nums(n, "projection_map"))

	case "LOGICAL_ORDER_BY":
		in := child(0)
		// Order does not change which rows exist, only which a LIMIT keeps.
		return stream{cols: in.cols, rows: union(in.rows), rowsFP: in.rowsFP, order: w.canon(n["orders"], in.cols)}, nil

	case "LOGICAL_LIMIT", "LOGICAL_SAMPLE":
		in := child(0)
		return stream{cols: in.cols, rows: union(in.rows), rowsFP: Hash(typ, in.rowsFP, in.order, w.params(n))}, nil

	case "LOGICAL_DISTINCT":
		in := child(0)
		out := stream{cols: in.cols, rows: union(in.rows)}
		targets := arr(n, "distinct_targets")
		if len(targets) == 0 {
			for _, c := range in.cols {
				out.rows.add(c.direct)
			}
		}
		for _, e := range targets {
			c, err := w.expr(e, in.cols)
			if err != nil {
				return stream{}, err
			}
			out.rows.add(c.direct)
		}
		out.rowsFP = Hash("distinct", in.rowsFP, colFPs(in.cols), w.canon(targets, in.cols), w.canon(n["order_by"], in.cols))
		return out, nil

	case "LOGICAL_AGGREGATE_AND_GROUP_BY":
		return w.aggregate(n, child(0))

	case "LOGICAL_WINDOW":
		in := child(0)
		out := stream{cols: append([]col{}, in.cols...), rows: union(in.rows), rowsFP: in.rowsFP}
		var windows []col
		for _, e := range arr(n, "expressions") {
			c, err := w.expr(e, in.cols)
			if err != nil {
				return stream{}, err
			}
			windows = append(windows, c)
		}
		if idx, ok := num(n, "window_index"); ok {
			w.bindings[idx] = windows
		}
		out.cols = append(out.cols, windows...)
		return out, nil

	case "LOGICAL_UNNEST":
		in := child(0)
		out := stream{cols: append([]col{}, in.cols...), rows: union(in.rows)}
		var unnests []col
		for _, e := range arr(n, "expressions") {
			c, err := w.expr(e, in.cols)
			if err != nil {
				return stream{}, err
			}
			unnests = append(unnests, c)
			out.rows.add(c.direct) // unnesting multiplies rows
		}
		out.rowsFP = Hash("unnest", in.rowsFP, colFPs(unnests))
		if idx, ok := num(n, "unnest_index"); ok {
			w.bindings[idx] = unnests
		}
		out.cols = append(out.cols, unnests...)
		return out, nil

	case "LOGICAL_COMPARISON_JOIN", "LOGICAL_ASOF_JOIN", "LOGICAL_ANY_JOIN":
		return w.join(n, child(0), child(1))

	case "LOGICAL_CROSS_PRODUCT", "LOGICAL_POSITIONAL_JOIN":
		l, r := child(0), child(1)
		return stream{cols: concat(l.cols, r.cols), rows: union(l.rows, r.rows), rowsFP: Hash(typ, l.rowsFP, r.rowsFP)}, nil

	case "LOGICAL_UNION", "LOGICAL_EXCEPT", "LOGICAL_INTERSECT":
		out, err := merge(typ, children)
		if err != nil {
			return stream{}, err
		}
		// Without ALL, which rows exist depends on the values.
		all, _ := n["setop_all"].(bool)
		parts := []string{typ, fmt.Sprint(all)}
		for _, c := range children {
			parts = append(parts, c.rowsFP)
			if !all || typ != "LOGICAL_UNION" {
				parts = append(parts, colFPs(c.cols))
			}
		}
		out.rowsFP = Hash(parts...)
		return out, nil

	case "LOGICAL_EXPRESSION_GET":
		in := child(0)
		out := stream{rows: union(in.rows)}
		rows := arr(n, "expressions")
		var fps [][]string
		for _, row := range rows {
			rowExprs, _ := row.([]any)
			for i, e := range rowExprs {
				c, err := w.expr(e, in.cols)
				if err != nil {
					return stream{}, err
				}
				if i == len(out.cols) {
					out.cols = append(out.cols, newCol(""))
					fps = append(fps, nil)
				}
				out.cols[i].direct.add(c.direct)
				out.cols[i].indirect.add(c.indirect)
				fps[i] = append(fps[i], c.fp)
			}
		}
		for i := range out.cols {
			out.cols[i].fp = Hash(append([]string{"values"}, fps[i]...)...)
		}
		out.rowsFP = Hash("values", in.rowsFP, fmt.Sprint(len(rows)))
		return out, nil

	case "LOGICAL_MATERIALIZED_CTE":
		kids := arr(n, "children")
		if len(kids) != 2 {
			return stream{}, unsupported("%s with %d children", typ, len(kids))
		}
		def, err := w.node(asObj(kids[0]))
		if err != nil {
			return stream{}, err
		}
		idx, _ := num(n, "table_index")
		w.ctes[idx] = def
		return w.node(asObj(kids[1]))

	case "LOGICAL_RECURSIVE_CTE":
		kids := arr(n, "children")
		if len(kids) != 2 {
			return stream{}, unsupported("%s with %d children", typ, len(kids))
		}
		anchor, err := w.node(asObj(kids[0]))
		if err != nil {
			return stream{}, err
		}
		idx, _ := num(n, "table_index")
		cur := anchor
		// The recursive part can move lineage between columns on every
		// iteration; walk it until the lineage stops growing.
		for i := 0; ; i++ {
			w.ctes[idx] = cur
			rec, err := w.node(asObj(kids[1]))
			if err != nil {
				return stream{}, err
			}
			next, err := merge(typ, []stream{anchor, rec, cur})
			if err != nil {
				return stream{}, err
			}
			if size(next) == size(cur) {
				// The fingerprint covers the definition, not the iterations.
				next.rowsFP = Hash(typ, anchor.rowsFP, rec.rowsFP)
				for i := range next.cols {
					next.cols[i].fp = Hash(typ, anchor.cols[i].fp, rec.cols[i].fp)
				}
				return next, nil
			}
			if i == 100 {
				return stream{}, unsupported("recursive CTE lineage does not converge")
			}
			cur = next
		}

	case "LOGICAL_CTE_REF":
		idx, _ := num(n, "cte_index")
		s, ok := w.ctes[idx]
		if !ok {
			return stream{}, unsupported("reference to unknown CTE %d", idx)
		}
		return stream{cols: s.cols, rows: union(s.rows), rowsFP: s.rowsFP}, nil
	}

	return stream{}, unsupported("unsupported operator %s", typ)
}

// get handles a scan: of a table, of a table function (read_csv), or of a
// table in-out function with an input (UNNEST in FROM).
func (w *walker) get(n obj, children []stream) (stream, error) {
	table := tableName(n)
	names := strs(n, "names")
	out := stream{rows: refSet{}}

	var input stream
	if len(children) > 1 {
		return stream{}, unsupported("LOGICAL_GET with %d inputs", len(children))
	}
	if len(children) == 1 {
		input = children[0]
		out.rows.add(input.rows)
	}

	// A table function is identified by its name and arguments.
	fn := ""
	if table == "" {
		fn = w.params(n)
	}
	if table != "" {
		out.rowsFP = Hash("table", table, w.upstream(table, ""))
	} else {
		out.rowsFP = Hash("function", fn, input.rowsFP, colFPs(input.cols))
	}

	for _, ci := range arr(n, "column_indexes") {
		idx, _ := num(asObj(ci), "index")
		c := newCol(Hash("virtual", table, fn, fmt.Sprint(idx)))
		switch {
		case table != "" && idx < uint64(len(names)):
			c.direct[Ref{table, names[idx]}] = struct{}{}
			c.fp = w.upstream(table, names[idx])
		case len(children) == 1:
			// A function of its input: each output depends on all of it.
			for _, in := range input.cols {
				c.direct.add(in.direct)
				c.indirect.add(in.indirect)
			}
			c.fp = Hash("function", fn, fmt.Sprint(idx), colFPs(input.cols))
		}
		// Otherwise a table function without input (read_csv), or a virtual
		// column such as rowid: no upstream columns.
		out.cols = append(out.cols, c)
	}

	if idx, ok := num(n, "table_index"); ok {
		w.bindings[idx] = out.cols
	}

	if ids := nums(n, "projection_ids"); len(ids) > 0 {
		var err error
		if out, err = project(out, ids); err != nil {
			return stream{}, err
		}
	}

	// Input columns passed through by an in-out function.
	for _, i := range nums(n, "projected_input") {
		if i >= uint64(len(input.cols)) {
			return stream{}, unsupported("projected input %d out of range", i)
		}
		out.cols = append(out.cols, input.cols[i])
	}
	return out, nil
}

func (w *walker) aggregate(n obj, in stream) (stream, error) {
	out := stream{rows: union(in.rows)}
	// Aggregates are computed per group: their values depend on the input
	// rows and the grouping, so both are part of their fingerprints.
	grouping := Hash("grouping", in.rowsFP, w.canon(n["groups"], in.cols), w.canon(n["grouping_sets"], nil))
	out.rowsFP = grouping

	var groups []col
	for _, e := range arr(n, "groups") {
		c, err := w.expr(e, in.cols)
		if err != nil {
			return stream{}, err
		}
		groups = append(groups, c)
		out.rows.add(c.direct) // group keys decide which rows exist
	}

	var aggs []col
	for _, e := range arr(n, "expressions") {
		c, err := w.expr(e, in.cols)
		if err != nil {
			return stream{}, err
		}
		c.fp = Hash("aggregate", c.fp, grouping)
		aggs = append(aggs, c)
	}

	// GROUPING(...) columns: which group keys are aggregated away.
	var groupings []col
	for _, g := range arr(n, "grouping_functions") {
		c := newCol(Hash("grouping_function", grouping, w.canon(g, nil)))
		list, _ := g.([]any)
		for _, v := range list {
			i, ok := toNum(v)
			if !ok || i >= uint64(len(groups)) {
				return stream{}, unsupported("grouping function index out of range")
			}
			c.indirect.add(groups[i].direct)
		}
		groupings = append(groupings, c)
	}

	if idx, ok := num(n, "group_index"); ok {
		w.bindings[idx] = groups
	}
	if idx, ok := num(n, "aggregate_index"); ok {
		w.bindings[idx] = aggs
	}
	if idx, ok := num(n, "groupings_index"); ok {
		w.bindings[idx] = groupings
	}
	out.cols = concat(groups, aggs, groupings)
	return out, nil
}

func (w *walker) join(n obj, l, r stream) (stream, error) {
	conds := refSet{}
	condFP := Hash(str(n, "join_type"), w.params(n))
	for _, c := range arr(n, "conditions") {
		cond := asObj(c)
		lc, err := w.expr(cond["left"], l.cols)
		if err != nil {
			return stream{}, err
		}
		rc, err := w.expr(cond["right"], r.cols)
		if err != nil {
			return stream{}, err
		}
		conds.add(union(lc.direct, lc.indirect, rc.direct, rc.indirect))
		condFP = Hash(condFP, lc.fp, str(cond, "comparison"), rc.fp)
	}
	if cond, ok := n["condition"]; ok && cond != nil {
		// ANY_JOIN: an arbitrary predicate over left ++ right.
		c, err := w.expr(cond, concat(l.cols, r.cols))
		if err != nil {
			return stream{}, err
		}
		conds.add(union(c.direct, c.indirect))
		condFP = Hash(condFP, c.fp)
	}
	// Any join can drop or repeat rows on either side.
	rowsFP := Hash("join", l.rowsFP, r.rowsFP, condFP)

	lp, err := project(l, nums(n, "left_projection_map"))
	if err != nil {
		return stream{}, err
	}
	rp, err := project(r, nums(n, "right_projection_map"))
	if err != nil {
		return stream{}, err
	}

	switch jt := str(n, "join_type"); jt {
	case "INNER", "OUTER":
		return stream{cols: concat(lp.cols, rp.cols), rows: union(l.rows, r.rows, conds), rowsFP: rowsFP}, nil
	case "LEFT", "SINGLE":
		// Every left row is kept: the condition and the right side's rows only
		// decide the right columns' values (SINGLE is a scalar subquery). For
		// fingerprints, a left join can still repeat left rows.
		return stream{cols: concat(lp.cols, shaped(rp.cols, union(conds, r.rows), condFP)), rows: union(l.rows), rowsFP: rowsFP}, nil
	case "RIGHT":
		return stream{cols: concat(shaped(lp.cols, union(conds, l.rows), condFP), rp.cols), rows: union(r.rows), rowsFP: rowsFP}, nil
	case "SEMI", "ANTI":
		return stream{cols: lp.cols, rows: union(l.rows, r.rows, conds), rowsFP: rowsFP}, nil
	case "RIGHT_SEMI", "RIGHT_ANTI":
		return stream{cols: rp.cols, rows: union(l.rows, r.rows, conds), rowsFP: rowsFP}, nil
	case "MARK":
		// One boolean column: whether a match exists on the right.
		mark := col{direct: union(conds, r.rows), indirect: refSet{}, fp: Hash("mark", r.rowsFP, condFP)}
		if idx, ok := num(n, "mark_index"); ok {
			w.bindings[idx] = []col{mark}
		}
		return stream{cols: concat(lp.cols, []col{mark}), rows: union(l.rows), rowsFP: l.rowsFP}, nil
	default:
		return stream{}, unsupported("unsupported join type %s", jt)
	}
}

// expr returns the lineage of an expression over the input columns in.
func (w *walker) expr(e any, in []col) (col, error) {
	out := newCol("")
	if err := w.collect(e, in, &out, false); err != nil {
		return col{}, err
	}
	// A bare column reference passes the column through unchanged, so a
	// SELECT * layer or a CTE does not change fingerprints.
	if t := str(asObj(e), "type"); t == "BOUND_REF" || t == "BOUND_COLUMN_REF" {
		out.fp = strings.TrimPrefix(w.canon(e, in), "ref:")
	} else {
		out.fp = Hash("expr", w.canon(e, in))
	}
	return out, nil
}

func (w *walker) collect(v any, in []col, out *col, indirect bool) error {
	if list, ok := v.([]any); ok {
		for _, x := range list {
			if err := w.collect(x, in, out, indirect); err != nil {
				return err
			}
		}
		return nil
	}
	e, ok := v.(obj)
	if !ok {
		return nil
	}

	add := func(c col) {
		if indirect {
			out.indirect.add(c.direct)
		} else {
			out.direct.add(c.direct)
		}
		out.indirect.add(c.indirect)
	}

	typ := str(e, "type")
	switch {
	case typ == "BOUND_REF":
		idx, _ := num(e, "index")
		if idx >= uint64(len(in)) {
			return unsupported("column reference #%d out of range (%d inputs)", idx, len(in))
		}
		add(in[idx])
		return nil

	case typ == "BOUND_COLUMN_REF":
		b := asObj(e["binding"])
		ti, _ := num(b, "table_index")
		ci, _ := num(b, "column_index")
		if depth, _ := num(e, "depth"); depth > 0 {
			return unsupported("correlated column reference")
		}
		cols, ok := w.bindings[ti]
		if !ok || ci >= uint64(len(cols)) {
			return unsupported("unresolved column binding %d.%d", ti, ci)
		}
		add(cols[ci])
		return nil

	case typ == "LAMBDA":
		// References inside the lambda body are to its parameters and
		// captures; captured columns are also passed as function arguments.
		return nil

	case typ == "BOUND_SUBQUERY" || typ == "SUBQUERY":
		return unsupported("unplanned subquery")

	case strings.HasPrefix(typ, "WINDOW_"):
		for key, x := range e {
			shapes := key == "partitions" || key == "orders" || key == "arg_orders" || key == "filters" ||
				strings.HasPrefix(key, "start") || strings.HasPrefix(key, "end")
			if err := w.collect(x, in, out, indirect || shapes); err != nil {
				return err
			}
		}
		return nil

	case typ == "BOUND_AGGREGATE":
		for key, x := range e {
			if key == "function_data" {
				continue
			}
			if err := w.collect(x, in, out, indirect || key == "filter" || key == "order_bys"); err != nil {
				return err
			}
		}
		return nil
	}

	for key, x := range e {
		// function_data holds bind data, including copies of lambda bodies.
		if key == "function_data" {
			continue
		}
		if err := w.collect(x, in, out, indirect); err != nil {
			return err
		}
	}
	return nil
}

// shaped returns copies of cols with extra indirect sources, as for the
// nullable side of an outer join.
func shaped(cols []col, by refSet, condFP string) []col {
	out := make([]col, len(cols))
	for i, c := range cols {
		out[i] = col{direct: c.direct, indirect: union(c.indirect, by), fp: Hash("outer", c.fp, condFP)}
	}
	return out
}

// project selects columns by index; an empty map keeps them all.
func project(s stream, idxs []uint64) (stream, error) {
	if len(idxs) == 0 {
		return s, nil
	}
	out := stream{rows: s.rows, rowsFP: s.rowsFP}
	for _, i := range idxs {
		if i >= uint64(len(s.cols)) {
			return stream{}, unsupported("projection map index %d out of range", i)
		}
		out.cols = append(out.cols, s.cols[i])
	}
	return out, nil
}

// merge combines streams column by column, as set operations do.
func merge(typ string, streams []stream) (stream, error) {
	out := stream{rows: refSet{}}
	for _, s := range streams {
		if len(out.cols) == 0 {
			for range s.cols {
				out.cols = append(out.cols, newCol(typ))
			}
		}
		if len(s.cols) != len(out.cols) {
			return stream{}, unsupported("set operation over %d and %d columns", len(out.cols), len(s.cols))
		}
		for i, c := range s.cols {
			out.cols[i].direct.add(c.direct)
			out.cols[i].indirect.add(c.indirect)
			out.cols[i].fp = Hash(out.cols[i].fp, c.fp)
		}
		out.rows.add(s.rows)
	}
	return out, nil
}

func size(s stream) int {
	n := len(s.rows)
	for _, c := range s.cols {
		n += len(c.direct) + len(c.indirect)
	}
	return n
}

func concat(parts ...[]col) []col {
	var out []col
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// tableName returns the qualified table a LOGICAL_GET scans, or "" for a
// table function.
func tableName(n obj) string {
	fd := asObj(n["function_data"])
	table := str(fd, "table")
	if table == "" {
		return ""
	}
	name := str(fd, "schema") + "." + table
	if catalog := str(fd, "catalog"); catalog != "" && catalog != "memory" {
		name = catalog + "." + name
	}
	return name
}

func asObj(v any) obj {
	o, _ := v.(obj)
	return o
}

func str(n obj, key string) string {
	s, _ := n[key].(string)
	return s
}

func arr(n obj, key string) []any {
	a, _ := n[key].([]any)
	return a
}

func strs(n obj, key string) []string {
	var out []string
	for _, v := range arr(n, key) {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

// invalidIndex is DuckDB's DConstants::INVALID_INDEX.
const invalidIndex = ^uint64(0)

// num returns a non-negative index field; INVALID_INDEX counts as absent.
func num(n obj, key string) (uint64, bool) {
	v, ok := toNum(n[key])
	if !ok || v == invalidIndex {
		return 0, false
	}
	return v, true
}

func nums(n obj, key string) []uint64 {
	var out []uint64
	for _, v := range arr(n, key) {
		if i, ok := toNum(v); ok {
			out = append(out, i)
		}
	}
	return out
}

func toNum(v any) (uint64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	var i uint64
	if _, err := fmt.Sscan(n.String(), &i); err != nil {
		return 0, false
	}
	return i, true
}
